package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nfr-tunnel/nfr/internal/api"
	"github.com/nfr-tunnel/nfr/internal/config"
	"github.com/nfr-tunnel/nfr/internal/log"
	"github.com/nfr-tunnel/nfr/internal/store"
)

func newTestServer(t *testing.T, mut func(*config.Server)) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.LogLevel = "error"
	if mut != nil {
		mut(&cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, st
}

func addNode(t *testing.T, st *store.Store, id string) {
	t.Helper()
	err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes, store.Node{ID: id, Name: "edge-" + id, CreatedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		t.Fatalf("add node: %v", err)
	}
}

func TestCertSummary(t *testing.T) {
	s, _ := newTestServer(t, nil)
	cert := s.CertSummary()
	if cert.Mode != modePin || cert.Source != "self-signed" {
		t.Fatalf("unexpected certificate summary: %+v", cert)
	}
	if len(cert.Pin) != 64 {
		t.Fatalf("fingerprint %q is not a hex sha256", cert.Pin)
	}
	if cert.NotAfter == nil || !cert.NotAfter.After(time.Now()) {
		t.Fatalf("certificate expiry missing: %+v", cert.NotAfter)
	}
}

func TestCreateTunnelHTTP(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.Domain = "example.com" })
	addNode(t, st, "n1")

	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "blog", NodeID: "n1", Type: "http", Local: "8080"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := tun.Hosts[0]; got != "blog.example.com" {
		t.Fatalf("default host = %q", got)
	}
	if tun.Local != "127.0.0.1:8080" {
		t.Fatalf("local normalised to %q", tun.Local)
	}

	// A second tunnel cannot claim the same host.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "blog2", NodeID: "n1", Type: "http", Local: "8081", Hosts: []string{"blog.example.com"}}); err == nil {
		t.Fatal("duplicate host accepted")
	}

	// Invalid input is rejected before touching state.
	for _, req := range []api.TunnelRequest{
		{Name: "", NodeID: "n1", Type: "http", Local: "8080", Hosts: []string{"x.example.com"}},
		{Name: "bad name!", NodeID: "n1", Type: "http", Local: "8080", Hosts: []string{"y.example.com"}},
		{Name: "ghost", NodeID: "missing", Type: "http", Local: "8080", Hosts: []string{"z.example.com"}},
		{Name: "weird", NodeID: "n1", Type: "udp", Local: "8080"},
	} {
		if _, err := s.CreateTunnel(req); err == nil {
			t.Fatalf("request %+v was accepted", req)
		}
	}
}

func TestCreateTunnelRequiresDomainForHTTP(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.Domain = "" })
	addNode(t, st, "n1")
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "web", NodeID: "n1", Type: "http", Local: "8080"}); err == nil {
		t.Fatal("http tunnel without a domain or hosts was accepted")
	}
	// An explicit host works without a domain.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "web", NodeID: "n1", Type: "http", Local: "8080", Hosts: []string{"web.internal"}}); err != nil {
		t.Fatalf("explicit host rejected: %v", err)
	}
}

// freePort returns a port that is currently unused on the host.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestAllocatePortAndLease(t *testing.T) {
	base := freePort(t)
	s, st := newTestServer(t, func(c *config.Server) { c.PortRange = [2]int{base, base + 40} })
	addNode(t, st, "n1")

	first, err := s.AllocatePort("")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first.Port < base || first.Port > base+40 {
		t.Fatalf("port %d outside the configured range", first.Port)
	}
	if first.LeaseID == "" || first.ExpiresAt.Before(time.Now()) {
		t.Fatalf("incomplete lease: %+v", first)
	}
	second, err := s.AllocatePort("")
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if second.Port == first.Port {
		t.Fatalf("allocated port %d twice", first.Port)
	}

	tun, err := s.CreateTunnel(api.TunnelRequest{
		Name: "raw", NodeID: "n1", Type: "tcp", Local: "9000",
		RemotePort: first.Port, LeaseID: first.LeaseID,
	})
	if err != nil {
		t.Fatalf("create with lease: %v", err)
	}
	if tun.RemotePort != first.Port {
		t.Fatalf("tunnel port = %d, want %d", tun.RemotePort, first.Port)
	}

	// The lease is consumed exactly once.
	if _, err := s.CreateTunnel(api.TunnelRequest{
		Name: "raw2", NodeID: "n1", Type: "tcp", Local: "9001",
		RemotePort: first.Port, LeaseID: first.LeaseID,
	}); err == nil {
		t.Fatal("a consumed lease was accepted twice")
	}

	// A port already used by a tunnel is refused.
	if _, err := s.CreateTunnel(api.TunnelRequest{
		Name: "dup", NodeID: "n1", Type: "tcp", Local: "9002", RemotePort: first.Port,
	}); err == nil {
		t.Fatal("duplicate remote port accepted")
	}

	// Auto assignment when remote_port is zero.
	auto, err := s.CreateTunnel(api.TunnelRequest{Name: "auto", NodeID: "n1", Type: "tcp", Local: "9003"})
	if err != nil {
		t.Fatalf("auto port: %v", err)
	}
	if auto.RemotePort < base || auto.RemotePort > base+40 {
		t.Fatalf("auto port %d outside the range", auto.RemotePort)
	}
}

func TestAllocatePortRangeExhausted(t *testing.T) {
	base := freePort(t)
	s, st := newTestServer(t, func(c *config.Server) { c.PortRange = [2]int{base, base} })
	addNode(t, st, "n1")
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "one", NodeID: "n1", Type: "tcp", Local: "9000", RemotePort: base}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AllocatePort(""); err == nil {
		t.Fatal("allocation succeeded in a range with no free port")
	}
}

func TestJoinCode(t *testing.T) {
	s, _ := newTestServer(t, nil)
	resp, err := s.CreateJoinCode(api.JoinCodeRequest{Name: "edge", TTLSeconds: 120})
	if err != nil {
		t.Fatalf("create join code: %v", err)
	}
	if len(resp.Code) != 6 || strings.ToUpper(resp.Code) != resp.Code {
		t.Fatalf("unexpected code %q", resp.Code)
	}
	if !strings.Contains(resp.Command, "nfr join") || !strings.Contains(resp.Command, resp.Code) {
		t.Fatalf("command %q is not copy-pasteable", resp.Command)
	}
	if time.Until(resp.ExpiresAt) > 2*time.Minute {
		t.Fatalf("ttl not honoured: %v", resp.ExpiresAt)
	}
}

func TestUpdateSettingsRange(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := s.UpdateSettings("example.com", true, [2]int{40000, 40100}, "a@b.c"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := s.Settings()
	if got.Domain != "example.com" || !got.APIEnabled || got.PortRange != [2]int{40000, 40100} {
		t.Fatalf("settings not applied: %+v", got)
	}
	if err := s.UpdateSettings("", false, [2]int{40000, 40000}, ""); err == nil {
		t.Fatal("a one-port range was accepted")
	}
}
