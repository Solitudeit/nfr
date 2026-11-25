package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/metrics"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

// reconcile opens and closes public listeners to match the registry. TCP and
// HTTP tunnels share the TCP port space; UDP tunnels listen in their own.
func (s *Server) reconcile() {
	st := s.st.Snapshot()
	s.pruneLimits(st.Tunnels)
	s.pruneHTTPProxies(st.Tunnels)
	desired := make(map[int]store.Tunnel, len(st.Tunnels))
	udpDesired := make(map[int]store.Tunnel)
	for _, t := range st.Tunnels {
		if !t.Enabled || t.RemotePort <= 0 {
			continue
		}
		// TCP tunnels always own a port; HTTP tunnels can own one too, which is
		// how http://<server-ip>:<port>/ works without any hostname.
		if t.Type == store.TunnelUDP {
			udpDesired[t.RemotePort] = t
			continue
		}
		desired[t.RemotePort] = t
	}

	var closePLs []*portListener
	s.mu.Lock()
	for port, pl := range s.listeners {
		t, ok := desired[port]
		if !ok || pl.typ != t.Type {
			closePLs = append(closePLs, pl)
			delete(s.listeners, port)
		}
	}
	for port, conn := range s.udpListeners {
		if _, ok := udpDesired[port]; !ok {
			conn.Close()
			delete(s.udpListeners, port)
		}
	}
	var open []int
	for port := range desired {
		if _, ok := s.listeners[port]; !ok {
			open = append(open, port)
		}
	}
	var openUDP []int
	for port := range udpDesired {
		if _, ok := s.udpListeners[port]; !ok {
			openUDP = append(openUDP, port)
		}
	}
	s.mu.Unlock()

	for _, pl := range closePLs {
		pl.close()
	}

	for _, port := range open {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			s.lg.Warn("cannot open public port", "port", port, "err", err)
			continue
		}
		typ := desired[port].Type
		pl := &portListener{ln: ln, typ: typ}
		if typ == store.TunnelHTTP {
			pl.httpSrv = s.newHTTPPortServer(port)
		}
		s.mu.Lock()
		s.listeners[port] = pl
		s.mu.Unlock()
		s.wg.Add(1)
		go func(port int, pl *portListener) {
			defer s.wg.Done()
			if pl.httpSrv != nil {
				_ = pl.httpSrv.Serve(pl.ln)
				return
			}
			s.servePort(port, pl.ln)
		}(port, pl)
		s.lg.Info("public port open", "port", port, "tunnel", desired[port].Name)
	}
	for _, port := range openUDP {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err != nil {
			s.lg.Warn("cannot open public udp port", "port", port, "err", err)
			continue
		}
		s.mu.Lock()
		s.udpListeners[port] = conn
		s.mu.Unlock()
		s.wg.Add(1)
		go func(port int, conn *net.UDPConn, t store.Tunnel) {
			defer s.wg.Done()
			s.serveUDPPort(port, conn, t)
		}(port, conn, udpDesired[port])
		s.lg.Info("public udp port open", "port", port, "tunnel", udpDesired[port].Name)
	}
}

func (s *Server) servePort(port int, ln net.Listener) {
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay *= 2
			}
			if delay > time.Second {
				delay = time.Second
			}
			select {
			case <-time.After(delay):
				continue
			case <-s.done():
				return
			}
		}
		delay = 0
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleTCP(c, port)
		}()
	}
}

// newHTTPPortServer builds an HTTP server for a dedicated public port. The
// tunnel is looked up per request so toggling it off takes effect immediately.
func (s *Server) newHTTPPortServer(port int) *http.Server {
	return &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok, _ := s.tunnelByPort(port)
			if !ok || !t.Enabled {
				http.Error(w, "nfr: no tunnel is listening on this port", http.StatusBadGateway)
				return
			}
			s.proxyHTTP(w, r, t)
		}),
	}
}

func (s *Server) done() <-chan struct{} {
	if s.ctx == nil {
		return make(chan struct{})
	}
	return s.ctx.Done()
}

func (s *Server) tunnelByPort(port int) (store.Tunnel, bool, string) {
	var (
		out  store.Tunnel
		okf  bool
		node string
	)
	_ = s.st.View(func(st *store.State) error {
		for _, t := range st.Tunnels {
			if (t.Type == store.TunnelTCP || t.Type == store.TunnelHTTP) && t.RemotePort == port {
				out, okf, node = t, true, t.NodeID
				return nil
			}
		}
		return nil
	})
	return out, okf, node
}

func (s *Server) handleTCP(c net.Conn, port int) {
	t, ok, _ := s.tunnelByPort(port)
	if !ok || !t.Enabled {
		c.Close()
		return
	}
	if lim := s.limiterFor(t); lim != nil {
		c = &throttledConn{Conn: c, up: lim.up, down: lim.down}
	}
	ctr := s.met.For(t.ID)
	ctr.Conns.Add(1)
	ctr.Active.Add(1)
	defer ctr.Active.Add(-1)

	stream, err := s.openStream(t.NodeID, &proto.StreamHeader{Tunnel: t.ID, Remote: c.RemoteAddr().String()})
	if err != nil {
		ctr.Errs.Add(1)
		s.lg.Debug("tunnel offline", "tunnel", t.Name, "err", err)
		c.Close()
		return
	}
	// A stream only exists once the agent accepted it, so any earlier
	// "cannot reach the inner address" report is stale.
	s.clearTargetDown(t.ID)
	proxyCount(c, stream, ctr)
}

// proxyCount copies both directions and records traffic.
func proxyCount(a, b net.Conn, ctr *metrics.Counter) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(b, a)
		ctr.In.Add(n)
		halfClose(b)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(a, b)
		ctr.Out.Add(n)
		halfClose(a)
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

// halfClose sends a FIN if the connection supports it; a yamux stream treats
// Close as a half close, which lets the peer drain what is still queued.
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// ---------------------------------------------------------------------------
// HTTP tunnels

func (s *Server) findHTTPTunnel(host string) (store.Tunnel, bool) {
	var (
		out store.Tunnel
		okf bool
	)
	_ = s.st.View(func(st *store.State) error {
		for _, t := range st.Tunnels {
			if !t.Enabled || t.Type != store.TunnelHTTP {
				continue
			}
			for _, h := range t.Hosts {
				if strings.EqualFold(h, host) {
					out, okf = t, true
					return nil
				}
			}
		}
		return nil
	})
	return out, okf
}

func (s *Server) serveTunnelHTTP(w http.ResponseWriter, r *http.Request) bool {
	// On TLS the SNI is the most reliable name: clients that reuse one
	// connection for several hosts still send the right SNI per connection.
	var t store.Tunnel
	ok := false
	if r.TLS != nil && r.TLS.ServerName != "" {
		t, ok = s.findHTTPTunnel(r.TLS.ServerName)
	}
	if !ok {
		t, ok = s.findHTTPTunnel(stripPort(r.Host))
	}
	if !ok {
		return false
	}
	s.proxyHTTP(w, r, t)
	return true
}

// httpTunnelProxy caches a pooled http.Transport and ReverseProxy for one HTTP
// tunnel so consecutive visitor requests reuse yamux streams and keep-alive
// connections to the private service instead of dialling per request.
type httpTunnelProxy struct {
	nodeID      string
	local       string
	name        string
	bandwidthKB int
	sess        *agentSession
	tr          *http.Transport
	rp          *httputil.ReverseProxy
}

func (p *httpTunnelProxy) close() {
	if p != nil && p.tr != nil {
		p.tr.CloseIdleConnections()
	}
}

// httpStreamConn wraps a yamux stream with bidirectional traffic accounting
// and optional token-bucket pacing. On a stream dialled toward the agent,
// Write carries visitor->origin traffic (In) and Read carries origin->visitor
// traffic (Out).
type httpStreamConn struct {
	net.Conn
	ctr *metrics.Counter
	lim *tunnelLimit
}

func (c *httpStreamConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if c.lim != nil {
			c.lim.down.take(n)
		}
		c.ctr.Out.Add(int64(n))
	}
	return n, err
}

func (c *httpStreamConn) Write(p []byte) (int, error) {
	if c.lim != nil {
		n, err := writeThrottled(c.Conn, c.lim.up, p)
		if n > 0 {
			c.ctr.In.Add(int64(n))
		}
		return n, err
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.ctr.In.Add(int64(n))
	}
	return n, err
}

type httpClientIPKey struct{}

func (s *Server) httpProxyFor(t store.Tunnel) (*httpTunnelProxy, error) {
	sess := s.sessionFor(t.NodeID)
	if sess == nil || sess.sess == nil {
		return nil, errors.New("node offline")
	}

	s.mu.Lock()
	if cur := s.httpProxies[t.ID]; cur != nil &&
		cur.nodeID == t.NodeID &&
		cur.local == t.Local &&
		cur.name == t.Name &&
		cur.bandwidthKB == t.BandwidthKB &&
		cur.sess == sess {
		s.mu.Unlock()
		return cur, nil
	}
	old := s.httpProxies[t.ID]
	s.mu.Unlock()

	ctr := s.met.For(t.ID)
	lim := s.limiterFor(t)
	tunnelID := t.ID
	nodeID := t.NodeID
	tunnelName := t.Name

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			remote, _ := ctx.Value(httpClientIPKey{}).(string)
			st, err := s.openStream(nodeID, &proto.StreamHeader{Tunnel: tunnelID, Remote: remote})
			if err != nil {
				return nil, err
			}
			return &httpStreamConn{Conn: st, ctr: ctr, lim: lim}, nil
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}

	rp := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "nfr-tunnel.internal"
			pr.Out.Host = pr.In.Host
			ip := clientIP(pr.In)
			if prior := pr.In.Header.Get("X-Forwarded-For"); prior != "" {
				pr.Out.Header.Set("X-Forwarded-For", prior+", "+ip)
			} else {
				pr.Out.Header.Set("X-Forwarded-For", ip)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", schemeOf(pr.In))
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Nfr-Tunnel", tunnelName)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Receiving response headers proves the agent dialled the local
			// service successfully, so clear any stale TargetDown report.
			s.clearTargetDown(tunnelID)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			ctr.Errs.Add(1)
			if strings.Contains(err.Error(), "node offline") {
				http.Error(w, "nfr: tunnel node is offline", http.StatusBadGateway)
				return
			}
			http.Error(w, "nfr: bad response from node", http.StatusBadGateway)
		},
	}

	p := &httpTunnelProxy{
		nodeID:      t.NodeID,
		local:       t.Local,
		name:        t.Name,
		bandwidthKB: t.BandwidthKB,
		sess:        sess,
		tr:          tr,
		rp:          rp,
	}

	s.mu.Lock()
	if s.httpProxies == nil {
		s.httpProxies = make(map[string]*httpTunnelProxy)
	}
	s.httpProxies[t.ID] = p
	s.mu.Unlock()
	old.close()
	return p, nil
}

func (s *Server) pruneHTTPProxies(tunnels []store.Tunnel) {
	desired := make(map[string]store.Tunnel, len(tunnels))
	for _, t := range tunnels {
		if t.Enabled && t.Type == store.TunnelHTTP {
			desired[t.ID] = t
		}
	}
	var drop []*httpTunnelProxy
	s.mu.Lock()
	for id, p := range s.httpProxies {
		t, ok := desired[id]
		if !ok || p.nodeID != t.NodeID || p.local != t.Local || p.name != t.Name || p.bandwidthKB != t.BandwidthKB {
			drop = append(drop, p)
			delete(s.httpProxies, id)
		}
	}
	s.mu.Unlock()
	for _, p := range drop {
		p.close()
	}
}

func (s *Server) dropHTTPProxiesForNode(nodeID string) {
	var drop []*httpTunnelProxy
	s.mu.Lock()
	for id, p := range s.httpProxies {
		if p.nodeID == nodeID {
			drop = append(drop, p)
			delete(s.httpProxies, id)
		}
	}
	s.mu.Unlock()
	for _, p := range drop {
		p.close()
	}
}

func (s *Server) closeAllHTTPProxies() {
	var drop []*httpTunnelProxy
	s.mu.Lock()
	for id, p := range s.httpProxies {
		drop = append(drop, p)
		delete(s.httpProxies, id)
	}
	s.mu.Unlock()
	for _, p := range drop {
		p.close()
	}
}

// proxyHTTP forwards one HTTP (or WebSocket/Upgrade) exchange to the tunnel's
// node using a pooled ReverseProxy.
func (s *Server) proxyHTTP(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	ctr := s.met.For(t.ID)
	ctr.Conns.Add(1)
	ctr.Active.Add(1)
	defer ctr.Active.Add(-1)

	p, err := s.httpProxyFor(t)
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: tunnel node is offline", http.StatusBadGateway)
		return
	}
	req := r.WithContext(context.WithValue(r.Context(), httpClientIPKey{}, clientIP(r)))
	p.rp.ServeHTTP(w, req)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Behind a local reverse proxy (nginx, Caddy), trust its headers.
	if isLoopback(host) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				xff = xff[:i]
			}
			if v := strings.TrimSpace(xff); v != "" {
				return v
			}
		}
	}
	return host
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if isLoopback(peerHost(r)) {
		switch p := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); p {
		case "http", "https":
			return p
		}
	}
	return "http"
}

func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
