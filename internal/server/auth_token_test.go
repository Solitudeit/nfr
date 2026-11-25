package server_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
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
