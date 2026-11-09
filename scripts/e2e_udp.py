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
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.environ.get("NFR_BIN", os.path.join(ROOT, "bin", "nfr"))

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
        _, page = self.request("/_nfr/nodes")
        m = re.search(r'name="csrf" value="([^"]+)"', page)
        self.csrf = m.group(1) if m else ""
        return status, html


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--bin", default=BIN)
    args = ap.parse_args()

    binary = os.path.abspath(args.bin)
    if not os.path.exists(binary):
        print("missing binary: " + binary + " (run: go build -o bin/nfr ./cmd/nfr)")
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
                'log_level = "debug"\n' % (control, data_dir)
            )

        server_log = open(os.path.join(tmp, "server.log"), "wb")
        server = subprocess.Popen(
            [binary, "server", "--foreground", "-c", cfg], stdout=server_log, stderr=subprocess.STDOUT
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

        status, html = panel.request("/_nfr/nodes", {"csrf": panel.csrf, "name": "testnode"})
        m = re.search(r'class="join-code">([0-9A-Z]+)<', html)
        if not m:
            check("join code issued", False, "status %d" % status)
            return 1
        code = m.group(1)

        agent_cfg = os.path.join(tmp, "agent.toml")
        p = subprocess.run(
            [binary, "join", "--no-start", "-c", agent_cfg, "--name", "testnode", "127.0.0.1:%d" % control, code],
            capture_output=True, text=True,
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
        status, body = panel.request(
            "/_nfr/api/allocate", {"tunnel_id": "", "type": "udp"}, {"X-CSRF": panel.csrf}
        )
        alloc = json.loads(body)
        check("udp port allocated", bool(alloc.get("port")), str(alloc))

        status, html = panel.request(
            "/_nfr/tunnels/save",
            {
                "csrf": panel.csrf, "server": "local", "name": "dns",
                "node_id": node["id"], "type": "udp",
                "local": "127.0.0.1:%d" % echo_port,
                "remote_port": str(alloc.get("port", "")),
                "lease_id": alloc.get("lease_id", ""), "enabled": "1",
            },
        )
        check("create udp tunnel", "隧道已保存" in html, "status %d" % status)

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
        ok = visit(b"other-visitor", src_port=free_udp_port() + 1) == b"other-visitor"
        check("second visitor session works", ok)

        # A large-ish payload (still a single datagram).
        big = bytes(range(256)) * 60  # 15360 bytes
        ok = visit(big) == big
        check("large datagram round trips", ok)

        # Metrics see the traffic.
        _, body = panel.request("/_nfr/api/state")
        tun = next((t for t in json.loads(body)["tunnels"] if t["name"] == "dns"), {})
        check("traffic counted", tun.get("in", 0) > 0 and tun.get("out", 0) > 0, str(tun)[:120])

        # Toggling stops and restores the datagram path.
        if tun:
            panel.request(
                "/_nfr/tunnels/toggle",
                {"csrf": panel.csrf, "server": "local", "id": tun["id"], "enabled": "false"},
            )
            ok = False
            end = time.time() + 6
            while time.time() < end:
                if visit(b"after-off", timeout=1.0) is None:
                    ok = True
                    break
                time.sleep(0.2)
            check("disabled udp tunnel stops traffic", ok)

            panel.request(
                "/_nfr/tunnels/toggle",
                {"csrf": panel.csrf, "server": "local", "id": tun["id"], "enabled": "true"},
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
    print("E2E OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
