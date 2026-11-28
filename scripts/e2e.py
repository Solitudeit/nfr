#!/usr/bin/env python3
"""End-to-end smoke test for a single NFR node (Vue 3 SPA + JSON API).

Starts a temporary server, verifies SPA + JSON panel auth, joins an agent,
creates HTTP / TCP / UDP tunnels, drives real traffic through them, tests
one-click end-to-end probe and probe-all, verifies tunnel-first onboarding,
toggling, REST API / metrics / certs / SSE, and server restart resilience.

    python3 scripts/e2e.py                 # builds its own temp dir
    python3 scripts/e2e.py --keep          # keep the temp dir for debugging
"""

import argparse
import http.client
import http.cookiejar
import json
import os
import re
import shlex
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NFRS_BIN = os.environ.get("NFRS_BIN", os.path.join(ROOT, "bin", "nfrs"))
NFRC_BIN = os.environ.get("NFRC_BIN", os.path.join(ROOT, "bin", "nfrc"))

failures = []


def check(name, cond, detail=""):
    if cond:
        print("  ok   " + name)
        return True
    print("  FAIL " + name + ("  (" + detail + ")" if detail else ""))
    failures.append(name)
    return False


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def free_udp_port():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def wait_port(port, timeout=15.0):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.05)
    return False


class Panel:
    def __init__(self, base_url):
        self.base = base_url
        self.jar = http.cookiejar.CookieJar()
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self.ctx = ctx
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(self.jar),
        )
        self.csrf = ""

    def request_raw(self, path, method="GET", body=None, headers=None):
        hdrs = dict(headers or {})
        req = urllib.request.Request(self.base + path, data=body, headers=hdrs, method=method)
        try:
            with self.opener.open(req, timeout=15) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def json(self, path, method="GET", payload=None, headers=None):
        hdrs = dict(headers or {})
        data = None
        if payload is not None:
            data = json.dumps(payload).encode("utf-8")
            hdrs.setdefault("Content-Type", "application/json")
        if method in ("POST", "PATCH", "PUT", "DELETE") and self.csrf:
            hdrs.setdefault("X-CSRF", self.csrf)
        st, text = self.request_raw(path, method=method, body=data, headers=hdrs)
        try:
            parsed = json.loads(text) if text.strip() else {}
        except ValueError:
            parsed = {"_raw": text}
        return st, parsed

    def login(self, password):
        st, data = self.json("/nfr/api/auth/login", method="POST", payload={"password": password})
        if st == 200 and data.get("csrf"):
            self.csrf = data["csrf"]
        return st, data


class LocalService(BaseHTTPRequestHandler):
    payload = b"hello from the private network\n"

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(self.payload)))
        self.end_headers()
        self.wfile.write(self.payload)

    def log_message(self, *args):
        pass


class UDPEchoServer:
    def __init__(self, port):
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.bind(("127.0.0.1", port))
        self.sock.settimeout(0.5)
        self.stop_ev = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def start(self):
        self.thread.start()

    def _run(self):
        while not self.stop_ev.is_set():
            try:
                data, addr = self.sock.recvfrom(4096)
                self.sock.sendto(b"echo:" + data, addr)
            except socket.timeout:
                continue
            except OSError:
                break

    def close(self):
        self.stop_ev.set()
        try:
            self.sock.close()
        except OSError:
            pass


def http_get(port, host_header, timeout=5):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    conn.request("GET", "/", headers={"Host": host_header})
    resp = conn.getresponse()
    body = resp.read()
    conn.close()
    return resp.status, body


def tcp_probe(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=3) as s:
            s.sendall(b"GET / HTTP/1.1\r\nHost: raw\r\nConnection: close\r\n\r\n")
            data = b""
            while True:
                chunk = s.recv(4096)
                if not chunk:
                    break
                data += chunk
        return LocalService.payload in data
    except OSError:
        return False


def udp_probe(port):
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.settimeout(1.5)
            s.sendto(b"ping-udp", ("127.0.0.1", port))
            data, _ = s.recvfrom(4096)
            return data == b"echo:ping-udp"
    except OSError:
        return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--nfrs", default=NFRS_BIN, help="the control node binary")
    ap.add_argument("--nfrc", default=NFRC_BIN, help="the client binary")
    args = ap.parse_args()

    nfrs = os.path.abspath(args.nfrs)
    nfrc = os.path.abspath(args.nfrc)
    missing = [p for p in (nfrs, nfrc) if not os.path.exists(p)]
    if missing:
        print(
            "missing binary: "
            + ", ".join(missing)
            + " (run: make build, or: go build -o bin/nfrs ./cmd/nfrs && go build -o bin/nfrc ./cmd/nfrc)"
        )
        return 2

    tmp = tempfile.mkdtemp(prefix="nfr-e2e-")
    procs = []
    httpd = None
    udp_srv = None
    print("work dir " + tmp)

    def cleanup():
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(5)
                except subprocess.TimeoutExpired:
                    p.kill()
        if httpd is not None:
            httpd.shutdown()
            httpd.server_close()
        if udp_srv is not None:
            udp_srv.close()
        server_log_path = os.path.join(tmp, "server.log")
        if args.keep or failures:
            print("logs kept in " + tmp)
            try:
                print(open(server_log_path, errors="replace").read()[-3000:])
            except OSError:
                pass
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        control = free_port()
        ingress = free_port()
        local_port = free_port()
        udp_local_port = free_udp_port()
        dead_port = free_port()

        data_dir = os.path.join(tmp, "data")
        cfg = os.path.join(tmp, "server.toml")
        with open(cfg, "w") as f:
            f.write(
                'control_listen = "127.0.0.1:%d"\n'
                'http_listen = "127.0.0.1:%d"\n'
                'https_listen = "off"\n'
                'data_dir = "%s"\n'
                'port_range = [20000, 29999]\n'
                'log_level = "debug"\n' % (control, ingress, data_dir)
            )

        server_log = open(os.path.join(tmp, "server.log"), "wb")
        srv_env = dict(os.environ, NFR_DISABLE_UPDATE_CHECK="1", NFR_STORAGE="file")
        server = subprocess.Popen(
            [nfrs, "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT, env=srv_env
        )
        procs.append(server)
        if not wait_port(control):
            print("server never listened")
            return 1

        m = None
        deadline = time.time() + 5.0
        while time.time() < deadline:
            text = open(os.path.join(tmp, "server.log"), errors="replace").read()
            m = re.search(r"密码\s+(\S+)", text)
            if m:
                break
            time.sleep(0.1)
        if not m:
            print("could not find the first-run password in the banner")
            return 1
        password = m.group(1)

        panel = Panel("https://127.0.0.1:%d" % control)

        # 1. SPA static serving + unauthenticated auth/me + protected API check.
        st, spa_html = panel.request_raw("/nfr/")
        check("SPA index served", st == 200 and 'id="app"' in spa_html, "status %d" % st)

        st, me = panel.json("/nfr/api/auth/me")
        check("auth/me unauthenticated", st == 200 and me.get("authenticated") is False, str(me))

        st, _ = panel.json("/nfr/api/dashboard")
        check("dashboard requires auth", st == 401, "status %d" % st)

        # 2. Panel JSON login + dashboard snapshot + configs payload.
        st, login_data = panel.login(password)
        check(
            "panel login",
            st == 200 and login_data.get("authenticated") is True and bool(panel.csrf),
            "status %d data=%s" % (st, login_data),
        )

        st, dash = panel.json("/nfr/api/dashboard")
        check(
            "dashboard snapshot",
            st == 200 and "info" in dash and "nodes" in dash and "tunnels" in dash and "rates" in dash,
            "status %d" % st,
        )

        st, cfgs = panel.json("/nfr/api/configs")
        tmpl_toml = (cfgs.get("template") or {}).get("toml", "")
        check(
            "configs payload",
            st == 200 and "server_addr" in tmpl_toml and "control_listen" in cfgs.get("server_toml", ""),
            "status %d" % st,
        )

        # 3. Join first agent via join-code API.
        st, jc = panel.json("/nfr/api/nodes/join-codes", method="POST", payload={"name": "testnode"})
        code = jc.get("code", "")
        check("join code issued", st == 201 and bool(code), "status %d body=%s" % (st, jc))
        if not code:
            return 1

        agent_cfg = os.path.join(tmp, "agent.toml")
        p = subprocess.run(
            [nfrc, "join", "--no-start", "-c", agent_cfg, "--name", "testnode", "127.0.0.1:%d" % control, code],
            capture_output=True,
            text=True,
        )
        check("agent joined", p.returncode == 0, (p.stderr or p.stdout).strip()[-300:])

        agent_log = open(os.path.join(tmp, "agent.log"), "wb")
        agent = subprocess.Popen(
            [nfrc, "-c", agent_cfg], stdout=agent_log, stderr=subprocess.STDOUT
        )
        procs.append(agent)

        node = None
        end = time.time() + 20
        while time.time() < end:
            _, state = panel.json("/nfr/api/dashboard")
            online = [n for n in state.get("nodes", []) if n.get("online")]
            if online:
                node = online[0]
                break
            time.sleep(0.3)
        check("agent online", node is not None)
        if not node:
            return 1

        httpd = ThreadingHTTPServer(("127.0.0.1", local_port), LocalService)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        udp_srv = UDPEchoServer(udp_local_port)
        udp_srv.start()

        # 4. Create HTTP tunnel (host-based) and verify proxying.
        st, created_http = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "web",
                "node_id": node["id"],
                "type": "http",
                "local": "127.0.0.1:%d" % local_port,
                "hosts": ["test.local"],
                "enabled": True,
            },
        )
        web_tun = created_http.get("tunnel") or {}
        check("create http tunnel", st == 201 and bool(web_tun.get("id")), "status %d %s" % (st, created_http))

        ok = False
        last_st = 0
        for _ in range(40):
            try:
                last_st, body = http_get(ingress, "test.local")
                if last_st == 200 and LocalService.payload in body:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check("http tunnel proxies", ok, "status %d" % last_st)

        # 5. Allocate TCP port, create TCP tunnel, verify proxying.
        st, alloc = panel.json("/nfr/api/ports/allocate", method="POST", payload={"network": "tcp"})
        check("port allocated", st == 200 and bool(alloc.get("port")), str(alloc))

        st, created_tcp = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "raw",
                "node_id": node["id"],
                "type": "tcp",
                "local": "127.0.0.1:%d" % local_port,
                "remote_port": alloc.get("port", 0),
                "lease_id": alloc.get("lease_id", ""),
                "enabled": True,
            },
        )
        tcp_tun = created_tcp.get("tunnel") or {}
        check("create tcp tunnel", st == 201 and bool(tcp_tun.get("id")), "status %d %s" % (st, created_tcp))

        ok = False
        port = alloc.get("port")
        if port:
            end = time.time() + 10
            while time.time() < end:
                if tcp_probe(port):
                    ok = True
                    break
                time.sleep(0.2)
        check("tcp tunnel proxies", ok, "port %s" % port)

        # 6. Allocate UDP port, create UDP tunnel, verify UDP traffic.
        st, ualloc = panel.json("/nfr/api/ports/allocate", method="POST", payload={"network": "udp"})
        check("udp port allocated", st == 200 and bool(ualloc.get("port")), str(ualloc))

        st, created_udp = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "udpecho",
                "node_id": node["id"],
                "type": "udp",
                "local": "127.0.0.1:%d" % udp_local_port,
                "remote_port": ualloc.get("port", 0),
                "lease_id": ualloc.get("lease_id", ""),
                "enabled": True,
            },
        )
        udp_tun = created_udp.get("tunnel") or {}
        check("create udp tunnel", st == 201 and bool(udp_tun.get("id")), "status %d %s" % (st, created_udp))

        uport = ualloc.get("port")
        ok = False
        if uport:
            end = time.time() + 10
            while time.time() < end:
                if udp_probe(uport):
                    ok = True
                    break
                time.sleep(0.2)
        check("udp tunnel proxies", ok, "port %s" % uport)

        # 7. One-click end-to-end probe: live TCP, live HTTP, dead TCP, UDP unsupported, probe-all.
        st, p_tcp = panel.json("/nfr/api/tunnels/%s/probe" % tcp_tun["id"], method="POST")
        check(
            "probe live tcp tunnel",
            st == 200 and p_tcp.get("status") == "ok" and isinstance(p_tcp.get("rtt_ms"), int),
            "status %d body=%s" % (st, p_tcp),
        )

        st, p_http = panel.json("/nfr/api/tunnels/%s/probe" % web_tun["id"], method="POST")
        check(
            "probe live http tunnel",
            st == 200 and p_http.get("status") == "ok",
            "status %d body=%s" % (st, p_http),
        )

        st, p_udp = panel.json("/nfr/api/tunnels/%s/probe" % udp_tun["id"], method="POST")
        check(
            "probe udp tunnel honest unsupported",
            st == 200 and p_udp.get("status") == "unsupported",
            "status %d body=%s" % (st, p_udp),
        )

        st, dalloc = panel.json("/nfr/api/ports/allocate", method="POST", payload={"network": "tcp"})
        st, created_dead = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "deadsvc",
                "node_id": node["id"],
                "type": "tcp",
                "local": "127.0.0.1:%d" % dead_port,
                "remote_port": dalloc.get("port", 0),
                "lease_id": dalloc.get("lease_id", ""),
                "enabled": True,
            },
        )
        dead_tun = created_dead.get("tunnel") or {}
        st, p_dead = panel.json("/nfr/api/tunnels/%s/probe" % dead_tun["id"], method="POST")
        check(
            "probe dead tcp tunnel fails",
            st == 200 and p_dead.get("status") == "failed",
            "status %d body=%s" % (st, p_dead),
        )

        st, pall = panel.json("/nfr/api/tunnels/probe-all", method="POST")
        by_id = {r["tunnel_id"]: r["status"] for r in pall.get("results", [])}
        check(
            "probe-all aggregates statuses",
            st == 200
            and by_id.get(tcp_tun["id"]) == "ok"
            and by_id.get(web_tun["id"]) == "ok"
            and by_id.get(dead_tun["id"]) == "failed"
            and by_id.get(udp_tun["id"]) == "unsupported",
            "status %d map=%s" % (st, by_id),
        )

        st, _ = panel.json("/nfr/api/tunnels/%s" % dead_tun["id"], method="DELETE")
        check("delete dead tunnel", st == 200, "status %d" % st)

        # 8. Tunnel-first awaiting flow (node_id = "new").
        st, created_await = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "await-tcp",
                "type": "tcp",
                "node_id": "new",
                "local": "127.0.0.1:%d" % local_port,
                "enabled": True,
            },
        )
        await_tun = created_await.get("tunnel") or {}
        tid = await_tun.get("id", "")
        command_text = created_await.get("join_command", "")
        code2 = created_await.get("join_code", "")
        check(
            "awaiting tunnel created",
            st == 201 and await_tun.get("awaiting") is True and command_text.startswith("nfrc join ") and bool(code2),
            "status %d resp=%s" % (st, created_await),
        )
        if not tid or not code2:
            return 1

        st, p_await = panel.json("/nfr/api/tunnels/%s/probe" % tid, method="POST")
        check("probe awaiting tunnel", st == 200 and p_await.get("status") == "awaiting", str(p_await))

        join_argv = shlex.split(command_text)
        agent2_cfg = os.path.join(tmp, "agent2.toml")
        p = subprocess.run(
            [nfrc, "join", "--no-start", "-c", agent2_cfg, "--name", "tunnelnode"] + join_argv[2:],
            capture_output=True,
            text=True,
            timeout=30,
        )
        check("tunnel node joined via join_command", p.returncode == 0, (p.stderr or p.stdout).strip()[-300:])

        if p.returncode == 0:
            agent2_log = open(os.path.join(tmp, "agent2.log"), "wb")
            agent2 = subprocess.Popen(
                [nfrc, "-c", agent2_cfg], stdout=agent2_log, stderr=subprocess.STDOUT
            )
            agent2_log.close()
            procs.append(agent2)

        end = time.time() + 20
        tun2 = None
        while time.time() < end:
            _, state = panel.json("/nfr/api/dashboard")
            tun2 = next((t for t in state.get("tunnels", []) if t["id"] == tid), None)
            if tun2 and tun2.get("node_id") and not tun2.get("awaiting"):
                break
            time.sleep(0.3)
        check("tunnel bound to new node", tun2 is not None and bool(tun2.get("node_id")) and not tun2.get("awaiting"))

        join_port = tun2.get("remote_port") if tun2 else None
        ok = False
        if join_port:
            end = time.time() + 10
            while time.time() < end:
                if tcp_probe(join_port):
                    ok = True
                    break
                time.sleep(0.2)
        check("tunnel-first tcp traffic flows", ok, "port %s" % join_port)

        try:
            p = subprocess.run(
                [nfrc, "join", "--no-start", "-c", agent2_cfg, "--name", "dupnode", "127.0.0.1:%d" % control, code2],
                capture_output=True,
                text=True,
                timeout=30,
            )
        except subprocess.TimeoutExpired:
            p = None
        check(
            "spent join code is rejected",
            p is not None and p.returncode != 0 and "already used" in (p.stderr + p.stdout).lower(),
            (p.stderr or p.stdout).strip()[-120:] if p else "nfrc timed out",
        )

        # 8b. Standalone panel tunnel -> per-tunnel config -> nfrc online -> kick offline.
        st, st_created = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "standalone-web",
                "node_id": "",
                "type": "tcp",
                "local": "127.0.0.1:%d" % local_port,
                "enabled": True,
            },
        )
        st_tun = st_created.get("tunnel") or {}
        st_id = st_tun.get("id", "")
        check(
            "standalone tunnel created offline",
            st == 201 and bool(st_id) and not st_tun.get("online") and st_tun.get("remote_port", 0) > 0,
            "status %d body=%s" % (st, st_created),
        )
        if st_id:
            st_c, cfg_res = panel.json("/nfr/api/tunnels/%s/config" % st_id)
            toml_text = cfg_res.get("toml", "")
            check(
                "per-tunnel config generated",
                st_c == 200 and ('name = "t-%s"' % st_id) in toml_text and 'name = "standalone-web"' in toml_text,
                "status %d" % st_c,
            )
            st_cfg_path = os.path.join(tmp, "standalone.toml")
            with open(st_cfg_path, "w") as sf:
                sf.write(toml_text)
            st_log = open(os.path.join(tmp, "standalone-agent.log"), "wb")
            st_agent = subprocess.Popen([nfrc, "-c", st_cfg_path], stdout=st_log, stderr=subprocess.STDOUT)
            st_log.close()
            procs.append(st_agent)

            st_online = False
            end = time.time() + 15
            while time.time() < end:
                _, dstate = panel.json("/nfr/api/dashboard")
                cur = next((t for t in dstate.get("tunnels", []) if t["id"] == st_id), None)
                if cur and cur.get("online"):
                    st_online = True
                    break
                time.sleep(0.25)
            check("standalone tunnel comes online via per-tunnel config", st_online)
            check(
                "standalone tunnel proxies traffic",
                st_online and tcp_probe(st_tun["remote_port"]),
                "port %s" % st_tun.get("remote_port"),
            )

            st_k, kick_res = panel.json("/nfr/api/tunnels/%s/kick" % st_id, method="POST")
            check("kick standalone tunnel api", st_k == 200 and kick_res.get("kicked") is True, str(kick_res))
            try:
                st_agent.wait(5)
                exited = True
            except subprocess.TimeoutExpired:
                exited = False
            check("kicked nfrc exits cleanly without reconnecting", exited)

            # 8b. Bring the exact same tunnel back online with zero-file `nfrc up <nfr1_token>`.
            quick_tok = cfg_res.get("quick_token", "")
            check(
                "quick-start launch token generated",
                quick_tok.startswith("nfr1_") and cfg_res.get("quick_cmd_unix") == ("./nfrc up " + quick_tok),
                str(cfg_res.get("quick_cmd_unix")),
            )
            if quick_tok:
                q_log = open(os.path.join(tmp, "quick-agent.log"), "wb")
                q_agent = subprocess.Popen([nfrc, "up", quick_tok], stdout=q_log, stderr=subprocess.STDOUT)
                q_log.close()
                procs.append(q_agent)
                q_online = False
                end = time.time() + 15
                while time.time() < end:
                    _, dstate = panel.json("/nfr/api/dashboard")
                    cur = next((t for t in dstate.get("tunnels", []) if t["id"] == st_id), None)
                    if cur and cur.get("online"):
                        q_online = True
                        break
                    time.sleep(0.25)
                check("nfrc up brings tunnel online without config file", q_online)
                check(
                    "nfrc up tunnel proxies traffic",
                    q_online and tcp_probe(st_tun["remote_port"]),
                    "port %s" % st_tun.get("remote_port"),
                )
                panel.json("/nfr/api/tunnels/%s/kick" % st_id, method="POST")
                try:
                    q_agent.wait(5)
                except subprocess.TimeoutExpired:
                    q_agent.terminate()

            panel.json("/nfr/api/tunnels/%s" % st_id, method="DELETE")

        # 9. HTTP on dedicated port without domain.
        st, halloc = panel.json("/nfr/api/ports/allocate", method="POST", payload={"network": "tcp"})
        st, created_hp = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "webport",
                "node_id": node["id"],
                "type": "http",
                "local": "127.0.0.1:%d" % local_port,
                "remote_port": halloc.get("port", 0),
                "lease_id": halloc.get("lease_id", ""),
                "hosts": [],
                "enabled": True,
            },
        )
        check("create http tunnel on a port", st == 201 and bool((created_hp.get("tunnel") or {}).get("id")), "status %d" % st)

        ok = False
        hport = halloc.get("port")
        if hport:
            end = time.time() + 10
            while time.time() < end:
                try:
                    st_h, body = http_get(hport, "127.0.0.1")
                    if st_h == 200 and LocalService.payload in body:
                        ok = True
                        break
                except OSError:
                    pass
                time.sleep(0.2)
        check("http tunnel on a port proxies", ok, "port %s" % hport)

        # 10. Toggle tunnel off/on and verify probe disabled status.
        st, _ = panel.json("/nfr/api/tunnels/%s/toggle" % web_tun["id"], method="POST", payload={"enabled": False})
        check("disable tunnel api", st == 200, "status %d" % st)
        ok = False
        last_st = 0
        for _ in range(20):
            try:
                last_st, _ = http_get(ingress, "test.local")
                if last_st == 502:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check("disabled tunnel stops traffic", ok, "status %d" % last_st)

        st, p_dis = panel.json("/nfr/api/tunnels/%s/probe" % web_tun["id"], method="POST")
        check("probe disabled tunnel", st == 200 and p_dis.get("status") == "disabled", str(p_dis))

        panel.json("/nfr/api/tunnels/%s/toggle" % web_tun["id"], method="POST", payload={"enabled": True})
        ok = False
        for _ in range(20):
            try:
                last_st, body = http_get(ingress, "test.local")
                if last_st == 200 and LocalService.payload in body:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check("re-enabled tunnel resumes", ok, "status %d" % last_st)

        check("agent stayed connected", agent.poll() is None)

        # 11. REST API + /metrics + /api/v1/certs + SSE.
        st, _ = panel.json("/nfr/api/settings/api-toggle", method="POST", payload={"enabled": True})
        check("enable REST API", st == 200, "status %d" % st)

        st, kresp = panel.json("/nfr/api/api-keys", method="POST", payload={"label": "e2e", "scope": "read"})
        key = kresp.get("key", "")
        check("api key created", st == 201 and bool(key), "status %d body=%s" % (st, kresp))

        api_ctx = ssl.create_default_context()
        api_ctx.check_hostname = False
        api_ctx.verify_mode = ssl.CERT_NONE

        def api_text(path, token=None):
            headers = {"Authorization": "Bearer " + token} if token else {}
            req = urllib.request.Request("https://127.0.0.1:%d%s" % (control, path), headers=headers)
            try:
                with urllib.request.urlopen(req, context=api_ctx, timeout=5) as resp:
                    return resp.status, resp.read().decode("utf-8", "replace")
            except urllib.error.HTTPError as e:
                return e.code, e.read().decode("utf-8", "replace")

        check("api accepts a valid key", api_text("/api/v1/info", key)[0] == 200)
        check("api rejects anonymous calls", api_text("/api/v1/info")[0] in (401, 403))

        st_m, body_m = api_text("/metrics", key)
        check("metrics endpoint works", st_m == 200 and "nfr_up 1" in body_m, "status %d" % st_m)
        check("metrics needs a key", api_text("/metrics")[0] in (401, 403))

        st_c, body_c = api_text("/api/v1/certs", key)
        cert = json.loads(body_c) if st_c == 200 else {}
        check(
            "certs endpoint describes certificate",
            st_c == 200 and cert.get("mode") == "pin" and cert.get("source") == "self-signed",
            str(cert)[:120],
        )

        events = []

        def sse():
            req = urllib.request.Request(
                "https://127.0.0.1:%d/api/v1/events" % control,
                headers={"Authorization": "Bearer " + key},
            )
            try:
                with urllib.request.urlopen(req, context=api_ctx, timeout=10) as resp:
                    for line in resp:
                        if line.startswith(b"data: "):
                            events.append(line.decode("utf-8", "replace"))
                            break
            except Exception as exc:
                events.append("error: %r" % (exc,))

        threading.Thread(target=sse, daemon=True).start()
        time.sleep(0.5)
        for on in (False, True):
            panel.json("/nfr/api/tunnels/%s/toggle" % web_tun["id"], method="POST", payload={"enabled": on})
        end = time.time() + 6
        while time.time() < end and not events:
            time.sleep(0.1)
        check("SSE stream delivers events", any(e.startswith("data: ") for e in events), str(events[:1]))

        # 12. Server restart resilience.
        server.terminate()
        try:
            server.wait(5)
        except subprocess.TimeoutExpired:
            server.kill()
        server_log.close()
        server_log = open(os.path.join(tmp, "server.log"), "ab")
        server = subprocess.Popen(
            [nfrs, "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT, env=srv_env
        )
        procs[0] = server
        check("server restarted", wait_port(control))

        panel2 = Panel("https://127.0.0.1:%d" % control)
        panel2.login(password)
        ok = False
        end = time.time() + 30
        while time.time() < end:
            _, state = panel2.json("/nfr/api/dashboard")
            if any(n.get("online") for n in state.get("nodes", [])) and len(state.get("tunnels", [])) >= 3:
                ok = True
                break
            time.sleep(0.3)
        check("agent reconnected after restart", ok)

        ok = False
        for _ in range(40):
            try:
                last_st, body = http_get(ingress, "test.local")
                if last_st == 200 and LocalService.payload in body:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check("tunnels survive a restart", ok, "status %d" % last_st)

    finally:
        cleanup()

    print()
    if failures:
        print("E2E FAILED: " + ", ".join(failures))
        return 1
    print("E2E OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
