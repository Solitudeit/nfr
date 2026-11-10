package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
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
	if !strings.Contains(resp.Command, "nfrc join") || !strings.Contains(resp.Command, resp.Code) {
		t.Fatalf("command %q is not copy-pasteable", resp.Command)
	}
	if time.Until(resp.ExpiresAt) > 2*time.Minute {
		t.Fatalf("ttl not honoured: %v", resp.ExpiresAt)
	}
}

func TestUpdateSettingsRange(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := s.UpdateSettings("example.com", "", true, [2]int{40000, 40100}, "a@b.c"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := s.Settings()
	if got.Domain != "example.com" || !got.APIEnabled || got.PortRange != [2]int{40000, 40100} {
		t.Fatalf("settings not applied: %+v", got)
	}
	if err := s.UpdateSettings("", "", false, [2]int{40000, 40000}, ""); err == nil {
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

func TestSetNodeName(t *testing.T) {
	s, st := newTestServer(t, nil)
	addNode(t, st, "n1")

	if err := s.SetNodeName("n1", "  office-nas  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var node store.Node
	var ok bool
	if err := st.View(func(s *store.State) error {
		n, found := s.NodeByID("n1")
		if found {
			node, ok = *n, true
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	if !ok || node.Name != "office-nas" {
		t.Fatalf("node not renamed: %q ok=%v", node.Name, ok)
	}

	if err := s.SetNodeName("n1", "   "); err == nil {
		t.Fatal("blank name accepted")
	}
	if err := s.SetNodeName("missing", "x"); err == nil {
		t.Fatal("unknown node accepted")
	}
}

func TestConsoleHostIsNotATargetByDefault(t *testing.T) {
	s, _ := newTestServer(t, nil)

	if nodes := s.Nodes(); len(nodes) != 0 {
		t.Fatalf("console host leaks into the node list: %+v", nodes)
	}
	// An inner address belongs to the machine that joined. Until the operator
	// opts in, asking for the console host must fail loudly rather than quietly
	// dial a local address on the server.
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "self", NodeID: localNodeID, Type: "tcp", Local: "127.0.0.1:9"}); err == nil {
		t.Fatal("console host accepted as a tunnel target while disabled")
	}
	if _, err := s.CreateTunnel(api.TunnelRequest{Name: "self2", Type: "tcp", Local: "127.0.0.1:9"}); err == nil {
		t.Fatal("a tunnel without a node was accepted")
	}
}

func TestConsoleHostCanBeEnabledExplicitly(t *testing.T) {
	s, _ := newTestServer(t, nil)

	if err := s.SetLocalNode(true); err != nil {
		t.Fatalf("enable the console host: %v", err)
	}
	nodes := s.Nodes()
	if len(nodes) != 1 || nodes[0].ID != localNodeID || !nodes[0].Online || !nodes[0].Local {
		t.Fatalf("console host missing after opting in: %+v", nodes)
	}
	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "self", NodeID: localNodeID, Type: "tcp", Local: "127.0.0.1:9"})
	if err != nil {
		t.Fatalf("create tunnel on the console host: %v", err)
	}
	if tun.NodeName == "" {
		t.Fatal("tunnel has no node name for the console host")
	}

	if err := s.RemoveNode(localNodeID); err == nil {
		t.Fatal("console host was removable")
	}
	if err := s.SetNodeName(localNodeID, "home"); err != nil {
		t.Fatalf("rename the console host: %v", err)
	}
	if got := s.Nodes()[0].Name; got != "home" {
		t.Fatalf("console host name = %q, want home", got)
	}

	// Switching it back off must not silently keep the old tunnels alive: they
	// now point at a node that is gone, and say so.
	if err := s.SetLocalNode(false); err != nil {
		t.Fatalf("disable the console host: %v", err)
	}
	got := s.Tunnels()
	if len(got) != 1 || !got[0].NodeMissing {
		t.Fatalf("tunnel did not report its lost node: %+v", got)
	}
}

func TestConsoleHostForwardsTraffic(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := s.SetLocalNode(true); err != nil {
		t.Fatalf("enable the console host: %v", err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "local")
		_, _ = io.WriteString(w, "hello from the built-in node")
	}))
	defer backend.Close()

	port := 0
	for p := 25100; p < 25200; p++ {
		if canListen("tcp", p) {
			port = p
			break
		}
	}
	if port == 0 {
		t.Skip("no free port in the default range")
	}
	addr := strings.TrimPrefix(backend.URL, "http://")
	if _, err := s.CreateTunnel(api.TunnelRequest{
		Name: "localtcp", NodeID: localNodeID, Type: "tcp", Local: addr, RemotePort: port,
	}); err != nil {
		t.Fatalf("create tunnel: %v", err)
	}

	// reconcile() opens the public listener asynchronously enough that a short
	// retry loop is the simplest reliable way to wait for it.
	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public port never opened: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(body), "hello from the built-in node") {
		t.Fatalf("unexpected response: %q", body)
	}
}

func TestJoinCommandIsDialable(t *testing.T) {
	s, _ := newTestServer(t, func(c *config.Server) {
		c.Domain = "nfr.example.com"
		c.ControlListen = ":8443"
	})
	resp, err := s.CreateJoinCode(api.JoinCodeRequest{Name: "edge"})
	if err != nil {
		t.Fatalf("join code: %v", err)
	}
	if want := "nfrc join nfr.example.com:8443 " + resp.Code; resp.Command != want {
		t.Errorf("command=%q want %q", resp.Command, want)
	}

	// A wildcard bind with no domain must still print an address another
	// machine can actually dial, never the useless "localhost".
	s2, _ := newTestServer(t, func(c *config.Server) { c.ControlListen = ":8443" })
	resp2, err := s2.CreateJoinCode(api.JoinCodeRequest{Name: "edge"})
	if err != nil {
		t.Fatalf("join code: %v", err)
	}
	if strings.Contains(resp2.Command, "localhost") {
		t.Errorf("command=%q must not point at localhost", resp2.Command)
	}
	if !strings.HasSuffix(resp2.Command, ":8443 "+resp2.Code) {
		t.Errorf("command=%q should keep the control port and code", resp2.Command)
	}

	// An explicit public address beats interface probing: it is the one thing
	// the operator knows that this process cannot discover.
	s3, _ := newTestServer(t, func(c *config.Server) { c.ControlListen = ":8443" })
	if err := s3.UpdateSettings("", "203.0.113.7", false, [2]int{20000, 29999}, ""); err != nil {
		t.Fatalf("settings: %v", err)
	}
	resp3, err := s3.CreateJoinCode(api.JoinCodeRequest{Name: "edge"})
	if err != nil {
		t.Fatalf("join code: %v", err)
	}
	if want := "nfrc join 203.0.113.7:8443 " + resp3.Code; resp3.Command != want {
		t.Errorf("command=%q want %q", resp3.Command, want)
	}

	// Bound to loopback with nothing configured: the address only works here,
	// so the response must say so instead of pretending it is dialable.
	s4, _ := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:8443" })
	resp4, err := s4.CreateJoinCode(api.JoinCodeRequest{Name: "edge"})
	if err != nil {
		t.Fatalf("join code: %v", err)
	}
	if !strings.Contains(resp4.Command, "127.0.0.1:8443") {
		t.Errorf("command=%q should keep the loopback bind", resp4.Command)
	}
	if resp4.Warning == "" {
		t.Error("a loopback-only join command must carry a warning")
	}
}

func TestTunnelStatusWording(t *testing.T) {
	base := api.Tunnel{Enabled: true, Online: true}
	cases := []struct {
		name string
		in   api.Tunnel
		want string
		kind string
	}{
		{"disabled", api.Tunnel{Enabled: false}, "已停用", "off"},
		{"quota", api.Tunnel{Enabled: false, QuotaPaused: true}, "配额停用", "off"},
		{"missing node", api.Tunnel{Enabled: true, NodeMissing: true}, "需重新选择节点", "warn"},
		{"node offline", api.Tunnel{Enabled: true, Online: false}, "节点离线", "err"},
		{"target down", api.Tunnel{Enabled: true, Online: true, TargetDown: true}, "目标不可达", "err"},
		{"ready", base, "就绪", "idle"},
		{"busy", api.Tunnel{Enabled: true, Online: true, Active: 3}, "活跃", "on"},
	}
	for _, c := range cases {
		got, kind := tunnelStatus(c.in)
		if got != c.want || kind != c.kind {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, got, kind, c.want, c.kind)
		}
	}
	// 在线 must never come back: a tunnel is not a host.
	for _, c := range cases {
		if strings.Contains(c.want, "在线") {
			t.Errorf("%s still says 在线", c.name)
		}
	}
}

func TestDialFailureMarksTunnelTargetDown(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "off" })
	addNode(t, st, "n1")
	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "edge", NodeID: "n1", Type: "tcp", Local: "127.0.0.1:9"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Pretend the agent is connected: without a session the tunnel would
	// truthfully report 节点离线 and never reach the target check.
	s.setSession("n1", &agentSession{nodeID: "n1", send: make(chan *proto.Frame, 4), done: make(chan struct{})})
	t.Cleanup(func() { s.dropSession("n1", s.sessionFor("n1")) })
	if got := mustTunnel(t, s, tun.ID); got.Status != "就绪" {
		t.Fatalf("before report status=%q", got.Status)
	}
	s.markTargetDown("n1", tun.ID, "connection refused", time.Now().Unix())
	got := mustTunnel(t, s, tun.ID)
	if !got.TargetDown || got.Status != "目标不可达" {
		t.Fatalf("after report status=%q target_down=%v", got.Status, got.TargetDown)
	}
	// A successful stream clears the flag, so a fixed service recovers.
	s.clearTargetDown(tun.ID)
	if got := mustTunnel(t, s, tun.ID); got.TargetDown {
		t.Fatalf("target_down should clear")
	}
}

func mustTunnel(t *testing.T, s *Server, id string) api.Tunnel {
	t.Helper()
	v, ok := s.Tunnel(id)
	if !ok {
		t.Fatalf("tunnel %s not found", id)
	}
	return v
}

func TestCreateTunnelAwaiting(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// The brief pins ttl 30 minutes for the awaiting flow.
	tun, jc, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	if tun.NodeID != "" {
		t.Errorf("node id = %q, want empty", tun.NodeID)
	}
	if !tun.Awaiting {
		t.Error("awaiting flag not set")
	}
	if tun.Status != "等待接入" {
		t.Errorf("status = %q, want 等待接入", tun.Status)
	}
	if len(tun.Name) == 0 || tun.Name == "new" {
		t.Errorf("auto name not generated: %q", tun.Name)
	}
	if want := fmt.Sprintf("tunnel-%d", tun.RemotePort); tun.Name != want {
		t.Errorf("auto name = %q, want %q", tun.Name, want)
	}
	if len(jc.Code) != 6 {
		t.Errorf("join code = %q, want six characters", jc.Code)
	}
	if !strings.HasPrefix(jc.Command, "nfrc join ") {
		t.Errorf("command = %q, want the nfrc join prefix", jc.Command)
	}
	// The minted code must be bound to this tunnel, and the tunnel must
	// render as awaiting through the list path too.
	got := mustTunnel(t, s, tun.ID)
	if !got.Awaiting || got.Status != "等待接入" {
		t.Errorf("rendered tunnel: awaiting=%v status=%q", got.Awaiting, got.Status)
	}
}

func TestAwaitingPortReachable(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// A dedicated HTTP port so the 502 semantics are observable without any
	// node: reconcile must listen before a device ever joins.
	tun, _, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "http", Local: "127.0.0.1:80", RemotePort: 25470}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	if tun.RemotePort != 25470 {
		t.Fatalf("remote port = %d, want 25470", tun.RemotePort)
	}
	// reconcile() opens the public listener asynchronously enough that a short
	// retry loop is the simplest reliable way to wait for it.
	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:25470", 300*time.Millisecond)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public port never opened: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.Get("http://127.0.0.1:25470/")
	if err != nil {
		t.Fatalf("request awaiting port: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 while awaiting", resp.StatusCode)
	}
}

func TestTunnelStatusAwaiting(t *testing.T) {
	// An awaiting tunnel must not fall into the nodeMissing wording even
	// though it has no node yet.
	got, kind := tunnelStatus(api.Tunnel{Enabled: true, Awaiting: true, NodeMissing: true})
	if got != "等待接入" || kind != "warn" {
		t.Fatalf("got (%q,%q), want (等待接入,warn)", got, kind)
	}
	// Disabled stays first: a deactivated tunnel never shows 等待接入.
	if got, _ := tunnelStatus(api.Tunnel{Enabled: false, Awaiting: true}); got != "已停用" {
		t.Errorf("disabled awaiting tunnel status = %q, want 已停用", got)
	}
	if got, _ := tunnelStatus(api.Tunnel{Enabled: false, Awaiting: true, QuotaPaused: true}); got != "配额停用" {
		t.Errorf("quota-paused awaiting tunnel status = %q, want 配额停用", got)
	}
}

// Awaiting auto-port picking must respect the tunnel's own network: a UDP
// tunnel must not be handed a port another UDP tunnel already owns, even
// when that port is free in TCP space. One-port range where the only port is
// taken by a UDP tunnel: the picker must skip it (port_range_exhausted), not
// hand it back for bindPort to reject (port_taken).
func TestAwaitingUDPAutoPortRespectsUDPSpace(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.PortRange = [2]int{21000, 21000} })
	err := st.Update(func(state *store.State) error {
		state.Tunnels = append(state.Tunnels, store.Tunnel{
			ID: "seed1", Name: "dns", NodeID: "n1", Type: "udp",
			Local: "127.0.0.1:53", RemotePort: 21000, Enabled: true,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err = s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "udp", Local: "127.0.0.1:53"}, 30*time.Minute)
	if err == nil {
		t.Fatal("expected an error: the only port in range is taken by another udp tunnel")
	}
	if _, code := statusOf(err); code == "port_taken" {
		t.Fatalf("picker ignored the udp space and handed back the occupied port: %v", err)
	}
}

// startTestControl runs the control accept loop on an ephemeral loopback
// port, exercising the same serveControlConn path as startControl without
// racing s.ctlLn. It returns the dial address.
func startTestControl(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen control: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	go func() {
		close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveControlConn(c)
		}
	}()
	<-done
	return ln.Addr().String()
}

// joinViaControl connects a fake agent through the real control path —
// TLS with the tunnel ALPN, a yamux session, a join frame — and returns the
// server's reply. It mirrors what internal/cli does on a real device, so the
// redemption path is exercised end to end, not just the store mutation.
func joinViaControl(t *testing.T, s *Server, addr, code, name string) (*proto.Frame, error) {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	raw, err := dialer.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	tc := tls.Client(raw, transport.ClientConfig("localhost", s.Pin()))
	hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, err
	}
	sess, err := yamux.Client(tc, yamuxConfig())
	if err != nil {
		tc.Close()
		return nil, err
	}
	defer sess.Close()
	stream, err := sess.Open()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proto.WriteFrame(stream, &proto.Frame{
		Type: proto.TypeJoin, Code: code, Node: name,
		OS: "linux", Arch: "amd64", Version: "test",
	}); err != nil {
		return nil, err
	}
	f, err := proto.ReadFrame(stream)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// findNodeByName returns the node with the given name, if it joined.
func findNodeByName(t *testing.T, st *store.Store, name string) (store.Node, bool) {
	t.Helper()
	var found store.Node
	var ok bool
	if err := st.View(func(s *store.State) error {
		for _, n := range s.Nodes {
			if n.Name == name {
				found, ok = n, true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	return found, ok
}

func TestJoinBindsAwaitingTunnel(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	tun, jc, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}

	// The control listener address is assigned after startControl, so the
	// server must already be running before the fake agent dials it.
	addr := startTestControl(t, s)

	f, err := joinViaControl(t, s, addr, jc.Code, "edge-box")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if f.Type != proto.TypeWelcome || f.Token == "" {
		t.Fatalf("reply %q (msg %q), want welcome with a token", f.Type, f.Message)
	}

	node, ok := findNodeByName(t, st, "edge-box")
	if !ok {
		t.Fatal("joined node was not created")
	}
	if node.ID != f.NodeID {
		t.Errorf("welcome node id %q, want the stored node %q", f.NodeID, node.ID)
	}
	// The whole point: redemption binds the awaiting tunnel to the new node
	// inside the same update, so it stops waiting immediately.
	got := mustTunnel(t, s, tun.ID)
	if got.NodeID != node.ID {
		t.Errorf("tunnel node id = %q, want %q", got.NodeID, node.ID)
	}
	if got.Awaiting {
		t.Error("tunnel still awaiting after its code was redeemed")
	}
	if got.Status == "等待接入" {
		t.Errorf("status = %q, the bound tunnel must not wait anymore", got.Status)
	}

}

func TestJoinRedeemsDeletedTunnel(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	tun, jc, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	// Revocation deletes a deleted tunnel's bound codes, so this test reaches
	// the defensive path in handleJoin — a code whose tunnel vanished without
	// going through DeleteTunnel — by removing the tunnel from the store
	// directly.
	if err := st.Update(func(s *store.State) error {
		for i := range s.Tunnels {
			if s.Tunnels[i].ID == tun.ID {
				s.Tunnels = append(s.Tunnels[:i], s.Tunnels[i+1:]...)
				break
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("remove tunnel from store: %v", err)
	}

	addr := startTestControl(t, s)

	// The tunnel is gone, but the code itself is still valid: the join must
	// succeed and create the node, never panic on the missing tunnel.
	f, err := joinViaControl(t, s, addr, jc.Code, "late-box")
	if err != nil {
		t.Fatalf("join after tunnel deleted: %v", err)
	}
	if f.Type != proto.TypeWelcome || f.Token == "" {
		t.Fatalf("reply %q (msg %q), want welcome with a token", f.Type, f.Message)
	}
	if _, ok := findNodeByName(t, st, "late-box"); !ok {
		t.Fatal("node was not created after redeeming a deleted tunnel's code")
	}

}

func TestJoinDoesNotRebind(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	// A tunnel already owned by n1, plus a code that still claims this tunnel.
	// The code avoids I/L/O: NormalizeJoinCode maps those to 1/1/0, so a code
	// with them would not round-trip through the lookup.
	addNode(t, st, "n1")
	tun, err := s.CreateTunnel(api.TunnelRequest{Name: "owned", NodeID: "n1", Type: "tcp", Local: "127.0.0.1:80"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	if err := st.Update(func(s *store.State) error {
		s.PruneJoins(now)
		s.Joins = append(s.Joins, auth.JoinCode{
			Code: "REBND7", NodeName: "sneaky", TunnelID: tun.ID,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("mint rebind code: %v", err)
	}

	addr := startTestControl(t, s)

	f, err := joinViaControl(t, s, addr, "REBND7", "sneaky")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if f.Type != proto.TypeWelcome || f.Token == "" {
		t.Fatalf("reply %q (msg %q), want welcome with a token", f.Type, f.Message)
	}
	// Joining succeeds, the node exists, but an already-bound tunnel is never
	// touched — no rebinding, even for a freshly minted node.
	if _, ok := findNodeByName(t, st, "sneaky"); !ok {
		t.Fatal("node was not created")
	}
	got := mustTunnel(t, s, tun.ID)
	if got.NodeID != "n1" {
		t.Errorf("tunnel node id = %q, want unchanged %q", got.NodeID, "n1")
	}

}

// findBoundCode returns the live join code bound to the tunnel, if any.
func findBoundCode(t *testing.T, st *store.Store, tunnelID string) (auth.JoinCode, bool) {
	t.Helper()
	var found auth.JoinCode
	var ok bool
	if err := st.View(func(s *store.State) error {
		if jc, foundHere := store.FindJoinCodeByTunnel(s, tunnelID); foundHere {
			found, ok = *jc, true
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	return found, ok
}

func TestDeleteTunnelRevokesCode(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	tun, jc, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	// The bound code must be visible before the delete, so a passing test
	// can only come from the revocation, not from a code that never existed.
	if _, ok := findBoundCode(t, st, tun.ID); !ok {
		t.Fatal("bound code not found before delete")
	}
	if err := s.DeleteTunnel(tun.ID); err != nil {
		t.Fatalf("delete tunnel: %v", err)
	}

	addr := startTestControl(t, s)

	// Revocation: deleting the awaiting tunnel kills its bound code, so the
	// old command can never activate anything again.
	if f, err := joinViaControl(t, s, addr, jc.Code, "late-box"); err != nil {
		t.Fatalf("join with revoked code: %v", err)
	} else if f.Type != proto.TypeError {
		t.Fatalf("join with revoked code: frame %q, want an error reply", f.Type)
	}
	if _, ok := findBoundCode(t, st, tun.ID); ok {
		t.Error("bound code still findable after tunnel deleted")
	}
}

func TestRegenerateReplacesBoundCode(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	tun, old, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	fresh, err := s.RegenerateTunnelCode(tun.ID)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if fresh.Code == old.Code || len(fresh.Code) != 6 {
		t.Fatalf("fresh code %q must be new and six characters", fresh.Code)
	}
	if !strings.HasPrefix(fresh.Command, "nfrc join ") || !strings.Contains(fresh.Command, fresh.Code) {
		t.Errorf("command %q is not copy-pasteable", fresh.Command)
	}
	if want := 30 * time.Minute; time.Until(fresh.ExpiresAt) > want || time.Until(fresh.ExpiresAt) <= 0 {
		t.Errorf("expires in %v, want within %v", time.Until(fresh.ExpiresAt), want)
	}
	// At most one live bound code per tunnel: the old one must be gone.
	if _, ok := findBoundCode(t, st, tun.ID); !ok {
		t.Fatal("fresh bound code not found after regenerate")
	}
	if found, ok := findBoundCode(t, st, tun.ID); ok && found.Code != fresh.Code {
		t.Errorf("live bound code = %q, want the fresh %q", found.Code, fresh.Code)
	}

	addr := startTestControl(t, s)

	// The old code is revoked: its join fails...
	if f, err := joinViaControl(t, s, addr, old.Code, "stale-box"); err != nil {
		t.Fatalf("join with old code: %v", err)
	} else if f.Type != proto.TypeError {
		t.Fatalf("join with old code: frame %q, want an error reply", f.Type)
	}
	// ...and the fresh one joins and binds the tunnel.
	f, err := joinViaControl(t, s, addr, fresh.Code, "fresh-box")
	if err != nil {
		t.Fatalf("join with fresh code: %v", err)
	}
	if f.Type != proto.TypeWelcome || f.Token == "" {
		t.Fatalf("reply %q (msg %q), want welcome with a token", f.Type, f.Message)
	}
	node, ok := findNodeByName(t, st, "fresh-box")
	if !ok {
		t.Fatal("fresh code did not create a node")
	}
	got := mustTunnel(t, s, tun.ID)
	if got.NodeID != node.ID {
		t.Errorf("tunnel node id = %q, want %q from the fresh code", got.NodeID, node.ID)
	}
}

func TestRegenerateRejectsBoundTunnel(t *testing.T) {
	s, st := newTestServer(t, func(c *config.Server) { c.ControlListen = "127.0.0.1:0" })
	tun, old, err := s.CreateTunnelAwaiting(api.TunnelRequest{NodeID: "new", Type: "tcp", Local: "127.0.0.1:80"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}
	addr := startTestControl(t, s)
	if _, err := joinViaControl(t, s, addr, old.Code, "owner-box"); err != nil {
		t.Fatalf("join: %v", err)
	}
	node, ok := findNodeByName(t, st, "owner-box")
	if !ok {
		t.Fatal("node was not created")
	}
	if _, err := s.RegenerateTunnelCode(tun.ID); err == nil {
		t.Fatal("regenerate on a bound tunnel succeeded, want an error")
	}
	// A rejected regenerate must not touch the tunnel or mint a new code:
	// the only bound code is still the redeemed one, never re-minted.
	got := mustTunnel(t, s, tun.ID)
	if got.NodeID != node.ID {
		t.Errorf("tunnel node id = %q, want unchanged %q", got.NodeID, node.ID)
	}
	var boundCodes int
	if err := st.View(func(s *store.State) error {
		for _, j := range s.Joins {
			if j.TunnelID == tun.ID {
				boundCodes++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	if boundCodes != 1 {
		t.Errorf("bound codes = %d, want 1 (the redeemed one, never re-minted)", boundCodes)
	}
	// An unknown tunnel is an error, never a panic or a minted code.
	if _, err := s.RegenerateTunnelCode("no-such-tunnel"); err == nil {
		t.Error("regenerate on unknown tunnel succeeded, want an error")
	}
}

func TestTunnelJoinCodeAccessor(t *testing.T) {
	s, _ := newTestServer(t, func(cfg *config.Server) {
		cfg.ControlListen = "127.0.0.1:0"
	})
	created, _, err := s.CreateTunnelAwaiting(api.TunnelRequest{
		Name:   "awaiting",
		NodeID: "new",
		Type:   "tcp",
		Local:  "127.0.0.1:80",
	}, 30*time.Minute)
	if err != nil {
		t.Fatalf("create awaiting tunnel: %v", err)
	}

	jc, ok := s.TunnelJoinCode(created.ID)
	if !ok {
		t.Fatal("the freshly created awaiting tunnel has no join code")
	}
	if jc.Code == "" {
		t.Fatal("join code accessor returned an empty code")
	}
	if !strings.HasPrefix(jc.Command, "nfrc join ") {
		t.Fatalf("command = %q, want a full nfrc join command", jc.Command)
	}
	if jc.ExpiresAt.IsZero() {
		t.Fatal("join code accessor returned no expiry")
	}

	if _, ok := s.TunnelJoinCode("no-such-tunnel"); ok {
		t.Fatal("unknown tunnel reported a join code")
	}
}
