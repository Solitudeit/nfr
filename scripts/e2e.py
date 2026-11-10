#!/usr/bin/env python3
"""End-to-end smoke test for a single NFR node.

Starts a temporary server, joins an agent, creates one HTTP and one TCP
tunnel and drives traffic through both. Only the Python standard library is
used, so it runs on any machine that has python3 and the built nfrs / nfrc
binaries.

    python3 scripts/e2e.py                 # builds its own temp dir
    python3 scripts/e2e.py --keep          # keep the temp dir for debugging
"""

import argparse
import http.client
import http.cookiejar
import json
import os
import re
import shutil
import shlex
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
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

    def request(self, path, form=None, headers=None, method=None):
        data = urllib.parse.urlencode(form).encode() if form is not None else None
        req = urllib.request.Request(
            self.base + path, data=data, headers=headers or {}, method=method
        )
        try:
            with self.opener.open(req, timeout=10) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def login(self, password):
        self.request("/nfr/login")
        status, html = self.request("/nfr/login", {"password": password})
        # The overview has no form of its own; take the token from the nodes page.
        _, page = self.request("/nfr/nodes")
        m = re.search(r'name="csrf" value="([^"]+)"', page)
        self.csrf = m.group(1) if m else ""
        return status, html


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


def http_get(port, host_header, timeout=5):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    conn.request("GET", "/", headers={"Host": host_header})
    resp = conn.getresponse()
    body = resp.read()
    conn.close()
    return resp.status, body


def tcp_probe(port):
    """Send one request through a raw TCP tunnel; True when the private
    service payload comes back."""
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
              " (run: make build, or: go build -o bin/nfrs ./cmd/nfrs && go build -o bin/nfrc ./cmd/nfrc)")
        return 2

    tmp = tempfile.mkdtemp(prefix="nfr-e2e-")
    procs = []
    httpd = None
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
        server_log = os.path.join(tmp, "server.log")
        if args.keep or failures:
            print("logs kept in " + tmp)
            try:
                print(open(server_log).read()[-3000:])
            except OSError:
                pass
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        control = free_port()
        ingress = free_port()
        local_port = free_port()

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
        server = subprocess.Popen(
            [nfrs, "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT
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
        status, html = panel.login(password)
        # /nfr/ 303-redirects to the tunnels page (urllib follows it), whose
        # nav reads 隧道/节点/服务器/设置 and whose local view carries the
        # stats block merged from the old overview.
        check(
            "panel login",
            status == 200
            and ">隧道</span>" in html
            and 'data-stat="nodes-online"' in html,
            "status %d" % status,
        )

        status, html = panel.request("/nfr/nodes", {"csrf": panel.csrf, "name": "testnode"})
        m = re.search(r'class="join-code">([0-9A-Z]+)<', html)
        check("join code issued", m is not None, "status %d" % status)
        if not m:
            return 1
        code = m.group(1)

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
            _, body = panel.request("/nfr/api/state")
            state = json.loads(body)
            online = [n for n in state["nodes"] if n.get("online")]
            if online:
                node = online[0]
                break
            time.sleep(0.3)
        check("agent online", node is not None)
        if not node:
            return 1

        httpd = ThreadingHTTPServer(("127.0.0.1", local_port), LocalService)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()

        status, html = panel.request(
            "/nfr/tunnels/save",
            {
                "csrf": panel.csrf, "server": "local", "name": "web",
                "node_id": node["id"], "type": "http",
                "local": "127.0.0.1:%d" % local_port,
                "hosts": "test.local", "enabled": "1",
            },
        )
        check("create http tunnel", "隧道已保存" in html, "status %d" % status)

        ok = False
        for _ in range(40):
            st, body = http_get(ingress, "test.local")
            if st == 200 and LocalService.payload in body:
                ok = True
                break
            time.sleep(0.2)
        check("http tunnel proxies", ok, "status %d" % st)

        _, body = panel.request("/nfr/api/allocate", {"tunnel_id": ""}, {"X-CSRF": panel.csrf})
        alloc = json.loads(body)
        check("port allocated", bool(alloc.get("port")), str(alloc))

        status, html = panel.request(
            "/nfr/tunnels/save",
            {
                "csrf": panel.csrf, "server": "local", "name": "raw",
                "node_id": node["id"], "type": "tcp",
                "local": "127.0.0.1:%d" % local_port,
                "remote_port": str(alloc.get("port", "")),
                "lease_id": alloc.get("lease_id", ""), "enabled": "1",
            },
        )
        check("create tcp tunnel", "隧道已保存" in html, "status %d" % status)

        ok = False
        port = alloc.get("port")
        if port:
            end = time.time() + 10
            while time.time() < end:
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=3) as s:
                        s.sendall(b"GET / HTTP/1.1\r\nHost: raw\r\nConnection: close\r\n\r\n")
                        data = b""
                        while True:
                            chunk = s.recv(4096)
                            if not chunk:
                                break
                            data += chunk
                    if LocalService.payload in data:
                        ok = True
                        break
                except OSError:
                    pass
                time.sleep(0.2)
        check("tcp tunnel proxies", ok, "port %s" % port)

        # Tunnel-first flow: creating a tunnel for a not-yet-joined device
        # mints a join command on the awaiting card; redeeming it brings the
        # device in and activates the tunnel in one step.
        status, html = panel.request(
            "/nfr/tunnels/save",
            {
                "csrf": panel.csrf, "server": "local", "type": "tcp",
                "node_id": "new", "local": "127.0.0.1:%d" % local_port,
                "enabled": "1",
            },
        )
        m = re.search(
            # The tempered (?!</article>) guard keeps every match inside ONE
            # card: without it the unbounded window could pair an id="t-…"
            # from a PRECEDING card with the awaiting card's command/code.
            r'id="t-([0-9a-z]+)"(?:(?!</article>).){0,4000}?<code>(nfrc join [^<]+)</code>'
            r'(?:(?!</article>).){0,400}?class="join-code">([0-9A-Z]+)<',
            html, re.S,
        )
        check("awaiting tunnel created", m is not None, "status %d" % status)
        if not m:
            return 1

        tid, command_text, code2 = m.group(1), m.group(2), m.group(3)
        # Cross-card guard: the regex must have bound the id of THE awaiting
        # tunnel. If it ever pairs an id="t-…" from a preceding card with
        # this card's command (the bug the tempered </article> guard fixes),
        # the api state says otherwise. Checked BEFORE redeeming, since the
        # join flips the awaiting flag off.
        _, body = panel.request("/nfr/api/state")
        awaiting = [t for t in json.loads(body)["tunnels"] if t.get("awaiting")]
        check("card regex owns the awaiting tunnel",
              len(awaiting) == 1 and awaiting[0]["id"] == tid,
              "awaiting=%s captured t-%s" % ([t["id"] for t in awaiting], tid))
        check("join command rendered", command_text.startswith("nfrc join "),
              command_text[:60])
        # The card command joins and then keeps the agent connected (it is
        # meant to be pasted on the device), so the harness redeems it with
        # --no-start and its own config dir instead. Flags must precede the
        # positionals: Go's flag package stops parsing at the first one.
        join_argv = shlex.split(command_text)
        agent2_cfg = os.path.join(tmp, "agent2.toml")
        p = subprocess.run(
            [nfrc, "join", "--no-start", "-c", agent2_cfg,
             "--name", "tunnelnode"] + join_argv[2:],
            capture_output=True, text=True, timeout=30,
        )
        check("tunnel node joined via card command", p.returncode == 0,
              (p.stderr or p.stdout).strip()[-300:])

        # Start the second agent so the redeemed tunnel carries traffic,
        # mirroring the first agent's lifecycle above.
        if p.returncode == 0:
            agent2_log = open(os.path.join(tmp, "agent2.log"), "wb")
            agent2 = subprocess.Popen(
                [nfrc, "-c", agent2_cfg], stdout=agent2_log,
                stderr=subprocess.STDOUT,
            )
            agent2_log.close()  # Popen keeps the fd; the handle does not
            procs.append(agent2)

        end = time.time() + 20
        tun2 = None
        while time.time() < end:
            _, body = panel.request("/nfr/api/state")
            tun2 = next(
                (t for t in json.loads(body)["tunnels"] if t["id"] == tid), None
            )
            if tun2 and tun2.get("node_id"):
                break
            time.sleep(0.3)
        check("tunnel bound to the new node",
              tun2 is not None and bool(tun2.get("node_id")))

        status, html2 = panel.request("/nfr/tunnels?server=local")
        card = re.search(r'id="t-%s".*?(?=<article|</main)' % re.escape(tid), html2, re.S)
        check("card no longer awaits",
              card is not None
              and "等待接入" not in card.group(0)
              and 'class="join-code"' not in card.group(0))

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
                [nfrc, "join", "--no-start", "-c", agent2_cfg,
                 "--name", "dupnode", "127.0.0.1:%d" % control, code2],
                capture_output=True, text=True, timeout=30,
            )
        except subprocess.TimeoutExpired:
            # A hang is not a rejection: the client is expected to fail
            # fast once the server reports the code already used.
            p = None
        check("spent join code is rejected",
              p is not None and p.returncode != 0
              and "already used" in (p.stderr + p.stdout).lower(),
              (p.stderr or p.stdout).strip()[-120:] if p else "nfrc timed out")

        # HTTP can also be reached on a dedicated port, without any domain.
        _, body = panel.request("/nfr/api/allocate", {"tunnel_id": ""}, {"X-CSRF": panel.csrf})
        halloc = json.loads(body)
        status, html = panel.request(
            "/nfr/tunnels/save",
            {
                "csrf": panel.csrf, "server": "local", "name": "webport",
                "node_id": node["id"], "type": "http",
                "local": "127.0.0.1:%d" % local_port,
                "remote_port": str(halloc.get("port", "")),
                "lease_id": halloc.get("lease_id", ""), "hosts": "", "enabled": "1",
            },
        )
        check("create http tunnel on a port", "隧道已保存" in html, "status %d" % status)

        ok = False
        hport = halloc.get("port")
        if hport:
            end = time.time() + 10
            while time.time() < end:
                try:
                    st, body = http_get(hport, "127.0.0.1")
                    if st == 200 and LocalService.payload in body:
                        ok = True
                        break
                except OSError:
                    pass
                time.sleep(0.2)
        check("http tunnel on a port proxies", ok, "port %s" % hport)

        # Toggling a tunnel must stop traffic without touching the agent.
        _, body = panel.request("/nfr/api/state")
        tunnels = json.loads(body)["tunnels"]
        web = next((t for t in tunnels if t["name"] == "web"), None)
        if web:
            panel.request(
                "/nfr/tunnels/toggle",
                {"csrf": panel.csrf, "server": "local", "id": web["id"], "enabled": "false"},
            )
            ok = False
            for _ in range(20):
                st, _ = http_get(ingress, "test.local")
                if st == 502:
                    ok = True
                    break
                time.sleep(0.2)
            check("disabled tunnel stops traffic", ok, "status %d" % st)

            panel.request(
                "/nfr/tunnels/toggle",
                {"csrf": panel.csrf, "server": "local", "id": web["id"], "enabled": "true"},
            )
            ok = False
            for _ in range(20):
                st, body = http_get(ingress, "test.local")
                if st == 200 and LocalService.payload in body:
                    ok = True
                    break
                time.sleep(0.2)
            check("re-enabled tunnel resumes", ok, "status %d" % st)
        else:
            check("find the http tunnel", False)

        alive = agent.poll() is None
        check("agent stayed connected", alive)

        # REST API + SSE smoke test.
        panel.request(
            "/nfr/settings",
            {"csrf": panel.csrf, "name": "testnode", "domain": "", "api_enabled": "1",
             "port_low": "20000", "port_high": "29999", "email": ""},
        )
        status, html = panel.request("/nfr/settings",
                                     {"csrf": panel.csrf, "action": "key", "label": "e2e", "scope": "read"})
        m = re.search(r'data-newkey="([^"]+)"', html)
        check("api key created", m is not None, "status %d" % status)
        key = m.group(1) if m else ""

        api_ctx = ssl.create_default_context()
        api_ctx.check_hostname = False
        api_ctx.verify_mode = ssl.CERT_NONE

        def api_status(path, token=None):
            headers = {"Authorization": "Bearer " + token} if token else {}
            req = urllib.request.Request(
                "https://127.0.0.1:%d%s" % (control, path), headers=headers
            )
            try:
                with urllib.request.urlopen(req, context=api_ctx, timeout=5) as resp:
                    return resp.status
            except urllib.error.HTTPError as e:
                return e.code

        check("api accepts a valid key", api_status("/api/v1/info", key) == 200)
        check("api rejects anonymous calls", api_status("/api/v1/info") in (401, 403))

        def api_text(path, token=None):
            headers = {"Authorization": "Bearer " + token} if token else {}
            req = urllib.request.Request(
                "https://127.0.0.1:%d%s" % (control, path), headers=headers
            )
            try:
                with urllib.request.urlopen(req, context=api_ctx, timeout=5) as resp:
                    return resp.status, resp.read().decode("utf-8", "replace")
            except urllib.error.HTTPError as e:
                return e.code, e.read().decode("utf-8", "replace")

        st, body = api_text("/metrics", key)
        check("metrics endpoint works", st == 200 and "nfr_up 1" in body, "status %d body %s" % (st, body[:80]))
        check("metrics needs a key", api_text("/metrics")[0] in (401, 403), "status %d" % api_text("/metrics")[0])

        st, body = api_text("/api/v1/certs", key)
        cert = json.loads(body) if st == 200 else {}
        check("certs endpoint describes the certificate",
              st == 200 and cert.get("mode") == "pin" and cert.get("source") == "self-signed",
              str(cert)[:120])

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
            except Exception as exc:  # noqa: BLE001 - surfaced through the check below
                events.append("error: %r" % (exc,))

        threading.Thread(target=sse, daemon=True).start()
        time.sleep(0.5)
        if web:
            for on in ("false", "true"):
                panel.request(
                    "/nfr/tunnels/toggle",
                    {"csrf": panel.csrf, "server": "local", "id": web["id"], "enabled": on},
                )
        end = time.time() + 6
        while time.time() < end and not events:
            time.sleep(0.1)
        check("SSE stream delivers events", any(e.startswith("data: ") for e in events), str(events[:1]))

        # Resilience: a restart must keep the state and bring the agent back.
        server.terminate()
        try:
            server.wait(5)
        except subprocess.TimeoutExpired:
            server.kill()
        server_log.close()
        server_log = open(os.path.join(tmp, "server.log"), "ab")
        server = subprocess.Popen(
            [nfrs, "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT
        )
        procs[0] = server
        check("server restarted", wait_port(control))

        panel2 = Panel("https://127.0.0.1:%d" % control)
        panel2.login(password)
        ok = False
        end = time.time() + 30
        while time.time() < end:
            _, body = panel2.request("/nfr/api/state")
            state = json.loads(body)
            if any(n.get("online") for n in state["nodes"]) and len(state["tunnels"]) >= 2:
                ok = True
                break
            time.sleep(0.3)
        check("agent reconnected after restart", ok)

        ok = False
        for _ in range(40):
            st, body = http_get(ingress, "test.local")
            if st == 200 and LocalService.payload in body:
                ok = True
                break
            time.sleep(0.2)
        check("tunnels survive a restart", ok, "status %d" % st)

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
