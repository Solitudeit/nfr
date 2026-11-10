package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
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
