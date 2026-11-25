#!/usr/bin/env python3
"""Federation REST API end-to-end test.

Verifies that backend federation (/api/v1/servers and ?server=<id> proxy)
remains fully functional via Bearer API keys after the Web Console focused
on single-host management.

Starts two NFR servers (A and B), enables the REST API and mints admin keys
on both via their single-host panel JSON API, registers B on A via
POST /api/v1/servers (with TOFU), joins an agent on B, creates a tunnel on B
through A's /api/v1/tunnels?server=<b_id>, drives real traffic through B,
checks wrong-pin rejection, reachability reporting, and server removal.

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
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NFRS_BIN = os.environ.get("NFRS_BIN", os.path.join(ROOT, "bin", "nfrs"))
NFRC_BIN = os.environ.get("NFRC_BIN", os.path.join(ROOT, "bin", "nfrc"))
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
        self.ctx = ctx
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
                text = resp.read().decode("utf-8", "replace")
                return resp.status, json.loads(text) if text.strip() else {}
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            try:
                parsed = json.loads(text) if text.strip() else {}
            except ValueError:
                parsed = {"_raw": text}
            return e.code, parsed

    def login(self, password):
        st, data = self.json("/nfr/api/auth/login", method="POST", payload={"password": password})
        if st == 200 and data.get("csrf"):
            self.csrf = data["csrf"]
        return st, data


class RestClient:
    def __init__(self, port, token):
        self.base = "https://127.0.0.1:%d" % port
        self.token = token
        self.ctx = ssl.create_default_context()
        self.ctx.check_hostname = False
        self.ctx.verify_mode = ssl.CERT_NONE

    def json(self, path, method="GET", payload=None):
        hdrs = {"Authorization": "Bearer " + self.token}
        data = None
        if payload is not None:
            data = json.dumps(payload).encode("utf-8")
            hdrs["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=hdrs, method=method)
        try:
            with urllib.request.urlopen(req, context=self.ctx, timeout=15) as resp:
                text = resp.read().decode("utf-8", "replace")
                return resp.status, json.loads(text) if text.strip() else {}
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8", "replace")
            try:
                parsed = json.loads(text) if text.strip() else {}
            except ValueError:
                parsed = {"_raw": text}
            return e.code, parsed


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


def start_server(nfrs, tmp, name):
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
    proc = subprocess.Popen([nfrs, "--foreground", "-c", cfg], stdout=log, stderr=subprocess.STDOUT)
    if not wait_port(control):
        raise RuntimeError("server " + name + " never listened on " + str(control))
    text = open(os.path.join(base, "server.log"), errors="replace").read()
    m = re.search(r"密码\s+(\S+)", text)
    if not m:
        raise RuntimeError("no first-run password for " + name)
    return proc, control, ingress, m.group(1)


def main():
    nfrs = os.path.abspath(NFRS_BIN)
    nfrc = os.path.abspath(NFRC_BIN)
    missing = [p for p in (nfrs, nfrc) if not os.path.exists(p)]
    if missing:
        print(
            "missing binary: "
            + ", ".join(missing)
            + " (run: make build, or: go build -o bin/nfrs ./cmd/nfrs && go build -o bin/nfrc ./cmd/nfrc)"
        )
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
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        a_proc, a_ctl, a_ing, a_pw = start_server(nfrs, tmp, "a")
        procs.append(a_proc)
        b_proc, b_ctl, b_ing, b_pw = start_server(nfrs, tmp, "b")
        procs.append(b_proc)

        A = Panel(a_ctl)
        B = Panel(b_ctl)
        check("login A", A.login(a_pw)[0] == 200)
        check("login B", B.login(b_pw)[0] == 200)

        # Enable REST API and mint admin keys on both A and B.
        check("A API enabled", A.json("/nfr/api/settings/api-toggle", method="POST", payload={"enabled": True})[0] == 200)
        check("B API enabled", B.json("/nfr/api/settings/api-toggle", method="POST", payload={"enabled": True})[0] == 200)

        st_ka, ka_resp = A.json("/nfr/api/api-keys", method="POST", payload={"label": "fed-a", "scope": "admin"})
        st_kb, kb_resp = B.json("/nfr/api/api-keys", method="POST", payload={"label": "fed-b", "scope": "admin"})
        key_a = ka_resp.get("key", "")
        key_b = kb_resp.get("key", "")
        check("A admin key created", st_ka == 201 and bool(key_a), "status %d" % st_ka)
        check("B admin key created", st_kb == 201 and bool(key_b), "status %d" % st_kb)
        if not key_a or not key_b:
            return 1

        apiA = RestClient(a_ctl, key_a)

        # Register B on A via /api/v1/servers with TOFU (empty pin).
        st, added = apiA.json(
            "/api/v1/servers",
            method="POST",
            payload={
                "url": "https://127.0.0.1:%d" % b_ctl,
                "api_key": key_b,
                "name": "Node B",
                "pin": "",
            },
        )
        b_id = added.get("id", "") if isinstance(added, dict) else ""
        check("A registered B via /api/v1/servers", st == 201 and bool(b_id), "status %d body=%s" % (st, added))
        if not b_id:
            return 1

        st, servers = apiA.json("/api/v1/servers")
        b_rec = next((s for s in (servers if isinstance(servers, list) else []) if s.get("id") == b_id), None)
        check("A lists B reachable", st == 200 and b_rec is not None and b_rec.get("reachable") is True, str(b_rec))

        # Join an agent to B.
        st, jc = B.json("/nfr/api/nodes/join-codes", method="POST", payload={"name": "node-b-agent"})
        code = jc.get("code", "")
        check("B join code issued", st == 201 and bool(code), "status %d" % st)
        if not code:
            return 1

        agent_cfg = os.path.join(tmp, "b-agent.toml")
        p = subprocess.run(
            [nfrc, "join", "--no-start", "-c", agent_cfg, "--name", "node-b-agent", "127.0.0.1:%d" % b_ctl, code],
            capture_output=True,
            text=True,
        )
        check("B agent joined", p.returncode == 0, (p.stderr or p.stdout).strip()[-200:])
        agent_log = open(os.path.join(tmp, "b-agent.log"), "wb")
        agent = subprocess.Popen([nfrc, "-c", agent_cfg], stdout=agent_log, stderr=subprocess.STDOUT)
        procs.append(agent)

        b_node = None
        end = time.time() + 20
        while time.time() < end:
            st_n, nodes = apiA.json("/api/v1/nodes?server=%s" % b_id)
            if st_n == 200 and isinstance(nodes, list):
                online = [n for n in nodes if n.get("online")]
                if online:
                    b_node = online[0]
                    break
            time.sleep(0.3)
        check("B agent visible online via A federation API", b_node is not None)
        if not b_node:
            return 1

        local = free_port()
        httpd = ThreadingHTTPServer(("127.0.0.1", local), LocalService)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()

        # Create a tunnel on B through A's federation REST API.
        st, created = apiA.json(
            "/api/v1/tunnels?server=%s" % b_id,
            method="POST",
            payload={
                "name": "on-b",
                "node_id": b_node["id"],
                "type": "http",
                "local": "127.0.0.1:%d" % local,
                "hosts": ["b.test"],
                "enabled": True,
            },
        )
        check("A created tunnel on B via REST", st == 201 and bool(created.get("id")), "status %d %s" % (st, created))

        ok = False
        last_st = 0
        for _ in range(40):
            try:
                last_st, body = http_get(b_ing, "b.test")
                if last_st == 200 and PAYLOAD in body:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(0.2)
        check("tunnel on B forwards traffic", ok, "status %d" % last_st)

        # Wrong pin must be rejected.
        st, err_body = apiA.json(
            "/api/v1/servers",
            method="POST",
            payload={
                "url": "https://127.0.0.1:%d" % b_ctl,
                "api_key": key_b,
                "name": "B wrong pin",
                "pin": "00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00",
            },
        )
        err_str = json.dumps(err_body)
        check(
            "wrong pin rejected",
            st >= 400 and re.search(r"fingerprint|mismatch|refused|certificate|pin", err_str, re.I) is not None,
            "status %d body=%s" % (st, err_str[:200]),
        )

        # Turning the API off on B must cause proxied calls from A to fail (502 remote_error).
        B.json("/nfr/api/settings/api-toggle", method="POST", payload={"enabled": False})
        st_off, body_off = apiA.json("/api/v1/info?server=%s" % b_id)
        check(
            "A reports B unreachable after API disabled",
            st_off == 502 and body_off.get("code") == "remote_error",
            "status %d body=%s" % (st_off, body_off),
        )

        st, _ = apiA.json("/api/v1/servers/%s" % b_id, method="DELETE")
        check("A deletes remote server B", st == 204, "status %d" % st)

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
