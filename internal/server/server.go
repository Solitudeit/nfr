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
	"golang.org/x/crypto/acme/autocert"
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
	acmeMgr    *autocert.Manager
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
	listeners    map[int]*portListener
	udpListeners map[int]*net.UDPConn
	udpSess      map[string]*udpSession
	httpProxies  map[string]*httpTunnelProxy
	attempts     map[string]*attempt
	lastUsed     map[string]time.Time
	limits       map[string]*tunnelLimit  // per tunnel bandwidth shaping
	flushed      map[string]int64         // quota accounting: metrics total already folded into the store
	targetDown   map[string]targetFailure // tunnel id -> last "cannot reach the inner address" report

	httpSrv    *http.Server
	httpsSrv   *http.Server
	consoleSrv *http.Server
	consoleLn  *chanListener
	httpLn     net.Listener
	httpsLn    net.Listener
	ctlLn      net.Listener
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	updater    *updater
}

type portListener struct {
	ln      net.Listener
	typ     string
	httpSrv *http.Server
}

func (pl *portListener) close() {
	if pl.httpSrv != nil {
		_ = pl.httpSrv.Close()
	}
	if pl.ln != nil {
		_ = pl.ln.Close()
	}
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
	if err := seedSettings(opts.Store, opts.Config); err != nil {
		return nil, "", fmt.Errorf("seed settings: %w", err)
	}
	tc, pin, mode, source, acmeMgr, err := loadTLS(opts.Config, opts.Store)
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
		acmeMgr:      acmeMgr,
		pin:          pin,
		certMode:     mode,
		certSource:   source,
		agents:       make(map[string]*agentSession),
		listeners:    make(map[int]*portListener),
		udpListeners: make(map[int]*net.UDPConn),
		udpSess:      make(map[string]*udpSession),
		httpProxies:  make(map[string]*httpTunnelProxy),
		attempts:     make(map[string]*attempt),
		lastUsed:     make(map[string]time.Time),
		limits:       make(map[string]*tunnelLimit),
		flushed:      make(map[string]int64),
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
	s.updater = newUpdater(s)

	var generated string
	var need bool
	_ = s.st.View(func(st *store.State) error {
		need = st.PanelPassword == ""
		return nil
	})
	if need {
		rawPass := strings.TrimSpace(opts.Config.Panel.Password)
		var hash string
		if rawPass == "" {
			plain := auth.NewSecret("", 9)
			var err error
			hash, err = config.HashPassword(plain)
			if err != nil {
				return nil, "", err
			}
			generated = plain
		} else if config.IsBcryptHash(rawPass) {
			hash = rawPass
		} else {
			var err error
			hash, err = config.HashPassword(rawPass)
			if err != nil {
				return nil, "", err
			}
		}
		if err := s.st.Update(func(st *store.State) error {
			st.PanelPassword = hash
			return nil
		}); err != nil {
			return nil, "", err
		}
	}
	// Ensure a shared client auth_token always exists so operators can drop a
	// static client.toml / client.yaml onto any machine immediately.
	if err := s.ensureAuthToken(); err != nil {
		return nil, "", fmt.Errorf("init auth_token: %w", err)
	}
	return s, generated, nil
}

func (s *Server) ensureAuthToken() error {
	if tok := strings.TrimSpace(s.cfg.AuthToken); tok != "" {
		return nil
	}
	_, err := s.st.UpdateIf(func(st *store.State) (bool, error) {
		if st.Settings.AuthToken != "" {
			return false, nil
		}
		st.Settings.AuthToken = auth.NewSecret("nfr_t_", 18)
		return true, nil
	})
	return err
}

// AuthToken returns the active shared client token. A token set in server.toml /
// server.yaml / NFR_AUTH_TOKEN takes precedence over the persisted store value.
func (s *Server) AuthToken() string {
	if tok := strings.TrimSpace(s.cfg.AuthToken); tok != "" {
		return tok
	}
	return s.st.Snapshot().Settings.AuthToken
}

// AuthTokenFromConfig reports whether auth_token is pinned by the server config
// file or environment (and therefore read-only in the web console).
func (s *Server) AuthTokenFromConfig() bool {
	return strings.TrimSpace(s.cfg.AuthToken) != ""
}

// SetAuthToken updates the stored shared client token.
func (s *Server) SetAuthToken(token string) error {
	if s.AuthTokenFromConfig() {
		return errBad("auth_token", "auth_token is set in server config / NFR_AUTH_TOKEN")
	}
	token = strings.TrimSpace(token)
	if len(token) < 6 {
		return errBad("auth_token", "auth_token must be at least 6 characters")
	}
	if err := s.st.Update(func(st *store.State) error {
		st.Settings.AuthToken = token
		return nil
	}); err != nil {
		return err
	}
	s.notify("auth token updated")
	return nil
}

// JoinAddr returns the explicit host:port address clients should put into
// server_addr in client.toml / client.yaml. Unlike legacy `nfrc join`, direct
// config connections default portless hosts to :8443, so the port is always
// included (even when control_listen is :443).
func (s *Server) JoinAddr() string {
	host, port, err := net.SplitHostPort(s.cfg.ControlListen)
	if err != nil {
		host, port = "", "8443"
	}
	host = transport.BestHost(s.publicHost(), host)
	return net.JoinHostPort(host, port)
}

// Pin is the certificate fingerprint peers should trust.
func (s *Server) Pin() string { return s.pin }

func panelScheme(cfg config.Server) string {
	if cfg.Panel.HTTPS {
		return "https"
	}
	return "http"
}

// EffectiveConfig returns the live server bootstrap config merged with runtime
// settings (domain, public_host, port_range, email, auth_token) for export.
func (s *Server) EffectiveConfig() config.Server {
	cfg := s.cfg
	set := s.Settings()
	cfg.Domain = set.Domain
	cfg.PublicHost = set.PublicHost
	cfg.PortRange = set.PortRange
	cfg.AuthToken = s.AuthToken()
	if set.HTTPSEmail != "" {
		cfg.ACME.Email = set.HTTPSEmail
	}
	cfg.Panel.Password = ""
	return cfg
}

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
	if s.updater != nil {
		s.updater.startBackground(ctx)
	}

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
			_, _ = s.st.UpdateIf(func(st *store.State) (bool, error) {
				l := st.PruneLeases(now)
				j := st.PruneJoins(now)
				return l || j, nil
			})
			s.flushQuotas()
			s.pruneUDPSessions()
			s.pruneAttempts(now)
		}
	}
}

func (s *Server) shutdown() error {
	s.mu.Lock()
	if s.ctlLn != nil {
		s.ctlLn.Close()
	}
	if s.consoleLn != nil {
		s.consoleLn.Close()
	}
	if s.httpLn != nil {
		s.httpLn.Close()
	}
	if s.httpsLn != nil {
		s.httpsLn.Close()
	}
	pls := make([]*portListener, 0, len(s.listeners))
	for port, pl := range s.listeners {
		pls = append(pls, pl)
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
	consoleSrv := s.consoleSrv
	s.mu.Unlock()

	for _, pl := range pls {
		pl.close()
	}
	for _, a := range agents {
		a.close()
	}
	s.closeAllHTTPProxies()
	s.closeUDPSessions()
	if consoleSrv != nil {
		_ = consoleSrv.Close()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	if s.httpsSrv != nil {
		_ = s.httpsSrv.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) startControl() error {
	if disabled(s.cfg.ControlListen) {
		return errors.New("control_listen is required")
	}
	lc := transport.ListenConfig()
	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.ControlListen)
	if err != nil {
		return fmt.Errorf("listen control %s: %w", s.cfg.ControlListen, err)
	}
	cln := newChanListener(ln.Addr())
	csrv := &http.Server{
		Handler:           http.HandlerFunc(s.serveConsole),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	s.mu.Lock()
	s.ctlLn = ln
	s.consoleLn = cln
	s.consoleSrv = csrv
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = csrv.Serve(cln)
	}()

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

// serveControlConn peeks the first byte of each connection on control_listen:
//   - 0x16 (TLS ClientHello): handshakes TLS; ALPN "nfr/1" routes to agents,
//     everything else routes to the HTTPS console/API server.
//   - anything else (plain HTTP): when panel.https is false (default), serves
//     the console/API over plain HTTP directly with zero self-signed browser
//     warnings; when panel.https is true, redirects HTTP requests to HTTPS.
func (s *Server) serveControlConn(raw net.Conn) {
	_ = raw.SetReadDeadline(time.Now().Add(15 * time.Second))
	var first [1]byte
	if _, err := raw.Read(first[:]); err != nil {
		raw.Close()
		return
	}
	_ = raw.SetReadDeadline(time.Time{})
	conn := &prefixConn{Conn: raw, first: first[0], unread: true}

	if first[0] == 0x16 {
		tc := tls.Server(conn, s.tls)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return
		}
		if tc.ConnectionState().NegotiatedProtocol == proto.ALPN {
			s.serveAgentConn(tc)
			return
		}
		s.serveConsoleConn(tc)
		return
	}
	s.serveConsoleConn(conn)
}

// prefixConn replays one peeked byte before reading from the underlying Conn.
type prefixConn struct {
	net.Conn
	first  byte
	unread bool
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.unread && len(p) > 0 {
		p[0] = c.first
		c.unread = false
		return 1, nil
	}
	return c.Conn.Read(p)
}

// serveConsoleConn hands a non-tunnel TLS connection to the shared console
// HTTP server, so shutdown immediately closes active keep-alive and SSE streams.
func (s *Server) serveConsoleConn(c net.Conn) {
	s.mu.Lock()
	cln := s.consoleLn
	s.mu.Unlock()
	if cln != nil {
		if !cln.push(c) {
			c.Close()
		}
		return
	}
	// Fallback for direct unit-test invocations that bypass startControl().
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
	lc := transport.ListenConfig()
	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.HTTPListen)
	if err != nil {
		return err
	}
	s.httpLn = ln
	srv := &http.Server{
		Handler:           s.httpIngressHandler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
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
	lc := transport.ListenConfig()
	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.HTTPSListen)
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
	if s.cfg.Panel.HTTPS && r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
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
	pref := s.publicHost()
	if pref == "" {
		pref = s.cfg.Domain
	}
	host = transport.BestHost(pref, host)
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
	return panelScheme(s.cfg) + "://" + net.JoinHostPort(host, port) + "/nfr/"
}

// PanelBaseURL builds the console address for a configuration: the configured
// domain or public host when present, otherwise the first real network address
// of this machine — "localhost" is useless on a headless public server.
func PanelBaseURL(cfg config.Server) string {
	_, port, err := net.SplitHostPort(cfg.ControlListen)
	if err != nil {
		port = "8443"
	}
	host := cfg.Domain
	if host == "" {
		host = cfg.PublicHost
	}
	if host == "" {
		host = firstSiteHost()
	}
	return panelScheme(cfg) + "://" + net.JoinHostPort(host, port) + "/nfr/"
}

// ConsoleCandidates lists the addresses a user may realistically paste into a
// browser: the configured domain / public host first, then every interface
// address, then localhost as the last resort.
func ConsoleCandidates(cfg config.Server) []string {
	_, port, err := net.SplitHostPort(cfg.ControlListen)
	if err != nil {
		port = "8443"
	}
	scheme := panelScheme(cfg)
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h == "" {
			return
		}
		u := scheme + "://" + net.JoinHostPort(h, port) + "/nfr/"
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	add(cfg.Domain)
	add(cfg.PublicHost)
	for _, ip := range siteIPs() {
		add(ip)
	}
	if len(out) == 0 {
		add("localhost")
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
	for _, m := range s.met.LiveSnapshot() {
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
		if !awaiting {
			if n, ok := st.NodeByID(t.NodeID); ok {
				nodeName = n.Name
			} else {
				nodeMissing = true
			}
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
	out := make([]api.Node, 0, len(st.Nodes))
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
	_, err := st.UpdateIf(func(s *store.State) (bool, error) {
		changed := false
		if cfg.Domain != "" && s.Settings.Domain != cfg.Domain {
			s.Settings.Domain = cfg.Domain
			changed = true
		}
		if cfg.PublicHost != "" && s.Settings.PublicHost != cfg.PublicHost {
			s.Settings.PublicHost = cfg.PublicHost
			changed = true
		}
		if cfg.PortRange[0] > 0 && cfg.PortRange[1] >= cfg.PortRange[0] && s.Settings.PortRange != cfg.PortRange {
			s.Settings.PortRange = cfg.PortRange
			changed = true
		}
		if cfg.ACME.Email != "" && s.Settings.HTTPSEmail == "" {
			s.Settings.HTTPSEmail = cfg.ACME.Email
			changed = true
		}
		return changed, nil
	})
	return err
}

// chanListener feeds already-handshaked TLS connections to a single shared
// http.Server so Close() tears down every active keep-alive and SSE stream.
type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr: addr,
		ch:   make(chan net.Conn, 64),
		done: make(chan struct{}),
	}
}

func (l *chanListener) push(c net.Conn) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		for {
			select {
			case c := <-l.ch:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *chanListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return &net.TCPAddr{}
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
