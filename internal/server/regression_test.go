package server

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

func newInternalTestServer(t *testing.T, mut func(*config.Server)) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	if mut != nil {
		mut(&cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestSyncDeclarativeProxiesDoesNotHijackOtherNode(t *testing.T) {
	srv := newInternalTestServer(t, nil)
	if err := srv.st.Update(func(st *store.State) error {
		st.Nodes = append(st.Nodes,
			store.Node{ID: "nodeA", Name: "alpha"},
			store.Node{ID: "nodeB", Name: "beta"},
		)
		st.Tunnels = append(st.Tunnels, store.Tunnel{
			ID: "t-ssh", Name: "ssh", NodeID: "", Type: store.TunnelTCP,
			Local: "127.0.0.1:22", RemotePort: 25022, Enabled: true,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Unregistered proxy MUST be rejected and NEVER create a tunnel.
	if err := srv.syncDeclarativeProxies("nodeA", []proto.ProxySpec{
		{Name: "ghost-tunnel", Type: "tcp", Local: "127.0.0.1:9999"},
	}, true); err == nil {
		t.Fatal("expected error when client declares tunnel not present in server panel")
	}
	if len(srv.Tunnels()) != 1 {
		t.Fatalf("ghost tunnel was created: %+v", srv.Tunnels())
	}

	// Node A binds to existing panel tunnel "ssh".
	if err := srv.syncDeclarativeProxies("nodeA", []proto.ProxySpec{
		{Name: "ssh", Type: "tcp", Local: "127.0.0.1:22"},
	}, true); err != nil {
		t.Fatalf("nodeA bind failed: %v", err)
	}

	// Node B declaring the same proxy name MUST fail with a name conflict
	// instead of silently stealing node A's tunnel.
	err := srv.syncDeclarativeProxies("nodeB", []proto.ProxySpec{
		{Name: "ssh", Type: "tcp", Local: "127.0.0.1:2222"},
	}, true)
	if err == nil {
		t.Fatal("expected conflict when nodeB declares a proxy name owned by nodeA, got nil")
	}

	tunnels := srv.Tunnels()
	if len(tunnels) != 1 || tunnels[0].NodeID != "nodeA" || tunnels[0].Local != "127.0.0.1:22" {
		t.Fatalf("nodeA tunnel was hijacked or corrupted: %+v", tunnels)
	}
}

func TestSyncDeclarativeProxiesPreservesQuotaPausedAndDisabled(t *testing.T) {
	srv := newInternalTestServer(t, nil)
	if err := srv.st.Update(func(st *store.State) error {
		st.Nodes = append(st.Nodes, store.Node{ID: "nodeA", Name: "alpha"})
		st.Tunnels = append(st.Tunnels, store.Tunnel{
			ID: "t-media", Name: "media", NodeID: "nodeA", Type: store.TunnelTCP,
			Local: "127.0.0.1:8096", RemotePort: 25096, Enabled: false, QuotaMB: 1,
			QuotaPaused: true, UsedBytes: 2 << 20, UsageMonth: time.Now().UTC().Format("2006-01"),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Client reconnects with any QuotaMB value -> panel's QuotaPaused/Enabled
	// MUST remain strictly authoritative and cannot be bypassed by client config!
	spec := []proto.ProxySpec{{Name: "media", Type: "tcp", Local: "127.0.0.1:8096", QuotaMB: 999}}
	if err := srv.syncDeclarativeProxies("nodeA", spec, true); err != nil {
		t.Fatalf("reconnect sync: %v", err)
	}
	got, _ := srv.Tunnel("t-media")
	if got.Enabled || !got.QuotaPaused || got.QuotaMB != 1 {
		t.Fatalf("client config bypassed panel quota/pause: enabled=%v quota_paused=%v quota_mb=%d", got.Enabled, got.QuotaPaused, got.QuotaMB)
	}
}

func TestFlushQuotasDoesNotDoubleCountAfterPause(t *testing.T) {
	srv := newInternalTestServer(t, nil)
	if err := srv.st.Update(func(st *store.State) error {
		st.Nodes = append(st.Nodes, store.Node{ID: "n1", Name: "edge"})
		st.Tunnels = append(st.Tunnels, store.Tunnel{
			ID: "t1", Name: "limited", NodeID: "n1", Type: store.TunnelTCP,
			Local: "127.0.0.1:80", RemotePort: 25001, Enabled: true, QuotaMB: 1,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate 1.5 MiB transferred through the tunnel.
	const sent = (1 << 20) + (512 << 10)
	srv.met.For("t1").In.Add(sent)

	// First flush pauses the tunnel and records 1.5 MiB.
	srv.flushQuotas()
	got, _ := srv.Tunnel("t1")
	if got.Enabled || !got.QuotaPaused {
		t.Fatalf("expected tunnel quota-paused after first flush: %+v", got)
	}
	if got.UsedBytes != sent {
		t.Fatalf("UsedBytes after 1st flush = %d, want %d", got.UsedBytes, sent)
	}

	// Second flush with zero additional traffic MUST NOT double-count!
	srv.flushQuotas()
	got, _ = srv.Tunnel("t1")
	if got.UsedBytes != sent {
		t.Fatalf("UsedBytes double-counted after 2nd flush: got %d, want %d", got.UsedBytes, sent)
	}
}

func TestJoinAddrAlwaysIncludesPort443(t *testing.T) {
	srv := newInternalTestServer(t, func(cfg *config.Server) {
		cfg.ControlListen = ":443"
		cfg.Domain = "tunnel.example.com"
	})
	if got := srv.JoinAddr(); got != "tunnel.example.com:443" {
		t.Fatalf("JoinAddr() = %q, want %q", got, "tunnel.example.com:443")
	}
}

func TestReconcileClosesActiveUDPSessionsOnDisable(t *testing.T) {
	srv := newInternalTestServer(t, nil)
	tun := store.Tunnel{
		ID: "u1", Name: "dns", NodeID: "n1", Type: store.TunnelUDP,
		Local: "127.0.0.1:53", RemotePort: 25053, Enabled: false,
	}
	if err := srv.st.Update(func(st *store.State) error {
		st.Tunnels = append(st.Tunnels, tun)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000}
	sess := &udpSession{
		srv:    srv,
		tunnel: tun,
		src:    addr,
		stream: c1,
		ctr:    srv.met.For(tun.ID),
		sendCh: make(chan udpPacket, udpSendQueueCap),
		done:   make(chan struct{}),
		last:   time.Now(),
	}
	srv.udpSess[tun.ID+"|"+addr.String()] = sess

	srv.reconcile()

	srv.mu.Lock()
	remaining := len(srv.udpSess)
	srv.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected disabled UDP tunnel sessions to be closed by reconcile, remaining=%d", remaining)
	}
}

func TestAddRemoteHandlesDefaultHTTPPanelURLWithNFRPath(t *testing.T) {
	// Start a real control server with default panel.https=false (plain HTTP + TLS mux).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctlAddr := ln.Addr().String()
	ln.Close()

	remote := newInternalTestServer(t, func(cfg *config.Server) {
		cfg.ControlListen = ctlAddr
		cfg.Panel.HTTPS = false
	})
	if err := remote.ToggleAPI(true); err != nil {
		t.Fatal(err)
	}
	keyResp, err := remote.CreateAPIKey(api.APIKeyRequest{Label: "fed", Scope: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = remote.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", ctlAddr, 100*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	local := newInternalTestServer(t, nil)
	// Operator pastes the default panel URL copied from browser/banner: http://host:port/nfr/
	rec, err := local.AddRemote(ctx, api.RemoteRequest{
		Name:   "peer-http-url",
		URL:    "http://" + ctlAddr + "/nfr/",
		APIKey: keyResp.Key,
	})
	if err != nil {
		t.Fatalf("AddRemote with default http://.../nfr/ URL failed: %v", err)
	}
	if !strings.HasPrefix(rec.URL, "https://") {
		t.Fatalf("AddRemote should upgrade direct NFR control port to https://, got %q", rec.URL)
	}
	if strings.Contains(rec.URL, "/nfr") {
		t.Fatalf("AddRemote did not strip /nfr path suffix: %q", rec.URL)
	}
}
