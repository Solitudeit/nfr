#!/usr/bin/env python3
"""End-to-end test for UDP tunnels.

Starts a temporary server, joins an agent, creates a UDP tunnel towards a
local echo socket and drives datagrams through the whole chain:

    visitor -> public udp port -> yamux stream -> agent -> 127.0.0.1:<echo>

    python3 scripts/e2e_udp.py [--keep]
"""

import argparse
import http.cookiejar
import json
import os
import re
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

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NFRS_BIN = os.environ.get("NFRS_BIN", os.path.join(ROOT, "bin", "nfrs"))
NFRC_BIN = os.environ.get("NFRC_BIN", os.path.join(ROOT, "bin", "nfrc"))

failures = []


def check(name, cond, detail=""):
    stamp = time.strftime("%H:%M:%S", time.localtime()) + ".%03d" % (int(time.time() * 1000) % 1000)
    if cond:
        print("  ok   [" + stamp + "] " + name)
        return True
    print("  FAIL [" + stamp + "] " + name + ("  (" + detail + ")" if detail else ""))
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
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(self.jar),
        )
        self.csrf = ""

    def json(self, path, method="GET", payload=None, headers=None):
        hdrs = dict(headers or {})
        data = None
        if payload is not None:
            data = json.dumps(payload).encode("utf-8")
            hdrs.setdefault("Content-Type", "application/json")
        if method in ("POST", "PATCH", "PUT", "DELETE") and self.csrf:
            hdrs.setdefault("X-CSRF", self.csrf)
        req = urllib.request.Request(self.base + path, data=data, headers=hdrs, method=method)
        try:
            with self.opener.open(req, timeout=15) as resp:
                raw = resp.read().decode("utf-8", "replace")
                return resp.status, (json.loads(raw) if raw.strip() else {})
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            try:
                parsed = json.loads(raw) if raw.strip() else {}
            except ValueError:
                parsed = {"_raw": raw}
            return e.code, parsed

    def login(self, password):
        st, data = self.json("/nfr/api/auth/login", method="POST", payload={"password": password})
        if st == 200 and data.get("csrf"):
            self.csrf = data["csrf"]
        return st, data


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
        print("missing binary: " + ", ".join(missing) +
              " (run: go build -o bin/nfrs ./cmd/nfrs && go build -o bin/nfrc ./cmd/nfrc)")
        return 2

    tmp = tempfile.mkdtemp(prefix="nfr-e2e-udp-")
    procs = []
    echo_sock = None
    print("work dir " + tmp)

    def cleanup():
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(5)
                except subprocess.TimeoutExpired:
                    p.kill()
        if echo_sock is not None:
            echo_sock.close()
        if args.keep or failures:
            print("logs kept in " + tmp)
            try:
                print(open(os.path.join(tmp, "server.log"), errors="replace").read()[-2000:])
            except OSError:
                pass
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        control = free_port()
        echo_port = free_udp_port()

        data_dir = os.path.join(tmp, "data")
        cfg = os.path.join(tmp, "server.toml")
        with open(cfg, "w") as f:
            f.write(
                'control_listen = "127.0.0.1:%d"\n'
                'http_listen = "off"\n'
                'https_listen = "off"\n'
                'data_dir = "%s"\n'
                'port_range = [20000, 29999]\n'
                'log_level = "debug"\n'
                '[panel]\n'
                'https = true\n' % (control, data_dir)
            )

        server_log = open(os.path.join(tmp, "server.log"), "wb")
        env = dict(os.environ, NFR_DISABLE_UPDATE_CHECK="1", NFR_STORAGE="file")
        server = subprocess.Popen(
            [nfrs, "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT, env=env
        )
        procs.append(server)
        if not wait_port(control):
            print("server never listened")
            return 1

        text = open(os.path.join(tmp, "server.log"), errors="replace").read()
        m = re.search(r"密码\s+(\S+)", text)
        if not m:
            print("could not find the first-run password in the banner")
            return 1
        password = m.group(1)

        panel = Panel("https://127.0.0.1:%d" % control)
        status, _ = panel.login(password)
        check("panel login", status == 200)

        status, jc = panel.json("/nfr/api/nodes/join-codes", method="POST", payload={"name": "testnode"})
        code = jc.get("code", "")
        if not check("join code issued", status in (200, 201) and bool(code), "status %d %s" % (status, jc)):
            return 1

        agent_cfg = os.path.join(tmp, "agent.toml")
        p = subprocess.run(
            [nfrc, "join", "--no-start", "-c", agent_cfg, "--name", "testnode", "127.0.0.1:%d" % control, code],
            capture_output=True, text=True,
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

        # Local "service": a UDP echo socket standing in for DNS.
        echo_sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        echo_sock.bind(("127.0.0.1", echo_port))
        echo_sock.settimeout(1.0)

        def echo_loop():
            while True:
                try:
                    data, src = echo_sock.recvfrom(65535)
                except socket.timeout:
                    continue
                except OSError:
                    return
                try:
                    echo_sock.sendto(data, src)
                except OSError:
                    return

        threading.Thread(target=echo_loop, daemon=True).start()

        # A UDP lease must come from the UDP space.
        status, alloc = panel.json(
            "/nfr/api/ports/allocate",
            method="POST",
            payload={"tunnel_id": "", "type": "udp"},
        )
        check("udp port allocated", status == 200 and bool(alloc.get("port")), str(alloc))

        status, res = panel.json(
            "/nfr/api/tunnels",
            method="POST",
            payload={
                "name": "dns",
                "node_id": node["id"],
                "type": "udp",
                "local": "127.0.0.1:%d" % echo_port,
                "remote_port": alloc.get("port", 0),
                "lease_id": alloc.get("lease_id", ""),
                "enabled": True,
            },
        )
        check("create udp tunnel", status in (200, 201), "status %d %s" % (status, res))

        pub = alloc.get("port")

        def visit(payload, src_port=0, timeout=2.0):
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            if src_port:
                s.bind(("127.0.0.1", src_port))
            s.settimeout(timeout)
            try:
                s.sendto(payload, ("127.0.0.1", pub))
                data, _ = s.recvfrom(65535)
                return data
            except socket.timeout:
                return None
            finally:
                s.close()

        ok, data = False, None
        end = time.time() + 15
        while time.time() < end:
            data = visit(b"query-1")
            if data == b"query-1":
                ok = True
                break
            time.sleep(0.3)
        check("udp tunnel echoes (first packet)", ok, "reply %r" % data)

        # The same visitor address reuses its session and still round trips.
        ok = visit(b"query-2") == b"query-2"
        check("udp session reused", ok)

        # A different visitor address gets its own session.
        ok = visit(b"other-visitor", src_port=free_udp_port()) == b"other-visitor"
        check("second visitor session works", ok)

        # A large-ish payload (still a single datagram).
        big = bytes(range(256)) * 60  # 15360 bytes
        ok = visit(big) == big
        check("large datagram round trips", ok)

        # Metrics see the traffic.
        _, state = panel.json("/nfr/api/dashboard")
        tun = next((t for t in state.get("tunnels", []) if t["name"] == "dns"), {})
        check("traffic counted", tun.get("in", 0) > 0 and tun.get("out", 0) > 0, str(tun)[:120])

        # Toggling stops and restores the datagram path.
        if tun:
            panel.json(
                "/nfr/api/tunnels/%s/toggle" % tun["id"],
                method="POST",
                payload={"enabled": False},
            )
            ok = False
            end = time.time() + 6
            while time.time() < end:
                if visit(b"after-off", timeout=1.0) is None:
                    ok = True
                    break
                time.sleep(0.2)
            check("disabled udp tunnel stops traffic", ok)

            panel.json(
                "/nfr/api/tunnels/%s/toggle" % tun["id"],
                method="POST",
                payload={"enabled": True},
            )
            ok = False
            end = time.time() + 10
            while time.time() < end:
                if visit(b"after-on") == b"after-on":
                    ok = True
                    break
                time.sleep(0.3)
            check("re-enabled udp tunnel resumes", ok)
        else:
            check("find the udp tunnel", False)

        alive = agent.poll() is None
        check("agent stayed connected", alive)

    finally:
        cleanup()

    print()
    if failures:
        print("E2E FAILED: " + ", ".join(failures))
        return 1
    print("UDP E2E OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
