#!/usr/bin/env python3
"""End-to-end smoke test for a single NFR node.

Starts a temporary server, joins an agent, creates one HTTP and one TCP
tunnel and drives traffic through both. Only the Python standard library is
used, so it runs on any machine that has python3 and a built ./nfr binary.

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
BIN = os.environ.get("NFR_BIN", os.path.join(ROOT, "bin", "nfr"))

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
        self.request("/_nfr/login")
        status, html = self.request("/_nfr/login", {"password": password})
        # The overview has no form of its own; take the token from the nodes page.
        _, page = self.request("/_nfr/nodes")
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--bin", default=BIN)
    args = ap.parse_args()

    binary = os.path.abspath(args.bin)
    if not os.path.exists(binary):
        print("missing binary: " + binary + " (run: go build -o bin/nfr ./cmd/nfr)")
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
            [binary, "server", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT
        )
        procs.append(server)
        if not wait_port(control):
            print("server never listened")
            return 1

        text = open(os.path.join(tmp, "server.log"), errors="replace").read()
        m = re.search(r"Password\s+(\S+)", text)
        if not m:
            print("could not find the first-run password in the banner")
            return 1
        password = m.group(1)

        panel = Panel("https://127.0.0.1:%d" % control)
        status, html = panel.login(password)
        check("panel login", status == 200 and "概览" in html, "status %d" % status)

        status, html = panel.request("/_nfr/nodes", {"csrf": panel.csrf, "name": "testnode"})
        m = re.search(r'class="join-code">([0-9A-Z]+)<', html)
        check("join code issued", m is not None, "status %d" % status)
        if not m:
            return 1
        code = m.group(1)

        agent_cfg = os.path.join(tmp, "agent.toml")
        p = subprocess.run(
            [binary, "join", "-c", agent_cfg, "--name", "testnode", "127.0.0.1:%d" % control, code],
            capture_output=True,
            text=True,
        )
        check("agent joined", p.returncode == 0, (p.stderr or p.stdout).strip()[-300:])

        agent_log = open(os.path.join(tmp, "agent.log"), "wb")
        agent = subprocess.Popen(
            [binary, "agent", "-c", agent_cfg], stdout=agent_log, stderr=subprocess.STDOUT
        )
        procs.append(agent)

        node = None
        end = time.time() + 20
        while time.time() < end:
            _, body = panel.request("/_nfr/api/state")
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
            "/_nfr/tunnels/save",
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

        _, body = panel.request("/_nfr/api/allocate", {"tunnel_id": ""}, {"X-CSRF": panel.csrf})
        alloc = json.loads(body)
        check("port allocated", bool(alloc.get("port")), str(alloc))

        status, html = panel.request(
            "/_nfr/tunnels/save",
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

        # Toggling a tunnel must stop traffic without touching the agent.
        _, body = panel.request("/_nfr/api/state")
        tunnels = json.loads(body)["tunnels"]
        web = next((t for t in tunnels if t["name"] == "web"), None)
        if web:
            panel.request(
                "/_nfr/tunnels/toggle",
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
                "/_nfr/tunnels/toggle",
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
            "/_nfr/settings",
            {"csrf": panel.csrf, "name": "testnode", "domain": "", "api_enabled": "1",
             "port_low": "20000", "port_high": "29999", "email": ""},
        )
        status, html = panel.request("/_nfr/keys", {"csrf": panel.csrf, "label": "e2e", "scope": "read"})
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
                    "/_nfr/tunnels/toggle",
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
            [binary, "server", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT
        )
        procs[0] = server
        check("server restarted", wait_port(control))

        panel2 = Panel("https://127.0.0.1:%d" % control)
        panel2.login(password)
        ok = False
        end = time.time() + 30
        while time.time() < end:
            _, body = panel2.request("/_nfr/api/state")
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
