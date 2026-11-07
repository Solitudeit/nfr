package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/metrics"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

// hopHeaders are connection scoped and must not cross the tunnel.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// reconcile opens and closes public TCP listeners to match the registry.
func (s *Server) reconcile() {
	st := s.st.Snapshot()
	desired := make(map[int]store.Tunnel, len(st.Tunnels))
	for _, t := range st.Tunnels {
		if t.Enabled && t.Type == store.TunnelTCP && t.RemotePort > 0 {
			desired[t.RemotePort] = t
		}
	}

	s.mu.Lock()
	for port, ln := range s.listeners {
		if _, ok := desired[port]; !ok {
			ln.Close()
			delete(s.listeners, port)
		}
	}
	var open []int
	for port := range desired {
		if _, ok := s.listeners[port]; !ok {
			open = append(open, port)
		}
	}
	s.mu.Unlock()

	for _, port := range open {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			s.lg.Warn("cannot open public port", "port", port, "err", err)
			continue
		}
		s.mu.Lock()
		s.listeners[port] = ln
		s.mu.Unlock()
		s.wg.Add(1)
		go func(port int, ln net.Listener) {
			defer s.wg.Done()
			s.servePort(port, ln)
		}(port, ln)
		s.lg.Info("public port open", "port", port, "tunnel", desired[port].Name)
	}
}

func (s *Server) servePort(port int, ln net.Listener) {
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
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
			if t.Type == store.TunnelTCP && t.RemotePort == port {
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
	if isUpgrade(r) {
		s.proxyUpgrade(w, r, t)
		return true
	}
	s.proxyRequest(w, r, t)
	return true
}

func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (s *Server) openFor(t store.Tunnel, remote string) (net.Conn, error) {
	return s.openStream(t.NodeID, &proto.StreamHeader{Tunnel: t.ID, Remote: remote})
}

func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	ctr := s.met.For(t.ID)
	ctr.Conns.Add(1)
	stream, err := s.openFor(t, clientIP(r))
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: tunnel node is offline", http.StatusBadGateway)
		return
	}
	defer stream.Close()

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = ""
	out.URL.Host = ""
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Expect")
	if prior := out.Header.Get("X-Forwarded-For"); prior != "" {
		out.Header.Set("X-Forwarded-For", prior+", "+clientIP(r))
	} else {
		out.Header.Set("X-Forwarded-For", clientIP(r))
	}
	out.Header.Set("X-Forwarded-Proto", schemeOf(r))
	out.Header.Set("X-Forwarded-Host", r.Host)
	out.Header.Set("X-Nfr-Tunnel", t.Name)

	if err := out.Write(stream); err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: failed to forward request", http.StatusBadGateway)
		return
	}

	br := bufio.NewReader(stream)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: bad response from node", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(flushWriter{w}, resp.Body)

	in := r.ContentLength
	if in < 0 {
		in = 0
	}
	ctr.In.Add(in)
	ctr.Out.Add(n)
}

// flushWriter pushes each write out immediately so streaming responses reach
// the client without buffering the whole body.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// proxyUpgrade tunnels a protocol upgrade (websocket and friends) after
// hijacking the client connection.
func (s *Server) proxyUpgrade(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	ctr := s.met.For(t.ID)
	ctr.Conns.Add(1)
	stream, err := s.openFor(t, clientIP(r))
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: tunnel node is offline", http.StatusBadGateway)
		return
	}
	defer stream.Close()

	req := r.Clone(r.Context())
	req.RequestURI = ""
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.Header.Set("X-Forwarded-For", clientIP(r))
	req.Header.Set("X-Forwarded-Proto", schemeOf(r))
	req.Header.Set("X-Forwarded-Host", r.Host)
	if err := req.Write(stream); err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: failed to forward upgrade", http.StatusBadGateway)
		return
	}

	br := bufio.NewReader(stream)
	status, err := br.ReadString('\n')
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: bad upgrade response", http.StatusBadGateway)
		return
	}
	code := 0
	parts := strings.SplitN(strings.TrimSpace(status), " ", 3)
	if len(parts) >= 2 {
		code, _ = strconv.Atoi(parts[1])
	}
	hdrText, err := readHeaderBlock(br)
	if err != nil {
		ctr.Errs.Add(1)
		http.Error(w, "nfr: bad upgrade headers", http.StatusBadGateway)
		return
	}
	hdr, _ := parseHeaderBlock(hdrText)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "nfr: upgrades unsupported", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		ctr.Errs.Add(1)
		return
	}
	defer client.Close()

	fmt.Fprintf(client, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	for k, vs := range hdr {
		for _, v := range vs {
			fmt.Fprintf(client, "%s: %s\r\n", k, v)
		}
	}
	io.WriteString(client, "\r\n")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, io.MultiReader(br, stream))
		ctr.Out.Add(n)
		halfClose(client)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(stream, rw)
		ctr.In.Add(n)
		halfClose(stream)
	}()
	wg.Wait()
}

func readHeaderBlock(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		b.WriteString(line)
		if line == "\r\n" || line == "\n" {
			return b.String(), nil
		}
	}
}

func parseHeaderBlock(text string) (http.Header, error) {
	h := make(http.Header)
	for _, line := range strings.Split(text, "\r\n") {
		if line == "" {
			continue
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		h.Add(k, v)
	}
	return h, nil
}
