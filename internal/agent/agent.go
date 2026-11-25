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
	"strings"
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
	Addr      string // host:port of the control listener
	AuthToken string // shared auth_token from client.toml / client.yaml
	Name      string // node display name when connecting with AuthToken
	Proxies   []proto.ProxySpec
	Token     string // legacy per-node token minted by nfrc join
	NodeID    string
	Pin       string
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
	nodeID string                 // active node id returned by welcome
	routes map[string]tunnelRoute // tunnel id -> local destination
	sess   *yamux.Session

	// ctrl is the control stream of the current session, guarded by wmu
	// because streams report dial failures from their own goroutines.
	ctrl     net.Conn
	wmu      sync.Mutex
	probeSem chan struct{}
}

// New builds an agent.
func New(opt Options) *Agent {
	lg := opt.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Agent{
		opt:      opt,
		lg:       lg,
		routes:   map[string]tunnelRoute{},
		probeSem: make(chan struct{}, maxConcurrentProbes),
	}
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

var (
	errKicked   = errors.New("kicked by server")
	errRejected = errors.New("rejected by server")
)

// Run dials the server until the context is cancelled, the server sends a kick
// frame, or the server permanently rejects the connection (e.g. unregistered or
// deleted tunnel).
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
		if errors.Is(err, errKicked) {
			a.mu.Lock()
			nid := a.nodeID
			a.mu.Unlock()
			if nid != "" {
				a.lg.Info(fmt.Sprintf("收到强制下线指令, 客户端已停止\nRunId: %s", nid))
			} else {
				a.lg.Info("收到强制下线指令, 客户端已停止")
			}
			return
		}
		if errors.Is(err, errRejected) {
			reason := strings.TrimPrefix(err.Error(), errRejected.Error()+": ")
			a.lg.Error(fmt.Sprintf("启动失败, 客户端已停止\n原因: %s", reason))
			return
		}
		if time.Since(start) > 20*time.Second {
			backoff = time.Second
		}
		a.lg.Warn(fmt.Sprintf("连接断开, %s 后重连...\n原因: %v", backoff, err))
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
	secret := a.opt.Token
	nodeID := a.opt.NodeID
	if nodeID == "" {
		secret = a.opt.AuthToken
	}
	if secret == "" {
		return errors.New("no auth_token configured; set auth_token in client.toml / client.yaml")
	}
	host, _, err := net.SplitHostPort(a.opt.Addr)
	if err != nil {
		return fmt.Errorf("bad server address %q: %w", a.opt.Addr, err)
	}
	a.lg.Info(fmt.Sprintf("连接节点\n地址: %s", a.opt.Addr))
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
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
	if !a.opt.CA {
		pin := transport.PinOf(tc.ConnectionState())
		if a.opt.Pin == "" {
			a.lg.Debug(fmt.Sprintf("首次信任证书指纹: %s", transport.FormatPin(pin)))
		} else if transport.FormatPin(pin) != transport.FormatPin(a.opt.Pin) {
			raw.Close()
			return errors.New("服务端证书指纹已变更，拒绝连接")
		}
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
		Type:        proto.TypeHello,
		Proto:       proto.Version,
		Node:        a.opt.Name,
		NodeID:      nodeID,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     version.String(),
		Proxies:     a.opt.Proxies,
		SyncProxies: nodeID == "",
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
		return fmt.Errorf("%w: %s", errRejected, f.Message)
	}
	if f.Type != proto.TypeChallenge {
		return fmt.Errorf("unexpected frame %q during handshake", f.Type)
	}
	mac := auth.MAC(auth.Hash(secret), f.Nonce)
	if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeAuth, MAC: mac}); err != nil {
		return err
	}
	f, err = proto.ReadFrame(ctrl)
	if err != nil {
		return err
	}
	if f.Type == proto.TypeError {
		return fmt.Errorf("%w: %s", errRejected, f.Message)
	}
	if f.Type != proto.TypeWelcome {
		return fmt.Errorf("unexpected frame %q after auth", f.Type)
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	a.lg.Info(fmt.Sprintf("登录节点成功\nRunId: %s", f.NodeID))

	a.mu.Lock()
	a.nodeID = f.NodeID
	a.sess = sess
	a.ctrl = ctrl
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.sess = nil
		a.ctrl = nil
		a.routes = map[string]tunnelRoute{}
		a.mu.Unlock()
	}()

	go a.acceptLoop(ctx, sess)

	// Proactive client-side heartbeat so NAT drops or blackholed links are
	// detected promptly instead of waiting passively for a long read timeout.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := a.sendFrame(&proto.Frame{Type: proto.TypePing, TS: time.Now().Unix()}); err != nil {
					sess.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()

	for {
		_ = ctrl.SetReadDeadline(time.Now().Add(90 * time.Second))
		f, err := proto.ReadFrame(ctrl)
		if err != nil {
			return err
		}
		switch f.Type {
		case proto.TypeConfig:
			a.applyConfig(f)
		case proto.TypePing:
			if err := a.sendFrame(&proto.Frame{Type: proto.TypePong, TS: time.Now().Unix()}); err != nil {
				return err
			}
		case proto.TypePong:
		case proto.TypeProbe:
			a.handleProbe(ctx, f)
		case proto.TypeKick:
			return errKicked
		case proto.TypeError:
			return fmt.Errorf("server error: %s", f.Message)
		}
	}
}

// sendFrame writes one control frame, serialising writers: the session loop
// answers pings while streams may report a dead local service at any moment.
func (a *Agent) sendFrame(f *proto.Frame) error {
	a.mu.Lock()
	ctrl := a.ctrl
	a.mu.Unlock()
	if ctrl == nil {
		return errors.New("not connected")
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_ = ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
	err := proto.WriteFrame(ctrl, f)
	_ = ctrl.SetWriteDeadline(time.Time{})
	return err
}

// reportDialFailed tells the server that this tunnel's inner address refused
// the connection, so the console can say 目标不可达 instead of guessing.
func (a *Agent) reportDialFailed(tunnelID, local string, cause error) {
	msg := local
	if cause != nil {
		msg = local + ": " + cause.Error()
	}
	a.mu.Lock()
	nid := a.nodeID
	a.mu.Unlock()
	if nid == "" {
		nid = a.opt.NodeID
	}
	_ = a.sendFrame(&proto.Frame{
		Type: proto.TypeDialFailed, NodeID: nid,
		Code: tunnelID, Message: msg, TS: time.Now().Unix(),
	})
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
	for _, t := range f.Tunnels {
		if _, existed := old[t.ID]; !existed {
			tag := t.ID + "." + t.Name
			a.lg.Info(fmt.Sprintf("隧道登记成功\n隧道: %s", tag))
			if t.PublicAddr != "" {
				a.lg.Info(fmt.Sprintf("启动 [%s] 隧道成功\n公网: %s\n本地: %s", t.Type, t.PublicAddr, t.Local))
			} else {
				a.lg.Info(fmt.Sprintf("启动 [%s] 隧道成功\n本地: %s", t.Type, t.Local))
			}
		}
	}
	for id := range old {
		if _, ok := next[id]; !ok {
			a.lg.Info(fmt.Sprintf("隧道已停止\n隧道: %s", id))
		}
	}
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
		a.reportDialFailed(h.Tunnel, route.local, err)
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
	return transport.YamuxConfig()
}
