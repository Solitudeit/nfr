package server_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

func newStandaloneTestServer(t *testing.T) (*server.Server, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	ctlPort := freePort(t)
	cfg := config.DefaultServer()
	cfg.ControlListen = "127.0.0.1:" + itoa(ctlPort)
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.DataDir = dir
	cfg.PortRange = [2]int{20000, 29999}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Run(ctx) }()
	waitListen(t, cfg.ControlListen)
	return srv, cfg.ControlListen, cancel
}

func startEchoServer(t *testing.T, reply string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.WriteString(conn, reply)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func dialReply(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestStandaloneTunnelLifecycleAndAuthoritativeEdit(t *testing.T) {
	srv, ctlAddr, cancel := newStandaloneTestServer(t)
	defer cancel()

	echo1, stop1 := startEchoServer(t, "svc-v1")
	defer stop1()
	echo2, stop2 := startEchoServer(t, "svc-v2")
	defer stop2()

	en := true
	tun1, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name:    "app1",
		Type:    "tcp",
		Local:   echo1,
		Enabled: &en,
	})
	if err != nil {
		t.Fatalf("CreateStandaloneTunnel 1: %v", err)
	}
	if tun1.Online || tun1.NodeID != "" || tun1.RemotePort == 0 {
		t.Fatalf("unexpected initial standalone tunnel: %+v", tun1)
	}

	tun2, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name:    "app2",
		Type:    "tcp",
		Local:   echo2,
		Enabled: &en,
	})
	if err != nil {
		t.Fatalf("CreateStandaloneTunnel 2: %v", err)
	}

	// Start two separate per-tunnel agents concurrently; neither may kick the other.
	a1Ctx, a1Cancel := context.WithCancel(context.Background())
	defer a1Cancel()
	ag1 := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun1.ID,
		Pin:       srv.Pin(),
		Proxies: []proto.ProxySpec{{
			Name: tun1.Name, Type: tun1.Type, Local: tun1.Local, RemotePort: tun1.RemotePort,
		}},
		Logger: log.New("error", ""),
	})
	go ag1.Run(a1Ctx)

	a2Ctx, a2Cancel := context.WithCancel(context.Background())
	defer a2Cancel()
	ag2 := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun2.ID,
		Pin:       srv.Pin(),
		Proxies: []proto.ProxySpec{{
			Name: tun2.Name, Type: tun2.Type, Local: tun2.Local, RemotePort: tun2.RemotePort,
		}},
		Logger: log.New("error", ""),
	})
	go ag2.Run(a2Ctx)

	// Wait until both tunnels are online simultaneously.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v1, _ := srv.Tunnel(tun1.ID)
		v2, _ := srv.Tunnel(tun2.ID)
		if v1.Online && v2.Online {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	v1, _ := srv.Tunnel(tun1.ID)
	v2, _ := srv.Tunnel(tun2.ID)
	if !v1.Online || !v2.Online {
		t.Fatalf("expected both standalone tunnels online concurrently: v1=%v v2=%v", v1.Online, v2.Online)
	}

	// Verify real traffic on tun1.
	pub1 := "127.0.0.1:" + itoa(tun1.RemotePort)
	var got string
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := dialReply(pub1); err == nil && s == "svc-v1" {
			got = s
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if got != "svc-v1" {
		t.Fatalf("expected svc-v1 from tun1, got %q", got)
	}

	// Edit tun1 in panel to point to echo2, then reconnect ag1 with the STALE local=echo1 config.
	// Panel must stay authoritative!
	if _, err := srv.UpdateTunnel(tun1.ID, api.TunnelRequest{Local: echo2}); err != nil {
		t.Fatalf("UpdateTunnel: %v", err)
	}
	a1Cancel()
	time.Sleep(150 * time.Millisecond)

	a1Ctx2, a1Cancel2 := context.WithCancel(context.Background())
	defer a1Cancel2()
	ag1b := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun1.ID,
		Pin:       srv.Pin(),
		Proxies: []proto.ProxySpec{{
			Name: tun1.Name, Type: tun1.Type, Local: echo1, RemotePort: tun1.RemotePort, // stale!
		}},
		Logger: log.New("error", ""),
	})
	go ag1b.Run(a1Ctx2)

	got = ""
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := dialReply(pub1); err == nil && s == "svc-v2" {
			got = s
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "svc-v2" {
		t.Fatalf("panel edit should stay authoritative across stale client reconnect, got %q", got)
	}
}

func TestKickTunnelStopsAgentAndKeepsOffline(t *testing.T) {
	srv, ctlAddr, cancel := newStandaloneTestServer(t)
	defer cancel()

	en := true
	tun, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name: "kickme", Type: "tcp", Local: "127.0.0.1:18080", Enabled: &en,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	aCtx, aCancel := context.WithCancel(context.Background())
	defer aCancel()
	ag := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun.ID,
		Pin:       srv.Pin(),
		Proxies:   []proto.ProxySpec{{Name: tun.Name, Type: tun.Type, Local: tun.Local, RemotePort: tun.RemotePort}},
		Logger:    log.New("error", ""),
	})
	done := make(chan struct{})
	go func() {
		ag.Run(aCtx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := srv.Tunnel(tun.ID); ok && v.Online {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	kicked, err := srv.KickTunnel(tun.ID)
	if err != nil || !kicked {
		t.Fatalf("KickTunnel: kicked=%v err=%v", kicked, err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not exit after KickTunnel")
	}

	time.Sleep(150 * time.Millisecond)
	if v, ok := srv.Tunnel(tun.ID); !ok || v.Online {
		t.Fatalf("expected kicked tunnel to stay offline, got ok=%v online=%v", ok, v.Online)
	}
}

func TestDeleteStandaloneTunnelCleansNodeAndBlocksResurrection(t *testing.T) {
	srv, ctlAddr, cancel := newStandaloneTestServer(t)
	defer cancel()

	en := true
	tun, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name: "delme", Type: "tcp", Local: "127.0.0.1:18081", Enabled: &en,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	aCtx, aCancel := context.WithCancel(context.Background())
	defer aCancel()
	ag := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun.ID,
		Pin:       srv.Pin(),
		Proxies:   []proto.ProxySpec{{Name: tun.Name, Type: tun.Type, Local: tun.Local, RemotePort: tun.RemotePort}},
		Logger:    log.New("error", ""),
	})
	done := make(chan struct{})
	go func() {
		ag.Run(aCtx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := srv.Tunnel(tun.ID); ok && v.Online {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if err := srv.DeleteTunnel(tun.ID); err != nil {
		t.Fatalf("DeleteTunnel: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deleting online standalone tunnel did not stop its agent")
	}

	for _, n := range srv.Nodes() {
		if n.Name == "t-"+tun.ID {
			t.Fatalf("synthetic node %q was not cleaned up on delete", n.Name)
		}
	}

	// Attempt to reconnect with the deleted tunnel's config: must be rejected and NOT resurrect the tunnel.
	rCtx, rCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer rCancel()
	ag2 := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun.ID,
		Pin:       srv.Pin(),
		Proxies:   []proto.ProxySpec{{Name: tun.Name, Type: tun.Type, Local: tun.Local, RemotePort: tun.RemotePort}},
		Logger:    log.New("error", ""),
	})
	ag2.Run(rCtx)

	if len(srv.Tunnels()) != 0 {
		t.Fatalf("deleted tunnel resurrected! tunnels=%+v", srv.Tunnels())
	}
}

func TestStandaloneTunnelClaimedByCustomNodeName(t *testing.T) {
	srv, ctlAddr, cancel := newStandaloneTestServer(t)
	defer cancel()

	en := true
	tun, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name: "ces", Type: "tcp", Local: "127.0.0.1:18082", Enabled: &en,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	aCtx, aCancel := context.WithCancel(context.Background())
	defer aCancel()
	ag := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "custom-phone",
		Pin:       srv.Pin(),
		Proxies:   []proto.ProxySpec{{Name: "ces", Type: "tcp", Local: tun.Local, RemotePort: tun.RemotePort}},
		Logger:    log.New("error", ""),
	})
	go ag.Run(aCtx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := srv.Tunnel(tun.ID); ok && v.Online {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	v, ok := srv.Tunnel(tun.ID)
	if !ok || !v.Online {
		t.Fatalf("expected standalone tunnel %q to be claimed online by custom-name client, got ok=%v online=%v tunnels=%+v",
			tun.Name, ok, v.Online, srv.Tunnels())
	}
	if len(srv.Tunnels()) != 1 {
		t.Fatalf("expected 1 tunnel (claimed in place), got %d", len(srv.Tunnels()))
	}
}
