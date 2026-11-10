#!/usr/bin/env python3
"""Regenerate the panel screenshots used by README.md.

Starts a throwaway control node, joins one agent and creates a few tunnels so
the console has realistic content, logs in through the panel as a normal
browser would, then hands the session cookie to a headless Chromium and
captures each page. Output goes to docs/screenshots/.

Only the Python standard library is used on this side; the browser half lives
in scripts/shot.js and needs a Playwright install. Point SHOT_PLAYWRIGHT at
it when it is not resolvable by plain `require("playwright")`:

    python3 scripts/screenshots.py
    SHOT_PLAYWRIGHT=/path/to/playwright python3 scripts/screenshots.py --keep
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
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NFRS_BIN = os.environ.get("NFRS_BIN", os.path.join(ROOT, "bin", "nfrs"))
NFRC_BIN = os.environ.get("NFRC_BIN", os.path.join(ROOT, "bin", "nfrc"))
OUT = os.path.join(ROOT, "docs", "screenshots")


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def port_free(port):
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def wait_port(port, timeout=15.0):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return True
        except OSError:
            time.sleep(0.1)
    return False


class Panel:
    """Minimal panel client: keeps the session cookie and the CSRF token."""

    def __init__(self, control_port):
        self.base = "https://127.0.0.1:%d" % control_port
        self.jar = http.cookiejar.CookieJar()
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ctx),
            urllib.request.HTTPCookieProcessor(self.jar),
        )
        self.csrf = ""

    def request(self, path, form=None):
        data = urllib.parse.urlencode(form).encode() if form is not None else None
        req = urllib.request.Request(self.base + path, data=data)
        try:
            with self.opener.open(req, timeout=15) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def refresh_csrf(self, path):
        _, page = self.request(path)
        m = re.search(r'name="csrf" value="([^"]+)"', page)
        if m:
            self.csrf = m.group(1)
        return page

    def login(self, password):
        self.request("/nfr/login")
        status, _ = self.request("/nfr/login", {"password": password})
        self.refresh_csrf("/nfr/tunnels/new?server=local")
        return status

    def save_tunnel(self, fields, expect_ok=True):
        form = {"csrf": self.csrf, "server": "local", "enabled": "1"}
        form.update(fields)
        status, html = self.request("/nfr/tunnels/save", form)
        if status not in (200, 302, 303):
            raise SystemExit("save %r -> %d %s" % (fields.get("name"), status, html[:300]))
        _, page = self.request("/nfr/tunnels?server=local")
        m = re.search(r'class="flash (?:warn|err)[^"]*"[^>]*>([^<]+)<', page)
        if expect_ok and m:
            raise SystemExit("save %r rejected: %s" % (fields.get("name"), m.group(1).strip()))
        self.refresh_csrf("/nfr/tunnels/new?server=local")
        time.sleep(0.4)

    def cookie_header(self):
        return [{"name": c.name, "value": c.value} for c in self.jar]


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


def shoot(name, url, cookies, tmp, viewport=(1440, 900), full_page=False, dark=False):
    cookie_path = os.path.join(tmp, "cookies.json")
    with open(cookie_path, "w") as f:
        json.dump(cookies, f)
    env = dict(os.environ)
    env.update({
        "SHOT_URL": url,
        "SHOT_OUT": os.path.join(OUT, name + ".png"),
        "SHOT_COOKIES": cookie_path,
        "SHOT_VIEWPORT": "%dx%d" % viewport,
        "SHOT_FULL": "1" if full_page else "0",
        "SHOT_DARK": "1" if dark else "0",
    })
    print("  %-16s" % name, end="", flush=True)
    p = subprocess.run(
        ["node", os.path.join(ROOT, "scripts", "shot.js")],
        capture_output=True, text=True, env=env,
    )
    if p.returncode != 0:
        print("FAIL")
        print((p.stderr or p.stdout).strip()[-1500:])
        raise SystemExit(1)
    print("ok")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()

    for p in (NFRS_BIN, NFRC_BIN):
        if not os.path.exists(p):
            print("missing binary: " + p + " (run: make build)")
            return 2

    tmp = tempfile.mkdtemp(prefix="nfr-shots-")
    procs = []
    print("work dir " + tmp)

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), LocalService)
    local_port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()

    def cleanup():
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(5)
                except subprocess.TimeoutExpired:
                    p.kill()
        httpd.shutdown()
        httpd.server_close()
        if not args.keep:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        control = free_port() if not port_free(8443) else 8443
        ingress = free_port()
        cfg = os.path.join(tmp, "server.toml")
        with open(cfg, "w") as f:
            f.write(
                'control_listen = "127.0.0.1:%d"\n'
                'http_listen = "127.0.0.1:%d"\n'
                'https_listen = "off"\n'
                'data_dir = "%s"\n'
                'port_range = [20000, 29999]\n'
                'log_level = "info"\n' % (control, ingress, os.path.join(tmp, "data"))
            )

        server_log = open(os.path.join(tmp, "server.log"), "wb")
        env = dict(os.environ)
        env.update({"HTTPS_PROXY": "http://127.0.0.1:9", "HTTP_PROXY": "http://127.0.0.1:9",
                    "NO_PROXY": "127.0.0.1,localhost"})
        procs.append(subprocess.Popen(
            [NFRS_BIN, "--foreground", "-c", cfg],
            stdout=server_log, stderr=subprocess.STDOUT, env=env,
        ))
        if not wait_port(control):
            print("server never listened")
            return 1

        text = open(os.path.join(tmp, "server.log"), errors="replace").read()
        password = re.search(r"密码\s+(\S+)", text).group(1)

        panel = Panel(control)
        if panel.login(password) != 200:
            print("panel login failed")
            return 1
        page = panel.refresh_csrf("/nfr/settings")
        status, page = panel.request("/nfr/settings", {
            "csrf": panel.csrf,
            "name": "NFR",
            "domain": "example.com",
            "public_host": "203.0.113.10",
            "port_low": "20000",
            "port_high": "29999",
            "email": "ops@example.com",
        })
        if "保存" in page and "错误" in page:
            print("settings may not have saved")

        # One live agent, so the node page and the tunnel list look real.
        page = panel.refresh_csrf("/nfr/nodes")
        m = re.search(r'class="join-code">([0-9A-Z]+)<', page)
        if not m:
            # No code on the page yet: mint one through the form.
            status, page = panel.request("/nfr/nodes", {"csrf": panel.csrf, "name": "edge-1"})
            m = re.search(r'class="join-code">([0-9A-Z]+)<', page)
        if not m:
            print("could not obtain a join code")
            return 1
        code = m.group(1)

        agent_cfg = os.path.join(tmp, "agent.toml")
        subprocess.run(
            [NFRC_BIN, "join", "--no-start", "-c", agent_cfg, "--name", "edge-1",
             "127.0.0.1:%d" % control, code],
            capture_output=True, text=True,
        )
        agent_log = open(os.path.join(tmp, "agent.log"), "wb")
        procs.append(subprocess.Popen(
            [NFRC_BIN, "-c", agent_cfg], stdout=agent_log, stderr=subprocess.STDOUT
        ))
        # Wait for the agent to register, then bind every tunnel to its node.
        node_id = ""
        for _ in range(40):
            time.sleep(0.4)
            _, body = panel.request("/nfr/api/state")
            try:
                state = json.loads(body)
            except ValueError:
                continue
            nodes = state.get("nodes") or []
            if nodes:
                node_id = nodes[0]["id"]
                break
        if not node_id:
            print("agent never registered")
            return 1

        print("seeding tunnels…")
        # http: routed by Host header, the shape most people use for a web app
        panel.save_tunnel({"node_id": node_id, "name": "blog", "type": "http",
                           "local_host": "127.0.0.1", "local_port": str(local_port),
                           "hosts": "blog.example.com"})
        panel.save_tunnel({"node_id": node_id, "name": "ssh-box", "type": "tcp",
                           "local_host": "127.0.0.1", "local_port": str(local_port),
                           "remote_port": "20612"})
        panel.save_tunnel({"node_id": node_id, "name": "dns", "type": "udp",
                           "local_host": "127.0.0.1", "local_port": "53",
                           "remote_port": "20653"})
        # A tunnel waiting for its device: this is the card the README documents.
        panel.save_tunnel({"node_id": "new", "name": "homelab", "type": "http",
                           "local_host": "127.0.0.1", "local_port": "8080"},
                          expect_ok=False)

        # Push some bytes through the TCP tunnel so the counters are not all 0.
        print("driving traffic…")
        for _ in range(6):
            try:
                with socket.create_connection(("127.0.0.1", 20612), timeout=2) as s:
                    s.sendall(b"GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
                    s.recv(4096)
            except OSError:
                pass
            time.sleep(0.2)
        time.sleep(1.5)
        _, body = panel.request("/nfr/api/state")
        try:
            for t in json.loads(body).get("tunnels", []):
                print("   %-9s %-6s %s" % (t.get("name"), t.get("type"), t.get("status")))
        except ValueError:
            pass

        cookies = panel.cookie_header()
        base = panel.base
        print("rendering…")
        shoot("tunnels", base + "/nfr/tunnels?server=local", cookies, tmp, full_page=True)
        shoot("tunnels-dark", base + "/nfr/tunnels?server=local", cookies, tmp, dark=True,
              full_page=True)
        shoot("tunnels-phone", base + "/nfr/tunnels?server=local", cookies, tmp,
              viewport=(390, 844), full_page=True)
        shoot("tunnel-new", base + "/nfr/tunnels/new?server=local", cookies, tmp,
              viewport=(1440, 1000))
        shoot("nodes", base + "/nfr/nodes", cookies, tmp)
        shoot("servers", base + "/nfr/servers", cookies, tmp, viewport=(1440, 760))
        shoot("settings", base + "/nfr/settings", cookies, tmp, viewport=(1440, 1500))
        print("wrote screenshots to " + OUT)
    finally:
        cleanup()


if __name__ == "__main__":
    sys.exit(main())
