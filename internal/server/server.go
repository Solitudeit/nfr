// Package server implements the control node: it terminates the public
// listeners, keeps the tunnel registry and speaks to edge agents over yamux.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/metrics"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
	"github.com/solitudeit/nfr/internal/version"
	"golang.org/x/crypto/acme"
)

// Options configures a control node.
type Options struct {
	Config config.Server
	Store  *store.Store
	Logger *slog.Logger
}

// Server is one NFR control node.
type Server struct {
	cfg        config.Server
	st         *store.Store
	lg         *slog.Logger
	met        *metrics.Registry
	hub        *hub
	tls        *tls.Config
	pin        string
	certMode   string
	certSource string
	panel      http.Handler
	api        http.Handler
	metrics    http.Handler
	started    time.Time
	ctx        context.Context

	mu           sync.Mutex
	agents       map[string]*agentSession
	listeners    map[int]net.Listener
	udpListeners map[int]*net.UDPConn
	udpSess      map[string]*udpSession
	attempts     map[string]*attempt
	lastUsed     map[string]time.Time
	limits       map[string]*tunnelLimit  // per tunnel bandwidth shaping
	flushed      map[string]int64         // quota accounting: metrics total already folded into the store
	targetDown   map[string]targetFailure // tunnel id -> last "cannot reach the inner address" report

	httpSrv  *http.Server
	httpsSrv *http.Server
	httpLn   net.Listener
	httpsLn  net.Listener
	ctlLn    net.Listener
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type attempt struct {
	count int
	until time.Time
}

// New prepares a node, generating the console password on first run. The
// plaintext password is returned exactly once.
func New(opts Options) (*Server, string, error) {
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}
	tc, pin, mode, source, err := loadTLS(opts.Config)
	if err != nil {
		return nil, "", fmt.Errorf("load certificate: %w", err)
	}
	s := &Server{
		cfg:          opts.Config,
		st:           opts.Store,
		lg:           lg,
		met:          metrics.New(),
		hub:          newHub(),
		tls:          tc,
		pin:          pin,
		certMode:     mode,
		certSource:   source,
		agents:       make(map[string]*agentSession),
		listeners:    make(map[int]net.Listener),
		udpListeners: make(map[int]*net.UDPConn),
		udpSess:      make(map[string]*udpSession),
		attempts:     make(map[string]*attempt),
		lastUsed:     make(map[string]time.Time),
		limits:       make(map[string]*tunnelLimit),
		flushed:      make(map[string]int64),
	}
	if err := seedSettings(opts.Store, opts.Config); err != nil {
		return nil, "", fmt.Errorf("seed settings: %w", err)
	}

	// The public HTTPS listener offers HTTP/1.1 as well as the tunnel
	// protocol so browsers reach the console on the control port. ACME needs
	// its own ALPN for the TLS-ALPN-01 challenge.
	s.tls.NextProtos = []string{proto.ALPN, "http/1.1"}
	if s.certSource == "acme" {
		s.tls.NextProtos = append(s.tls.NextProtos, acme.ALPNProto)
	}
	s.api = s.buildAPI()
	s.metrics = s.buildMetrics()

	var generated string
	var need bool
	_ = s.st.View(func(st *store.State) error {
		need = st.PanelPassword == ""
		return nil
	})
	if need {
		plain := auth.NewSecret("", 9)
		hash, err := config.HashPassword(plain)
		if err != nil {
			return nil, "", err
		}
		if err := s.st.Update(func(st *store.State) error {
			st.PanelPassword = hash
			return nil
		}); err != nil {
			return nil, "", err
		}
		generated = plain
	}
	return s, generated, nil
}

// Pin is the certificate fingerprint peers should trust.
func (s *Server) Pin() string { return s.pin }

// SetPanel installs the console handler served on the control port.
func (s *Server) SetPanel(h http.Handler) { s.panel = h }

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.ctx = ctx
	s.started = time.Now().UTC()

	if err := s.startControl(); err != nil {
		return err
	}
	if err := s.startHTTP(); err != nil {
		s.lg.Warn("http ingress disabled", "err", err, "hint", ingressHint(s.cfg.HTTPListen))
	}
	if err := s.startHTTPS(); err != nil {
		s.lg.Warn("https ingress disabled", "err", err, "hint", ingressHint(s.cfg.HTTPSListen))
	}
	s.reconcile()

	s.wg.Add(1)
	go s.loop(ctx)

	<-ctx.Done()
	return s.shutdown()
}

func (s *Server) loop(ctx context.Context) {
	defer s.wg.Done()
	sec := time.NewTicker(time.Second)
	defer sec.Stop()
	slow := time.NewTicker(30 * time.Second)
	defer slow.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sec.C:
			s.met.Sample()
		case <-slow.C:
			now := time.Now().UTC()
			_ = s.st.Update(func(st *store.State) error {
				st.PruneLeases(now)
				st.PruneJoins(now)
				return nil
			})
			s.flushQuotas()
			s.pruneUDPSessions()
		}
	}
}

func (s *Server) shutdown() error {
	s.mu.Lock()
	if s.ctlLn != nil {
		s.ctlLn.Close()
	}
	if s.httpLn != nil {
		s.httpLn.Close()
	}
	if s.httpsLn != nil {
		s.httpsLn.Close()
	}
	for port, ln := range s.listeners {
		ln.Close()
		delete(s.listeners, port)
	}
	for port, conn := range s.udpListeners {
		conn.Close()
		delete(s.udpListeners, port)
	}
	agents := make([]*agentSession, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, a)
	}
	s.agents = make(map[string]*agentSession)
	s.mu.Unlock()

	for _, a := range agents {
		a.close()
	}
	s.closeUDPSessions()
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
	if s.httpsSrv != nil {
		s.httpsSrv.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) startControl() error {
	if disabled(s.cfg.ControlListen) {
		return errors.New("control_listen is required")
	}
	ln, err := net.Listen("tcp", s.cfg.ControlListen)
	if err != nil {
		return fmt.Errorf("listen control %s: %w", s.cfg.ControlListen, err)
	}
	s.ctlLn = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.lg.Info("control listening", "addr", ln.Addr().String(), "pin", transport.FormatPin(s.pin))
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveControlConn(c)
			}()
		}
	}()
	return nil
}

func (s *Server) serveControlConn(raw net.Conn) {
	tc := tls.Server(raw, s.tls)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return
	}
	if tc.ConnectionState().NegotiatedProtocol == proto.ALPN {
		s.serveAgentConn(tc)
		return
	}
	s.serveConsoleConn(tc)
}

// serveConsoleConn hands a single TLS connection to the HTTP server, which is
// how the console shares the control port without a second listener.
func (s *Server) serveConsoleConn(c net.Conn) {
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.serveConsole),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	_ = srv.Serve(&oneConnListener{c: c})
}

// ingressHint explains how to coexist with a web server that already owns the
// port, which is the usual situation on nginx / Caddy boxes.
func ingressHint(addr string) string {
	return fmt.Sprintf("if %s is already used by nginx or Caddy, keep it running: set http_listen = \"127.0.0.1:8080\" and https_listen = \"off\" in server.toml, then run `nfrs proxy-config <domain>` and paste the snippet into that site's reverse-proxy config", addr)
}

func (s *Server) startHTTP() error {
	if disabled(s.cfg.HTTPListen) {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.HTTPListen)
	if err != nil {
		return err
	}
	s.httpLn = ln
	srv := s.newIngress()
	s.httpSrv = srv
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.lg.Info("http ingress listening", "addr", ln.Addr().String())
		_ = srv.Serve(ln)
	}()
	return nil
}

func (s *Server) startHTTPS() error {
	if disabled(s.cfg.HTTPSListen) {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.HTTPSListen)
	if err != nil {
		return err
	}
	s.httpsLn = ln
	srv := s.newIngress()
	s.httpsSrv = srv
	tlsLn := tls.NewListener(ln, s.tls)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.lg.Info("https ingress listening", "addr", ln.Addr().String())
		_ = srv.Serve(tlsLn)
	}()
	return nil
}

func (s *Server) newIngress() *http.Server {
	return &http.Server{
		Handler:           http.HandlerFunc(s.serveIngress),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
}

// serveConsole handles everything that lives on the control port.
func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/nfr":
		s.serveWellKnown(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/api/v1":
		s.api.ServeHTTP(w, r)
	case r.URL.Path == "/metrics":
		s.metrics.ServeHTTP(w, r)
	case s.panel != nil && (r.URL.Path == "/nfr" || strings.HasPrefix(r.URL.Path, "/nfr/")):
		s.panel.ServeHTTP(w, r)
	case r.URL.Path == "/":
		http.Redirect(w, r, "/nfr/", http.StatusTemporaryRedirect)
	default:
		http.NotFound(w, r)
	}
}

// serveIngress handles tunnelled traffic and discovery only.
func (s *Server) serveIngress(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/nfr" {
		s.serveWellKnown(w, r)
		return
	}
	if !s.serveTunnelHTTP(w, r) {
		host := stripPort(r.Host)
		http.Error(w, "nfr: no tunnel for host "+host, http.StatusBadGateway)
	}
}

func (s *Server) serveWellKnown(w http.ResponseWriter, r *http.Request) {
	st := s.st.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    st.Name,
		"version": version.Version,
		"control": s.advertisedControl(),
		"pin":     s.pin,
		"verify":  s.certMode,
	})
}

// advertisedControl is the control address a joining machine should dial. A
// wildcard bind (:8443) is not dialable, so the domain — or this machine's
// first public address — stands in; without that, discovery hands out an
// address nobody can reach.
func (s *Server) advertisedControl() string {
	host, port, err := net.SplitHostPort(s.cfg.ControlListen)
	if err != nil {
		return s.cfg.ControlListen
	}
	host = transport.BestHost(s.cfg.Domain, host)
	if port == "443" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ---------------------------------------------------------------------------
// registry views

// Info renders instance identity.
func (s *Server) Info() api.Info {
	st := s.st.Snapshot()
	s.mu.Lock()
	nodes := len(s.agents)
	s.mu.Unlock()
	return api.Info{
		ID:          st.ID,
		Name:        st.Name,
		Version:     version.Version,
		Revision:    st.Revision,
		Domain:      st.Settings.Domain,
		PortRange:   st.Settings.PortRange,
		APIEnabled:  st.Settings.APIEnabled,
		ControlAddr: s.cfg.ControlListen,
		PanelURL:    s.panelURL(),
		Pinned:      transport.FormatPin(s.pin),
		StartedAt:   s.started,
		Nodes:       nodes,
		Tunnels:     len(st.Tunnels),
	}
}

func (s *Server) panelURL() string {
	// The console address is for the operator's browser, so it prefers the
	// same address they configured for visitors; interface probing is only a
	// fallback for a fresh install with nothing set yet.
	host := s.publicHost()
	_, port, err := net.SplitHostPort(s.cfg.ControlListen)
	if err != nil {
		port = "8443"
	}
	if host == "" {
		host = firstSiteHost()
	}
	return "https://" + net.JoinHostPort(host, port) + "/nfr/"
}

// PanelBaseURL builds the console address for a configuration: the configured
// domain when present, otherwise the first real network address of this
// machine — "localhost" is useless on a headless public server.
func PanelBaseURL(cfg config.Server) string {
	_, port, err := net.SplitHostPort(cfg.ControlListen)
	if err != nil {
		port = "8443"
	}
	host := cfg.Domain
	if host == "" {
		host = firstSiteHost()
	}
	return "https://" + net.JoinHostPort(host, port) + "/nfr/"
}

// ConsoleCandidates lists the addresses a user may realistically paste into a
// browser: the configured domain first, then every interface address, then
// localhost as the last resort.
func ConsoleCandidates(cfg config.Server) []string {
	_, port, err := net.SplitHostPort(cfg.ControlListen)
	if err != nil {
		port = "8443"
	}
	var out []string
	if cfg.Domain != "" {
		out = append(out, "https://"+net.JoinHostPort(cfg.Domain, port)+"/nfr/")
	}
	for _, ip := range siteIPs() {
		out = append(out, "https://"+net.JoinHostPort(ip, port)+"/nfr/")
	}
	if len(out) == 0 {
		out = append(out, "https://"+net.JoinHostPort("localhost", port)+"/nfr/")
	}
	return out
}

// firstSiteHost returns the first usable public-facing address, cached.
func firstSiteHost() string {
	ips := siteIPs()
	if len(ips) == 0 {
		return "localhost"
	}
	return ips[0]
}

// siteIPs enumerates IPv4 addresses of up, non-loopback, non-link-local
// interfaces, most likely to be reachable from the outside first.
var siteIPs = sync.OnceValue(func() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
})

// Tunnels renders every tunnel with live counters.
func (s *Server) Tunnels() []api.Tunnel {
	st := s.st.Snapshot()
	stats := map[string]metrics.Snapshot{}
	for _, m := range s.met.Snapshot() {
		stats[m.TunnelID] = m
	}
	s.mu.Lock()
	online := make(map[string]bool, len(s.agents))
	for id := range s.agents {
		online[id] = true
	}
	s.mu.Unlock()
	s.mu.Lock()
	failures := make(map[string]targetFailure, len(s.targetDown))
	for id, f := range s.targetDown {
		failures[id] = f
	}
	s.mu.Unlock()
	// Visitors dial the configured domain, else the operator's public
	// address, else this host's first interface address. A bare ":20001" in
	// the console tells nobody where to connect.
	publicHost := s.publicHost()
	if publicHost == "" {
		publicHost = firstSiteHost()
	}
	out := make([]api.Tunnel, 0, len(st.Tunnels))
	for _, t := range st.Tunnels {
		nodeName := ""
		nodeOnline := online[t.NodeID]
		nodeMissing := false
		awaiting := t.NodeID == ""
		if t.NodeID == localNodeID {
			// The console host is only a target when the operator opted in;
			// otherwise the tunnel has lost its node and says so.
			if st.Settings.LocalNode {
				nodeName = st.Name
				if nodeName == "" {
					nodeName = "NFR"
				}
				nodeOnline = true
			} else {
				nodeMissing = true
			}
		} else if n, ok := st.NodeByID(t.NodeID); ok {
			nodeName = n.Name
		} else {
			nodeMissing = true
		}
		v := api.Tunnel{
			ID: t.ID, Name: t.Name, NodeID: t.NodeID, NodeName: nodeName,
			Type: t.Type, Local: t.Local, RemotePort: t.RemotePort, Hosts: t.Hosts,
			Enabled: t.Enabled, Awaiting: awaiting, Online: nodeOnline, NodeMissing: nodeMissing, PublicHost: publicHost,
			BandwidthKB: t.BandwidthKB, QuotaMB: t.QuotaMB, UsedBytes: t.UsedBytes, QuotaPaused: t.QuotaPaused,
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}
		if m, ok := stats[t.ID]; ok {
			v.In, v.Out, v.Conns, v.Active, v.Errs = m.In, m.Out, m.Conns, m.Active, m.Errs
			v.InRate, v.OutRate = m.InRate, m.OutRate
		}
		if _, ok := failures[t.ID]; ok {
			v.TargetDown = true
		}
		v.Status, v.StatusKind = tunnelStatus(v)
		out = append(out, v)
	}
	return out
}

// tunnelStatus is the single place that decides what one tunnel is doing.
// The wording avoids 在线: a tunnel is never "online", a node is connected and
// traffic is either flowing, ready to flow, or blocked by a named cause. A
// waiting tunnel has no node yet and says so ahead of the node checks.
func tunnelStatus(t api.Tunnel) (label, kind string) {
	switch {
	case !t.Enabled && t.QuotaPaused:
		return "配额停用", "off"
	case !t.Enabled:
		return "已停用", "off"
	case t.Awaiting:
		return "等待接入", "warn"
	case t.NodeMissing:
		return "需重新选择节点", "warn"
	case !t.Online:
		return "节点离线", "err"
	case t.TargetDown:
		return "目标不可达", "err"
	case t.Active > 0:
		return "活跃", "on"
	default:
		return "就绪", "idle"
	}
}

// Tunnel returns one tunnel view.
func (s *Server) Tunnel(id string) (api.Tunnel, bool) {
	for _, t := range s.Tunnels() {
		if t.ID == id {
			return t, true
		}
	}
	return api.Tunnel{}, false
}

// Nodes renders every agent.
func (s *Server) Nodes() []api.Node {
	st := s.st.Snapshot()
	s.mu.Lock()
	sess := make(map[string]string, len(s.agents))
	for id, a := range s.agents {
		sess[id] = a.remote
	}
	s.mu.Unlock()
	out := make([]api.Node, 0, len(st.Nodes)+1)
	for _, n := range st.Nodes {
		v := api.Node{
			ID: n.ID, Name: n.Name, OS: n.OS, Arch: n.Arch, Version: n.Version,
			LastSeen: n.LastSeen, CreatedAt: n.CreatedAt,
		}
		for _, t := range st.Tunnels {
			if t.NodeID == n.ID {
				v.Tunnels++
			}
		}
		if addr, ok := sess[n.ID]; ok {
			v.Online = true
			v.RemoteAddr = addr
		}
		out = append(out, v)
	}
	// The built-in node goes last so callers that pick the first online node
	// (scripts and integrations) keep choosing a real agent. It is hidden
	// unless the operator opted in to tunnelling the console host itself.
	if st.Settings.LocalNode {
		out = append(out, s.localNode(st))
	}
	return out
}

// Remotes renders managed instances.
func (s *Server) Remotes() []api.Remote {
	st := s.st.Snapshot()
	out := make([]api.Remote, 0, len(st.Remotes))
	for _, r := range st.Remotes {
		out = append(out, api.Remote{
			ID: r.ID, Name: r.Name, URL: r.URL, Version: r.Version,
			Nodes: r.Nodes, Tunnels: r.Tunnels,
			Reachable: r.LastError == "", LastError: r.LastError, CA: r.CA,
			LastSeen: r.LastSeen, CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// APIKeys lists credentials without their secrets.
func (s *Server) APIKeys() []api.APIKey {
	st := s.st.Snapshot()
	s.mu.Lock()
	used := make(map[string]time.Time, len(s.lastUsed))
	for k, v := range s.lastUsed {
		used[k] = v
	}
	s.mu.Unlock()
	out := make([]api.APIKey, 0, len(st.APIKeys))
	for _, k := range st.APIKeys {
		item := api.APIKey{ID: k.ID, Label: k.Label, Scope: k.Scope, CreatedAt: k.CreatedAt, LastUsed: k.LastUsed}
		if t, ok := used[k.ID]; ok {
			tt := t
			item.LastUsed = &tt
		}
		out = append(out, item)
	}
	return out
}

// Events exposes the change feed.
func (s *Server) Events() (<-chan api.Event, func()) { return s.hub.subscribe() }

// notify publishes a change and pushes fresh configuration to every agent.
func (s *Server) notify(msg string) {
	s.hub.publish(api.Event{Type: "change", At: time.Now().UTC(), Message: msg})
	s.pushAll()
}

// ---------------------------------------------------------------------------
// helpers

type errStatus struct {
	status int
	code   string
	msg    string
}

func (e *errStatus) Error() string { return e.msg }

func errBad(code, msg string) error {
	return &errStatus{status: http.StatusBadRequest, code: code, msg: msg}
}
func errConflict(code, msg string) error {
	return &errStatus{status: http.StatusConflict, code: code, msg: msg}
}
func errNotFound(msg string) error {
	return &errStatus{status: http.StatusNotFound, code: "not_found", msg: msg}
}

func statusOf(err error) (int, string) {
	var e *errStatus
	if errors.As(err, &e) {
		return e.status, e.code
	}
	return http.StatusInternalServerError, "internal"
}

// disabled reports whether a listener was switched off with "off" or "none".
func disabled(addr string) bool {
	switch strings.ToLower(strings.TrimSpace(addr)) {
	case "", "off", "none", "false":
		return true
	}
	return false
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(strings.TrimSuffix(h, "."))
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func portNum(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

func normalizeLocal(local string) (string, error) {
	local = strings.TrimSpace(local)
	if local == "" {
		return "", fmt.Errorf("local address is required")
	}
	host, port, err := net.SplitHostPort(local)
	if err != nil {
		// A bare port is the common case when people think "my service runs
		// on 8080", so accept it and assume the loopback interface.
		if n, convErr := strconv.Atoi(local); convErr == nil && n > 0 && n < 65536 {
			return net.JoinHostPort("127.0.0.1", local), nil
		}
		return "", fmt.Errorf("local must look like host:port or a port number")
	}
	if port == "" {
		return "", fmt.Errorf("local must include a port")
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n >= 65536 {
		return "", fmt.Errorf("local port %q is not a number", port)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// seedSettings copies bootstrap values from the configuration file into a
// brand new state document. Once the panel has been used the state file is
// authoritative and later config edits are ignored.
func seedSettings(st *store.Store, cfg config.Server) error {
	fresh := false
	_ = st.View(func(s *store.State) error {
		fresh = s.Revision <= 1 && len(s.Tunnels) == 0 && len(s.Nodes) == 0 && len(s.Remotes) == 0
		return nil
	})
	if !fresh {
		return nil
	}
	return st.Update(func(s *store.State) error {
		if cfg.Domain != "" {
			s.Settings.Domain = cfg.Domain
		}
		if cfg.PortRange[0] > 0 && cfg.PortRange[1] >= cfg.PortRange[0] {
			s.Settings.PortRange = cfg.PortRange
		}
		return nil
	})
}

// oneConnListener yields a single accepted connection.
type oneConnListener struct {
	mu   sync.Mutex
	c    net.Conn
	done bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.c == nil {
		return nil, net.ErrClosed
	}
	l.done = true
	return l.c, nil
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil {
		return l.c.RemoteAddr()
	}
	return &net.TCPAddr{}
}
