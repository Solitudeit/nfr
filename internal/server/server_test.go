package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/store"
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
		{Name: "weird", NodeID: "n1", Type: "grpc", Local: "8080"},
		{Name: "udpdomain", NodeID: "n1", Type: "udp", Local: "8080", Hosts: []string{"dns.example.com"}},
	} {
		if _, err := s.CreateTunnel(req); err == nil {
			t.Fatalf("request %+v was accepted", req)
		}
	}
}

func TestCreateTunnelHTTPOnPort(t *testing.T) {
	s, st := newTestServer(t, nil)
	addNode(t, st, "n1")
	port := 0
	for p := 25000; p < 25100; p++ {
		if canListen("tcp", p) {
			port = p
			break
		}
	}
	if port == 0 {
		t.Skip("no free port in the default range")
	}

	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "webui", NodeID: "n1", Type: "http", Local: "8080", RemotePort: port})
	if err != nil {
		t.Fatalf("create http tunnel on a port: %v", err)
	}
	if tun.RemotePort != port {
		t.Fatalf("remote port = %d, want %d", tun.RemotePort, port)
	}
	if len(tun.Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", tun.Hosts)
	}

	// The same port cannot be reused by another tunnel, whatever its type.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "dup", NodeID: "n1", Type: "tcp", Local: "8081", RemotePort: port}); err == nil {
		t.Fatal("duplicate public port accepted")
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

	first, err := s.AllocatePort("", "tcp")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first.Port < base || first.Port > base+40 {
		t.Fatalf("port %d outside the configured range", first.Port)
	}
	if first.LeaseID == "" || first.ExpiresAt.Before(time.Now()) {
		t.Fatalf("incomplete lease: %+v", first)
	}
	second, err := s.AllocatePort("", "tcp")
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
	if _, err := s.AllocatePort("", "tcp"); err == nil {
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

func TestForwardedHeadersFromLocalProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want the first X-Forwarded-For hop", got)
	}
	if got := schemeOf(r); got != "https" {
		t.Fatalf("schemeOf = %q, want https", got)
	}

	// Headers from a non-loopback peer are not trusted.
	r.RemoteAddr = "198.51.100.9:51234"
	if got := clientIP(r); got != "198.51.100.9" {
		t.Fatalf("clientIP = %q, want the socket peer", got)
	}
	if got := schemeOf(r); got != "http" {
		t.Fatalf("schemeOf = %q, want http", got)
	}
}

func TestCreateTunnelUDP(t *testing.T) {
	s, st := newTestServer(t, nil)
	addNode(t, st, "n1")

	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "dns", NodeID: "n1", Type: "udp", Local: "127.0.0.1:53"})
	if err != nil {
		t.Fatalf("create udp tunnel: %v", err)
	}
	if tun.RemotePort == 0 {
		t.Fatal("udp tunnel did not get a public port")
	}

	// TCP and UDP listen in independent port spaces: the same number works.
	tcpTwin, err := s.CreateTunnel(api.TunnelRequest{Name: "sshd", NodeID: "n1", Type: "tcp", Local: "127.0.0.1:22", RemotePort: tun.RemotePort})
	if err != nil {
		t.Fatalf("tcp tunnel on the same port number was rejected: %v", err)
	}
	if tcpTwin.RemotePort != tun.RemotePort {
		t.Fatalf("tcp twin port = %d, want %d", tcpTwin.RemotePort, tun.RemotePort)
	}

	// A second UDP tunnel cannot claim the same UDP port.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "dns2", NodeID: "n1", Type: "udp", Local: "127.0.0.1:54", RemotePort: tun.RemotePort}); err == nil {
		t.Fatal("duplicate udp public port accepted")
	}

	// UDP tunnels cannot use domains.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "dnsw", NodeID: "n1", Type: "udp", Local: "8053", Hosts: []string{"dns.example.com"}}); err == nil {
		t.Fatal("udp tunnel with domains accepted")
	}

	// Leases remember their port space.
	first, err := s.AllocatePort("", "udp")
	if err != nil {
		t.Fatalf("allocate udp port: %v", err)
	}
	_, err = s.CreateTunnel(api.TunnelRequest{Name: "leased", NodeID: "n1", Type: "tcp", Local: "80", RemotePort: first.Port, LeaseID: first.LeaseID})
	if err == nil {
		t.Fatal("a tcp tunnel consumed a udp lease")
	}
	_, err = s.CreateTunnel(api.TunnelRequest{Name: "leased-udp", NodeID: "n1", Type: "udp", Local: "8054", RemotePort: first.Port, LeaseID: first.LeaseID})
	if err != nil {
		t.Fatalf("udp tunnel could not consume the udp lease: %v", err)
	}
}
