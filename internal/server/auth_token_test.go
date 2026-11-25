package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/panel"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

func TestAuthTokenAndDeclarativeProxiesViaYAML(t *testing.T) {
	dir := t.TempDir()

	// Pick free ports for control, local backend echo, and public tunnel.
	ctlPort := freePort(t)
	pubPort := freePort(t)

	// Start a local echo server simulating an inner service.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// Write server.yaml and load it through config.LoadServer.
	srvYAML := filepath.Join(dir, "server.yaml")
	srvCfg := config.DefaultServer()
	srvCfg.ControlListen = "127.0.0.1:" + itoa(ctlPort)
	srvCfg.HTTPListen = "off"
	srvCfg.HTTPSListen = "off"
	srvCfg.DataDir = filepath.Join(dir, "data")
	srvCfg.AuthToken = "yaml-shared-secret"
	srvCfg.PortRange = [2]int{pubPort, pubPort + 10}
	if err := srvCfg.Save(srvYAML); err != nil {
		t.Fatalf("save server.yaml: %v", err)
	}
	loadedSrv, ok, err := config.LoadServer(srvYAML)
	if err != nil || !ok {
		t.Fatalf("load server.yaml: %v ok=%v", err, ok)
	}

	st, err := store.Open(loadedSrv.DataDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, _, err := server.New(server.Options{Config: loadedSrv, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	waitListen(t, loadedSrv.ControlListen)

	// Write client.yaml declaring auth_token + one TCP proxy.
	cliYAML := filepath.Join(dir, "client.yaml")
	rawClient := "server_addr: \"" + loadedSrv.ControlListen + "\"\n" +
		"auth_token: \"yaml-shared-secret\"\n" +
		"name: \"nas-box\"\n" +
		"proxies:\n" +
		"  - name: echo-tcp\n" +
		"    type: tcp\n" +
		"    local: \"" + echoLn.Addr().String() + "\"\n" +
		"    remote_port: " + itoa(pubPort) + "\n"
	if err := os.WriteFile(cliYAML, []byte(rawClient), 0o600); err != nil {
		t.Fatal(err)
	}
	cliCfg, ok, err := config.LoadAgent(cliYAML)
	if err != nil || !ok {
		t.Fatalf("load client.yaml: %v ok=%v", err, ok)
	}
	if err := cliCfg.Validate(); err != nil {
		t.Fatalf("validate client.yaml: %v", err)
	}
	target := cliCfg.EffectiveServers()[0]
	var specs []proto.ProxySpec
	for _, p := range target.Proxies {
		specs = append(specs, proto.ProxySpec{
			Name: p.Name, Type: p.Type, Local: p.EffectiveLocal(), RemotePort: p.RemotePort,
		})
	}

	// Connect agent #1 with auth_token (zero join codes!).
	a1Ctx, a1Cancel := context.WithCancel(ctx)
	ag1 := agent.New(agent.Options{
		Addr:      target.Addr,
		AuthToken: target.AuthToken,
		Name:      target.Name,
		Proxies:   specs,
		Logger:    log.New("error", ""),
	})
	go ag1.Run(a1Ctx)

	// Wait for node "nas-box" online and tunnel ready.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes := srv.Nodes()
		tunnels := srv.Tunnels()
		if len(nodes) == 1 && nodes[0].Online && len(tunnels) == 1 && tunnels[0].Name == "echo-tcp" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if nodes := srv.Nodes(); len(nodes) != 1 || !nodes[0].Online || nodes[0].Name != "nas-box" {
		t.Fatalf("expected 1 online node nas-box, got %+v", nodes)
	}

	// Verify real TCP traffic flows end-to-end through the declared proxy.
	waitListen(t, "127.0.0.1:"+itoa(pubPort))
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(pubPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial public tunnel: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	msg := "hello-yaml-nfr"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo = %q, want %q", string(buf), msg)
	}

	// Disconnect and reconnect with the exact same name -> must reuse node, no duplicates!
	firstID := srv.Nodes()[0].ID
	a1Cancel()
	time.Sleep(150 * time.Millisecond)

	a2Ctx, a2Cancel := context.WithCancel(ctx)
	defer a2Cancel()
	ag2 := agent.New(agent.Options{
		Addr:      target.Addr,
		AuthToken: target.AuthToken,
		Name:      target.Name,
		Proxies:   specs,
		Logger:    log.New("error", ""),
	})
	go ag2.Run(a2Ctx)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes := srv.Nodes()
		if len(nodes) == 1 && nodes[0].Online && nodes[0].ID == firstID {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if nodes := srv.Nodes(); len(nodes) != 1 || nodes[0].ID != firstID || !nodes[0].Online {
		t.Fatalf("reconnect duplicated or failed to reuse node: %+v (want id %s)", nodes, firstID)
	}

	// Create a manual tunnel on the same node (ManagedByConfig=false), then
	// reconnect with an EMPTY proxies list -> config-managed "echo-tcp" must be
	// pruned while the manual tunnel MUST survive untouched!
	if _, err := srv.CreateTunnel(apiTunnelReq("manual-web", firstID, echoLn.Addr().String())); err != nil {
		t.Fatalf("CreateTunnel manual: %v", err)
	}
	a2Cancel()
	time.Sleep(150 * time.Millisecond)

	a3Ctx, a3Cancel := context.WithCancel(ctx)
	defer a3Cancel()
	ag3 := agent.New(agent.Options{
		Addr:      target.Addr,
		AuthToken: target.AuthToken,
		Name:      target.Name,
		Proxies:   nil, // removed all declarative proxies
		Logger:    log.New("error", ""),
	})
	go ag3.Run(a3Ctx)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tunnels := srv.Tunnels()
		if len(tunnels) == 1 && tunnels[0].Name == "manual-web" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	tunnels := srv.Tunnels()
	if len(tunnels) != 1 || tunnels[0].Name != "manual-web" {
		t.Fatalf("expected only manual-web after pruning config proxies, got %+v", tunnels)
	}
}

func apiTunnelReq(name, nodeID, local string) api.TunnelRequest {
	en := true
	return api.TunnelRequest{
		Name:    name,
		NodeID:  nodeID,
		Type:    "tcp",
		Local:   local,
		Enabled: &en,
	}
}

func TestPanelDefaultPlainHTTPAndOptInHTTPS(t *testing.T) {
	// 1) Default (panel.https = false): browser talks plain HTTP to control_listen
	//    with zero TLS cert warning, login cookie works over HTTP, AND TLS 1.3
	//    agents still connect on the exact same port.
	dir1 := t.TempDir()
	p1 := freePort(t)
	cfg1 := config.DefaultServer()
	cfg1.ControlListen = "127.0.0.1:" + itoa(p1)
	cfg1.HTTPListen = "off"
	cfg1.HTTPSListen = "off"
	cfg1.DataDir = dir1
	st1, err := store.Open(dir1)
	if err != nil {
		t.Fatal(err)
	}
	srv1, pw1, err := server.New(server.Options{Config: cfg1, Store: st1, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	srv1.SetPanel(panel.New(srv1, log.New("error", "")))
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() { _ = srv1.Run(ctx1) }()
	waitListen(t, cfg1.ControlListen)

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	baseHTTP := "http://" + cfg1.ControlListen

	resp, err := hc.PostForm(baseHTTP+"/nfr/login", url.Values{"password": {pw1}})
	if err != nil {
		t.Fatalf("plain HTTP login failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(bodyBytes), `data-stat="nodes-online"`) {
		t.Fatalf("plain HTTP login did not land on authenticated tunnels page: status=%d", resp.StatusCode)
	}

	// 2) Opt-in HTTPS (panel.https = true): plain HTTP redirects 308 -> https://.
	dir2 := t.TempDir()
	p2 := freePort(t)
	cfg2 := config.DefaultServer()
	cfg2.ControlListen = "127.0.0.1:" + itoa(p2)
	cfg2.HTTPListen = "off"
	cfg2.HTTPSListen = "off"
	cfg2.DataDir = dir2
	cfg2.Panel.HTTPS = true
	st2, err := store.Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	srv2, _, err := server.New(server.Options{Config: cfg2, Store: st2, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	srv2.SetPanel(panel.New(srv2, log.New("error", "")))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = srv2.Run(ctx2) }()
	waitListen(t, cfg2.ControlListen)

	noRedir := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	r2, err := noRedir.Get("http://" + cfg2.ControlListen + "/nfr/login")
	if err != nil {
		t.Fatalf("GET on https-only panel failed: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusPermanentRedirect || !strings.HasPrefix(r2.Header.Get("Location"), "https://") {
		t.Fatalf("expected 308 redirect to https://, got %d %q", r2.StatusCode, r2.Header.Get("Location"))
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func itoa(n int) string {
	return net.JoinHostPort("", string(rune(0)))[:0] + func() string {
		var b [20]byte
		i := len(b)
		if n == 0 {
			return "0"
		}
		for n > 0 {
			i--
			b[i] = byte('0' + n%10)
			n /= 10
		}
		return string(b[i:])
	}()
}

func waitListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never started listening", addr)
}
