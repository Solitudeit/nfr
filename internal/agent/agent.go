// Package agent maintains the outbound connection from a private machine to
// one NFR server and forwards the streams the server opens back to local
// services.
package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/transport"
	"github.com/solitudeit/nfr/internal/version"
)

// Options describes one server this agent connects to.
type Options struct {
	Addr   string // host:port of the control listener
	Token  string // node token minted by nfr join
	NodeID string
	Pin    string
	// CA verifies the server with the system trust store instead of pinning
	// its certificate fingerprint (used when the server runs ACME).
	CA     bool
	Logger *slog.Logger
}

// tunnelRoute is the local destination of one tunnel plus how to talk to it.
type tunnelRoute struct {
	local string
	kind  string // store.TunnelTCP / store.TunnelHTTP / store.TunnelUDP
}

// Agent keeps a single connection alive.
type Agent struct {
	opt Options
	lg  *slog.Logger

	mu     sync.Mutex
	routes map[string]tunnelRoute // tunnel id -> local destination
	sess   *yamux.Session
}

// New builds an agent.
func New(opt Options) *Agent {
	lg := opt.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Agent{opt: opt, lg: lg, routes: map[string]tunnelRoute{}}
}

// Local returns the address currently bound to a tunnel.
func (a *Agent) Local(tunnelID string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.routes[tunnelID]
	if !ok {
		return "", false
	}
	return v.local, true
}

// Run dials the server until the context is cancelled.
func (a *Agent) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 20*time.Second {
			backoff = time.Second
		}
		a.lg.Warn("connection lost", "server", a.opt.Addr, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (a *Agent) session(ctx context.Context) error {
	if a.opt.Token == "" || a.opt.NodeID == "" {
		return errors.New("this machine has no node token yet, run: nfr join <server> <code>")
	}
	host, _, err := net.SplitHostPort(a.opt.Addr)
	if err != nil {
		return fmt.Errorf("bad server address %q: %w", a.opt.Addr, err)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", a.opt.Addr)
	if err != nil {
		return err
	}
	tlsCfg := transport.ClientConfig(host, a.opt.Pin)
	if a.opt.CA {
		tlsCfg = transport.PublicClientConfig(host)
	}
	tc := tls.Client(raw, tlsCfg)
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		raw.Close()
		return err
	}
	if pin := transport.PinOf(tc.ConnectionState()); a.opt.Pin == "" && !a.opt.CA {
		a.lg.Info("trusting server certificate", "server", a.opt.Addr, "pin", transport.FormatPin(pin))
	} else if transport.FormatPin(pin) != transport.FormatPin(a.opt.Pin) {
		raw.Close()
		return errors.New("server certificate fingerprint changed, refusing to connect")
	}

	sess, err := yamux.Client(tc, yamuxConfig())
	if err != nil {
		tc.Close()
		return err
	}
	defer sess.Close()
	// Cancelling the context (Ctrl-C) must break the blocking frame read below,
	// so close the session instead of waiting for the read deadline.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-stop:
		}
	}()
	ctrl, err := sess.Open()
	if err != nil {
		return err
	}
	defer ctrl.Close()

	hello := &proto.Frame{
		Type:    proto.TypeHello,
		Proto:   proto.Version,
		NodeID:  a.opt.NodeID,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Version: version.String(),
	}
	if err := proto.WriteFrame(ctrl, hello); err != nil {
		return err
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
	f, err := proto.ReadFrame(ctrl)
	if err != nil {
		return err
	}
	if f.Type == proto.TypeError {
		return fmt.Errorf("server rejected the connection: %s", f.Message)
	}
	if f.Type != proto.TypeChallenge {
		return fmt.Errorf("unexpected frame %q during handshake", f.Type)
	}
	mac := auth.MAC(auth.Hash(a.opt.Token), f.Nonce)
	if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeAuth, MAC: mac}); err != nil {
		return err
	}
	f, err = proto.ReadFrame(ctrl)
	if err != nil {
		return err
	}
	if f.Type == proto.TypeError {
		return fmt.Errorf("server rejected the token: %s", f.Message)
	}
	if f.Type != proto.TypeWelcome {
		return fmt.Errorf("unexpected frame %q after auth", f.Type)
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	a.lg.Info("connected", "server", a.opt.Addr, "node", f.NodeID, "revision", f.Revision)

	a.mu.Lock()
	a.sess = sess
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.sess = nil
		a.routes = map[string]tunnelRoute{}
		a.mu.Unlock()
	}()

	go a.acceptLoop(ctx, sess)

	for {
		_ = ctrl.SetReadDeadline(time.Now().Add(2 * time.Minute))
		f, err := proto.ReadFrame(ctrl)
		if err != nil {
			return err
		}
		switch f.Type {
		case proto.TypeConfig:
			a.applyConfig(f)
		case proto.TypePing:
			if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypePong, TS: time.Now().Unix()}); err != nil {
				return err
			}
		case proto.TypePong:
		case proto.TypeError:
			return fmt.Errorf("server error: %s", f.Message)
		}
	}
}

func (a *Agent) applyConfig(f *proto.Frame) {
	next := make(map[string]tunnelRoute, len(f.Tunnels))
	for _, t := range f.Tunnels {
		next[t.ID] = tunnelRoute{local: t.Local, kind: t.Type}
	}
	a.mu.Lock()
	old := a.routes
	a.routes = next
	a.mu.Unlock()
	added, removed := 0, 0
	for id := range next {
		if _, ok := old[id]; !ok {
			added++
		}
	}
	for id := range old {
		if _, ok := next[id]; !ok {
			removed++
		}
	}
	a.lg.Info("configuration applied", "revision", f.Revision, "tunnels", len(next), "added", added, "removed", removed)
}

func (a *Agent) acceptLoop(ctx context.Context, sess *yamux.Session) {
	for {
		conn, err := sess.Accept()
		if err != nil {
			return
		}
		go a.handleStream(ctx, conn)
	}
}

func (a *Agent) handleStream(ctx context.Context, st net.Conn) {
	defer st.Close()
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	h, err := proto.ReadStreamHeader(st)
	if err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	a.mu.Lock()
	route, ok := a.routes[h.Tunnel]
	a.mu.Unlock()
	if !ok {
		a.lg.Warn("stream for unknown tunnel", "tunnel", h.Tunnel)
		return
	}
	if route.kind == "udp" {
		a.serveUDPStream(st, h.Tunnel, route.local)
		return
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	hc, err := dialer.DialContext(ctx, "tcp", route.local)
	if err != nil {
		a.lg.Warn("cannot reach local service", "tunnel", h.Tunnel, "local", route.local, "err", err)
		return
	}
	defer hc.Close()
	pipe(hc, st)
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		halfClose(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		halfClose(b)
	}()
	wg.Wait()
}

func halfClose(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = 256
	return cfg
}
