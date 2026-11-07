#!/usr/bin/env python3
"""Federation end-to-end test: one console manages a second, independent node.

Starts two NFR servers (A and B), enables the API on B, adds B to A with a
key, then creates a tunnel on B *from A's console* and drives traffic through
B. Also checks TOFU pinning and reachability reporting.

    python3 scripts/e2e_federation.py
"""

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
PAYLOAD = b"hello from the second node\n"

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
    def __init__(self, port):
        self.base = "https://127.0.0.1:%d" % port
        self.jar = http.cookiejar.CookieJar()
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(self.jar),
        )
        self.csrf = ""

    def request(self, path, form=None, headers=None):
        data = urllib.parse.urlencode(form).encode() if form is not None else None
        req = urllib.request.Request(self.base + path, data=data, headers=headers or {})
        try:
            with self.opener.open(req, timeout=15) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def login(self, password):
        self.request("/_nfr/login")
        status, html = self.request("/_nfr/login", {"password": password})
        _, page = self.request("/_nfr/nodes")
        m = re.search(r'name="csrf" value="([^"]+)"', page)
        self.csrf = m.group(1) if m else ""
        return status, html


class LocalService(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(PAYLOAD)))
        self.end_headers()
        self.wfile.write(PAYLOAD)

    def log_message(self, *args):
        pass


def http_get(port, host_header, timeout=5):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    conn.request("GET", "/", headers={"Host": host_header})
    resp = conn.getresponse()
    body = resp.read()
    conn.close()
    return resp.status, body


def start_server(binary, tmp, name):
    control = free_port()
    ingress = free_port()
    base = os.path.join(tmp, name)
    os.makedirs(base, exist_ok=True)
    cfg = os.path.join(base, "server.toml")
    with open(cfg, "w") as f:
        f.write(
            'control_listen = "127.0.0.1:%d"\n'
            'http_listen = "127.0.0.1:%d"\n'
            'https_listen = "off"\n'
            'data_dir = "%s"\n'
            'port_range = [20000, 29999]\n'
            'log_level = "debug"\n' % (control, ingress, os.path.join(base, "data"))
        )
    log = open(os.path.join(base, "server.log"), "wb")
    proc = subprocess.Popen([binary, "server", "-c", cfg], stdout=log, stderr=subprocess.STDOUT)
    if not wait_port(control):
        raise RuntimeError("server " + name + " never listened on " + str(control))
    text = open(os.path.join(base, "server.log"), errors="replace").read()
    m = re.search(r"Password\s+(\S+)", text)
    if not m:
        raise RuntimeError("no first-run password for " + name)
    return proc, control, ingress, m.group(1)


def main():
    binary = os.path.abspath(BIN)
    if not os.path.exists(binary):
        print("missing binary: " + binary + " (run: go build -o bin/nfr ./cmd/nfr)")
        return 2

    tmp = tempfile.mkdtemp(prefix="nfr-fed-")
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
        if failures:
            print("logs kept in " + tmp)

    try:
        a_proc, a_ctl, a_ing, a_pw = start_server(binary, tmp, "a")
        procs.append(a_proc)
        b_proc, b_ctl, b_ing, b_pw = start_server(binary, tmp, "b")
        procs.append(b_proc)

        A = Panel(a_ctl)
        B = Panel(b_ctl)
        check("login A", A.login(a_pw)[0] == 200)
        check("login B", B.login(b_pw)[0] == 200)

        # B: enable the API, then mint an admin key.
        status, html = B.request(
            "/_nfr/settings",
            {
                "csrf": B.csrf, "name": "node-b", "domain": "",
                "api_enabled": "1", "port_low": "20000", "port_high": "29999", "email": "",
            },
        )
        check("B API enabled", status == 200 and "设置已保存" in html, "status %d" % status)

        status, html = B.request("/_nfr/settings",
                                 {"csrf": B.csrf, "action": "key", "label": "console-a", "scope": "admin"})
        m = re.search(r'data-newkey="([^"]+)"', html)
        check("B admin key created", m is not None, "status %d" % status)
        if not m:
            return 1
        key = m.group(1)

        # A: add B with TOFU (no pin supplied).
        status, html = A.request(
            "/_nfr/servers",
            {"csrf": A.csrf, "url": "https://127.0.0.1:%d" % b_ctl, "api_key": key, "name": "Node B", "pin": ""},
        )
        err = re.search(r'class="flash err"[^>]*>\s*([^<]+)', html)
        check("A added B", "已添加 Node B" in html,
              "status %d err=%s" % (status, err.group(1).strip() if err else "?"))
        check("A card shows B online", "在线" in html and "Node B" in html)

        m = re.search(r'href="/_nfr/tunnels\?server=([0-9a-z]+)"', html)
        check("B appears as a server card", m is not None)
        if not m:
            return 1
        b_id = m.group(1)

        # B: join an agent so B can actually forward traffic.
        status, html = B.request("/_nfr/nodes", {"csrf": B.csrf, "name": "node-b-agent"})
        m = re.search(r'class="join-code">([0-9A-Z]+)<', html)
        check("B join code issued", m is not None)
        if not m:
            return 1
        code = m.group(1)

        agent_cfg = os.path.join(tmp, "b-agent.toml")
        p = subprocess.run(
            [binary, "join", "--no-start", "-c", agent_cfg, "--name", "node-b-agent",
             "127.0.0.1:%d" % b_ctl, code],
            capture_output=True, text=True,
        )
        check("B agent joined", p.returncode == 0, (p.stderr or p.stdout).strip()[-200:])
        agent_log = open(os.path.join(tmp, "b-agent.log"), "wb")
        agent = subprocess.Popen([binary, "agent", "-c", agent_cfg], stdout=agent_log, stderr=subprocess.STDOUT)
        procs.append(agent)

        b_node = None
        end = time.time() + 20
        while time.time() < end:
            _, body = B.request("/_nfr/api/state")
            online = [n for n in json.loads(body)["nodes"] if n.get("online")]
            if online:
                b_node = online[0]
                break
            time.sleep(0.3)
        check("B agent online", b_node is not None)
        if not b_node:
            return 1

        local = free_port()
        httpd = ThreadingHTTPServer(("127.0.0.1", local), LocalService)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()

        # Create a tunnel on B through A's console.
        status, html = A.request(
            "/_nfr/tunnels/save",
            {
                "csrf": A.csrf, "server": b_id, "name": "on-b", "node_id": b_node["id"],
                "type": "http", "local": "127.0.0.1:%d" % local,
                "hosts": "b.test", "enabled": "1",
            },
        )
        check("A created a tunnel on B", "隧道已保存" in html, "status %d" % status)

        ok = False
        for _ in range(40):
            st, body = http_get(b_ing, "b.test")
            if st == 200 and PAYLOAD in body:
                ok = True
                break
            time.sleep(0.2)
        check("tunnel on B forwards traffic", ok, "status %d" % st)

        # Wrong pin must be rejected.
        status, html = A.request(
            "/_nfr/servers",
            {
                "csrf": A.csrf, "url": "https://127.0.0.1:%d" % b_ctl, "api_key": key,
                "name": "B wrong pin",
                "pin": "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
            },
        )
        check("wrong pin rejected", re.search(r"fingerprint|mismatch|refused|certificate", html, re.I) is not None, "status %d" % status)

        # Turning the API off on B must surface as unreachable on A.
        B.request(
            "/_nfr/settings",
            {
                "csrf": B.csrf, "name": "node-b", "domain": "",
                "port_low": "20000", "port_high": "29999", "email": "",
            },
        )
        status, html = A.request("/_nfr/servers")
        check("A reports B unreachable after API off", "不可达" in html, "status %d" % status)

    finally:
        cleanup()

    print()
    if failures:
        print("FEDERATION E2E FAILED: " + ", ".join(failures))
        return 1
    print("FEDERATION E2E OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
