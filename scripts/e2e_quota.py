#!/usr/bin/env python3
"""End-to-end test for bandwidth limits and monthly traffic quotas.

Starts a temporary server, joins an agent and drives a TCP tunnel through the
throttled data plane, then burns through a quota and watches the guard pause
and resume the tunnel:

    visitor -> public port -> token bucket -> yamux stream -> agent -> echo

    python3 scripts/e2e_quota.py [--keep]
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

    def request(self, path, form=None, headers=None):
        data = urllib.parse.urlencode(form).encode() if form is not None else None
        req = urllib.request.Request(self.base + path, data=data, headers=headers or {})
        try:
            with self.opener.open(req, timeout=15) as resp:
                return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def login(self, password):
        self.request("/nfr/login")
        status, _ = self.request("/nfr/login", {"password": password})
        _, page = self.request("/nfr/nodes")
        m = re.search(r'name="csrf" value="([^"]+)"', page)
        self.csrf = m.group(1) if m else ""
        return status


def echo_server(port):
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen(16)

    def loop():
        while True:
            try:
                conn, _ = srv.accept()
            except OSError:
                return
            threading.Thread(target=pipe, args=(conn,), daemon=True).start()

    def pipe(conn):
        try:
            total = 0
            conn.settimeout(10)
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                conn.sendall(chunk)
                total += len(chunk)
        except OSError:
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass

    threading.Thread(target=loop, daemon=True).start()
    return srv


def fetch(port, size):
    """Pull `size` bytes through the tunnel, return elapsed seconds."""
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    s.settimeout(60)
    start = time.time()
    payload = b"x" * 65536
    sent = 0
    while sent < size:
        n = s.send(payload[: min(len(payload), size - sent)])
        sent += n
    s.shutdown(socket.SHUT_WR)
    got = 0
    while got < size:
        chunk = s.recv(65536)
        if not chunk:
            break
        got += len(chunk)
    elapsed = time.time() - start
    s.close()
    return got, elapsed


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

    tmp = tempfile.mkdtemp(prefix="nfr-e2e-quota-")
    procs = []
    echo_srv = None
    print("work dir " + tmp)

    def cleanup():
        for p in procs:
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(5)
                except subprocess.TimeoutExpired:
                    p.kill()
        if echo_srv:
            echo_srv.close()
        if args.keep or failures:
            print("logs kept in " + tmp)
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    try:
        control = free_port()
        echo_port = free_port()

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
        status = panel.login(password)
        check("panel login", status == 200)

        status, html = panel.request("/nfr/nodes", {"csrf": panel.csrf, "name": "testnode"})
        m = re.search(r'class="join-code">([0-9A-Z]+)<', html)
        if not m:
            check("join code issued", False, "status %d" % status)
            return 1
        code = m.group(1)

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

        echo_srv = echo_server(echo_port)

        def save_tunnel(name, **extra):
            form = {
                "csrf": panel.csrf, "server": "local", "name": name,
                "node_id": node["id"], "type": "tcp",
                "local": "127.0.0.1:%d" % echo_port, "enabled": "1",
            }
            form.update(extra)
            return panel.request("/nfr/tunnels/save", form)

        def find_tunnel(name):
            _, body = panel.request("/nfr/api/state")
            return next((t for t in json.loads(body)["tunnels"] if t["name"] == name), None)

        # ------------------------------------------------------------------
        # Part 1: bandwidth limit paces the data plane.
        status, _ = save_tunnel("throttled", bandwidth_kb="512")
        check("create throttled tunnel", status == 200, "status %d" % status)
        tun = find_tunnel("throttled")
        if not tun:
            check("find throttled tunnel", False)
            return 1
        check("bandwidth stored", tun.get("bandwidth_kb") == 512, str(tun)[:150])
        time.sleep(1.5)  # let the agent pick up the tunnel

        got, elapsed = fetch(tun["remote_port"], 2 << 20)  # 2 MB each way
        check("throttled transfer complete", got == 2 << 20, "got %d" % got)

        # Unlimited control tunnel on the same echo: same transfer, no pacing.
        status, _ = save_tunnel("fastlane")
        check("create unlimited control tunnel", status == 200, "status %d" % status)
        ctl = find_tunnel("fastlane")
        time.sleep(1.5)
        got, fast = fetch(ctl["remote_port"], 2 << 20)
        check("control transfer complete", got == 2 << 20, "got %d" % got)

        check("control is fast", fast < 1.5, "%.2fs" % fast)
        check("bandwidth limit paces throughput",
              elapsed >= 1.8 and elapsed > fast * 3,
              "throttled %.2fs vs control %.2fs" % (elapsed, fast))

        # ------------------------------------------------------------------
        # Part 2: quota guard pauses, month rollover resumes.
        status, _ = save_tunnel("metered", quota_mb="3")
        check("create metered tunnel", status == 200, "status %d" % status)
        tun = find_tunnel("metered")
        if not tun:
            check("find metered tunnel", False)
            return 1
        check("quota stored", tun.get("quota_mb") == 3, str(tun)[:150])
        time.sleep(1.5)

        got, _ = fetch(tun["remote_port"], 1 << 20)  # in 1MB + out 1MB = 2MB
        check("metered transfer under quota", got == 1 << 20, "got %d" % got)
        time.sleep(35)  # slow ticker folds usage into the store every 30s

        tun = find_tunnel("metered")
        check("usage persisted", tun.get("used_bytes", 0) >= 2 << 20, "used=%s" % tun.get("used_bytes"))
        check("under-quota tunnel still enabled", tun.get("enabled") is True, str(tun)[:150])

        fetch(tun["remote_port"], 1 << 20)  # +2MB -> 4MB total, over the 3MB quota
        paused = False
        end = time.time() + 75
        while time.time() < end:
            tun = find_tunnel("metered")
            if tun and not tun.get("enabled") and tun.get("quota_paused"):
                paused = True
                break
            time.sleep(2)
        check("over-quota tunnel auto-paused", paused, str(tun)[:200])

        # The paused tunnel no longer forwards.
        refused = False
        try:
            fetch(tun["remote_port"], 1 << 16)
        except OSError:
            refused = True
        check("paused tunnel refuses traffic", refused)

        # The panel marks the pause.
        _, html = panel.request("/nfr/tunnels?server=local")
        check("panel shows quota pause", "配额停用" in html)

        # Clearing the quota and re-enabling restores service.
        status, _ = save_tunnel(
            "metered", id=tun["id"], remote_port=str(tun["remote_port"]), quota_mb="0"
        )
        check("edit tunnel clears quota", status == 200, "status %d" % status)
        panel.request("/nfr/tunnels/toggle",
                      {"csrf": panel.csrf, "server": "local", "id": tun["id"], "enabled": "true"})
        ok = False
        end = time.time() + 10
        while time.time() < end:
            try:
                got, _ = fetch(tun["remote_port"], 1 << 20)
                if got == 1 << 20:
                    ok = True
                    break
            except OSError:
                pass
            time.sleep(1)
        check("resumed tunnel forwards again", ok)

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
