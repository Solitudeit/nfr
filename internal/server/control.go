package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
)

const maxRecentEvents = 100

// hub fan-outs change events to console subscribers and keeps a bounded recent
// history so the web console shows recent activity immediately on open.
type hub struct {
	mu     sync.Mutex
	subs   map[chan api.Event]struct{}
	recent []api.Event
}

func newHub() *hub { return &hub{subs: make(map[chan api.Event]struct{})} }

func (h *hub) subscribe() (<-chan api.Event, func()) {
	ch := make(chan api.Event, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *hub) publish(ev api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, ev)
	if len(h.recent) > maxRecentEvents {
		h.recent = h.recent[len(h.recent)-maxRecentEvents:]
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *hub) history() []api.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]api.Event, len(h.recent))
	copy(out, h.recent)
	return out
}

// RecentEvents returns a snapshot of the most recent server activity events.
func (s *Server) RecentEvents() []api.Event {
	if s.hub == nil {
		return nil
	}
	return s.hub.history()
}

// SubscribeEvents subscribes to live server activity events.
func (s *Server) SubscribeEvents() (<-chan api.Event, func()) {
	return s.hub.subscribe()
}

// agentSession is one live control connection.
type agentSession struct {
	nodeID    string
	sess      *yamux.Session
	send      chan *proto.Frame
	done      chan struct{}
	remote    string
	connected time.Time
	closeOnce sync.Once

	probeMu       sync.Mutex
	probeSem      chan struct{}
	probes        map[string]chan probeReply
	probeInflight map[string]*inflightProbe
}

func (a *agentSession) close() {
	a.closeOnce.Do(func() {
		close(a.done)
		if a.sess != nil {
			a.sess.Close()
		}
	})
}

func (a *agentSession) sendFrame(f *proto.Frame) error {
	select {
	case a.send <- f:
		return nil
	case <-a.done:
		return errors.New("session closed")
	default:
		// Control stream is backed up and unresponsive; tear down this session
		// so the agent reconnects cleanly instead of stalling global pushes.
		a.close()
		return errors.New("control send queue full")
	}
}

func (a *agentSession) kick(msg string) {
	select {
	case a.send <- &proto.Frame{Type: proto.TypeKick, Message: msg}:
	case <-a.done:
	default:
		a.close()
	}
}

func yamuxConfig() *yamux.Config {
	return transport.YamuxConfig()
}

// serveAgentConn upgrades a TLS connection to a yamux session and runs the
// first stream's handshake: either a join request or a control session.
func (s *Server) serveAgentConn(tc *tls.Conn) {
	sess, err := yamux.Server(tc, yamuxConfig())
	if err != nil {
		tc.Close()
		return
	}
	ctrl, err := sess.Accept()
	if err != nil {
		sess.Close()
		return
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
	f, err := proto.ReadFrame(ctrl)
	if err != nil {
		sess.Close()
		return
	}
	switch f.Type {
	case proto.TypeJoin:
		s.handleJoin(tc, sess, ctrl, f)
	case proto.TypeHello:
		s.handleControl(tc, sess, ctrl, f)
	default:
		_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "expected hello"})
		sess.Close()
	}
}

// joinAllowed rate limits join attempts per source address.
func (s *Server) joinAllowed(ip string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[ip]
	if a == nil {
		return true
	}
	if now.Before(a.until) && a.count >= 5 {
		return false
	}
	if now.After(a.until) {
		delete(s.attempts, ip)
	}
	return true
}

func (s *Server) pruneAttempts(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, a := range s.attempts {
		if !a.until.IsZero() && now.After(a.until) {
			delete(s.attempts, ip)
		}
	}
}

func (s *Server) joinFailed(ip string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[ip]
	if a == nil || (!a.until.IsZero() && now.After(a.until)) {
		a = &attempt{}
		s.attempts[ip] = a
	}
	a.count++
	if a.count >= 5 {
		a.until = now.Add(15 * time.Minute)
	} else {
		// Expire partial failed counts after 15 minutes of quiet so s.attempts
		// does not grow unbounded from random internet scanners.
		a.until = now.Add(15 * time.Minute)
	}
}

func (s *Server) handleJoin(tc *tls.Conn, sess *yamux.Session, ctrl net.Conn, f *proto.Frame) {
	defer sess.Close()
	ip := remoteIP(tc.RemoteAddr())
	if !s.joinAllowed(ip) {
		_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "too many attempts, try later"})
		return
	}
	code := auth.NormalizeJoinCode(f.Code)
	name := f.Node
	if name == "" {
		name = "node-" + store.NewID()[:4]
	}

	var (
		nodeID string
		token  string
		errMsg string
		bound  string
	)
	now := time.Now().UTC()
	err := s.st.Update(func(st *store.State) error {
		jc, ok := st.JoinCodeByValue(code)
		if !ok {
			errMsg = "invalid join code"
			return errors.New(errMsg)
		}
		if err := jc.Valid(now); err != nil {
			errMsg = err.Error()
			return err
		}
		token = auth.NewSecret("nfr_n_", 24)
		nodeID = store.NewID()
		st.Nodes = append(st.Nodes, store.Node{
			ID:         nodeID,
			Name:       name,
			TokenHash:  auth.Hash(token),
			OS:         f.OS,
			Arch:       f.Arch,
			Version:    f.Version,
			CreatedAt:  now,
			LastRemote: ip,
		})
		t := now
		jc.UsedAt = &t
		// A code minted by CreateTunnelAwaiting carries the tunnel it belongs
		// to: redeeming it activates that tunnel right here, in the same
		// update that creates the node. A deleted tunnel or one that already
		// has a node is skipped silently — the join itself still succeeds.
		if jc.TunnelID != "" {
			if t, ok := st.TunnelByID(jc.TunnelID); ok && t.NodeID == "" {
				t.NodeID = nodeID
				t.UpdatedAt = now
				bound = t.Name
			}
		}
		return nil
	})
	if err != nil {
		s.joinFailed(ip)
		_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: errMsg})
		return
	}
	s.lg.Info("node joined", "node", name, "id", nodeID, "remote", ip)
	s.hub.publish(api.Event{Type: "node-joined", At: now, Message: name})
	if bound != "" {
		// The store is no longer locked: the hub does not need it, and the
		// event wants the tunnel name captured inside the update.
		s.hub.publish(api.Event{Type: "tunnel-bound", At: now, Message: bound})
	}
	_ = proto.WriteFrame(ctrl, &proto.Frame{
		Type:    proto.TypeWelcome,
		NodeID:  nodeID,
		Token:   token,
		Message: "joined",
	})
}

func (s *Server) handleControl(tc *tls.Conn, sess *yamux.Session, ctrl net.Conn, hello *proto.Frame) {
	ip := remoteIP(tc.RemoteAddr())
	var node store.Node
	var found bool
	usingSharedToken := strings.TrimSpace(hello.NodeID) == ""

	if usingSharedToken {
		if !s.joinAllowed(ip) {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "too many attempts, try later"})
			sess.Close()
			return
		}
		shared := s.AuthToken()
		if shared == "" {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "server has no auth_token configured"})
			sess.Close()
			return
		}
		nonce := auth.RandomBytes(32)
		if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeChallenge, Nonce: nonce}); err != nil {
			sess.Close()
			return
		}
		_ = ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
		authFrame, err := proto.ReadFrame(ctrl)
		if err != nil || authFrame.Type != proto.TypeAuth || !auth.CheckMAC(auth.Hash(shared), nonce, authFrame.MAC) {
			s.joinFailed(ip)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "authentication failed: invalid auth_token"})
			sess.Close()
			return
		}
		_ = ctrl.SetReadDeadline(time.Time{})

		name := strings.TrimSpace(hello.Node)
		if name == "" {
			name = "node-" + store.NewID()[:4]
		}
		isPerTunnel := strings.HasPrefix(name, "t-")
		perTunnelID := strings.TrimPrefix(name, "t-")
		now := time.Now().UTC()
		created := false
		claimedChanged := false
		if err := s.st.Update(func(st *store.State) error {
			// Per-tunnel generated configs connect with name "t-<tunnel_id>".
			// Check existence, upsert node, and claim tunnel atomically inside
			// one store transaction so a concurrent delete never leaves an
			// orphan node or resurrects a deleted tunnel.
			var targetTun *store.Tunnel
			if isPerTunnel {
				t, ok := st.TunnelByID(perTunnelID)
				if !ok {
					return errNotFound("tunnel deleted or not found")
				}
				targetTun = t
			}
			if existing, ok := st.NodeByName(name); ok {
				existing.OS, existing.Arch, existing.Version = hello.OS, hello.Arch, hello.Version
				existing.LastSeen = &now
				existing.LastRemote = ip
				node = *existing
			} else {
				node = store.Node{
					ID:         store.NewID(),
					Name:       name,
					TokenHash:  auth.Hash(shared),
					OS:         hello.OS,
					Arch:       hello.Arch,
					Version:    hello.Version,
					CreatedAt:  now,
					LastSeen:   &now,
					LastRemote: ip,
				}
				st.Nodes = append(st.Nodes, node)
				created = true
			}
			if targetTun != nil && (targetTun.NodeID != node.ID || targetTun.ManagedByConfig) {
				targetTun.NodeID = node.ID
				targetTun.ManagedByConfig = false
				targetTun.UpdatedAt = now
				claimedChanged = true
			}
			return nil
		}); err != nil {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: err.Error()})
			sess.Close()
			return
		}
		if created {
			s.lg.Info("node registered via config", "node", node.Name, "id", node.ID, "remote", ip)
			s.hub.publish(api.Event{Type: "node-joined", At: now, Message: node.Name})
		}
		if claimedChanged {
			s.reconcile()
			s.notify("tunnel claimed: " + perTunnelID)
		}
	} else {
		if !s.joinAllowed(ip) {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "too many attempts, try later"})
			sess.Close()
			return
		}
		_ = s.st.View(func(st *store.State) error {
			if n, ok := st.NodeByID(hello.NodeID); ok {
				node, found = *n, true
			}
			return nil
		})
		if !found {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "unknown node; configure auth_token in client.toml / client.yaml or re-join"})
			sess.Close()
			return
		}
		nonce := auth.RandomBytes(32)
		if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeChallenge, Nonce: nonce}); err != nil {
			sess.Close()
			return
		}
		_ = ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
		authFrame, err := proto.ReadFrame(ctrl)
		if err != nil || authFrame.Type != proto.TypeAuth || !auth.CheckMAC(node.TokenHash, nonce, authFrame.MAC) {
			s.joinFailed(ip)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "authentication failed"})
			sess.Close()
			return
		}
		_ = ctrl.SetReadDeadline(time.Time{})
	}

	// Per-tunnel panel configs ("t-<id>") already claimed their tunnel atomically
	// above; regular client configs use declarative sync.
	if !(usingSharedToken && strings.HasPrefix(node.Name, "t-")) && (hello.SyncProxies || len(hello.Proxies) > 0) {
		if err := s.syncDeclarativeProxies(node.ID, hello.Proxies, hello.SyncProxies); err != nil {
			s.lg.Warn("sync client proxies failed", "node", node.Name, "err", err)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "proxy config error: " + err.Error()})
			sess.Close()
			return
		}
	}

	st := s.st.Snapshot()
	if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeWelcome, NodeID: node.ID, Revision: st.Revision}); err != nil {
		sess.Close()
		return
	}

	a := &agentSession{
		nodeID:        node.ID,
		sess:          sess,
		send:          make(chan *proto.Frame, 64),
		done:          make(chan struct{}),
		remote:        ip,
		connected:     time.Now().UTC(),
		probeSem:      make(chan struct{}, probeMaxPerNode),
		probes:        make(map[string]chan probeReply),
		probeInflight: make(map[string]*inflightProbe),
	}
	s.setSession(node.ID, a)
	defer func() {
		s.dropSession(node.ID, a)
		a.close()
	}()

	now := time.Now().UTC()
	_ = s.st.Update(func(storeState *store.State) error {
		if n, ok := storeState.NodeByID(node.ID); ok {
			n.LastSeen = &now
			n.OS, n.Arch, n.Version = hello.OS, hello.Arch, hello.Version
			n.LastRemote = ip
		}
		return nil
	})
	s.lg.Info("node online", "node", node.Name, "id", node.ID, "remote", ip)
	s.hub.publish(api.Event{Type: "node-online", At: now, Message: node.Name})
	defer func() {
		s.hub.publish(api.Event{Type: "node-offline", At: time.Now().UTC(), Message: node.Name})
		s.lg.Info("node offline", "node", node.Name, "id", node.ID)
	}()

	// Serialised writer with bounded write deadlines so a stalled peer cannot
	// hang the writer goroutine indefinitely.
	go func() {
		for {
			select {
			case f := <-a.send:
				_ = ctrl.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if err := proto.WriteFrame(ctrl, f); err != nil {
					a.close()
					return
				}
				_ = ctrl.SetWriteDeadline(time.Time{})
				if f.Type == proto.TypeKick {
					// Give the TCP stack a brief moment to flush the kick frame
					// before tearing down the yamux session.
					time.Sleep(30 * time.Millisecond)
					a.close()
					return
				}
			case <-a.done:
				return
			}
		}
	}()

	// Initial desired state and periodic liveness probes.
	cur := s.st.Snapshot()
	_ = a.sendFrame(s.configFrame(node.ID, cur.Revision, cur.TunnelsForNode(node.ID)))
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = a.sendFrame(&proto.Frame{Type: proto.TypePing, TS: time.Now().Unix()})
			case <-a.done:
				return
			}
		}
	}()

	for {
		_ = ctrl.SetReadDeadline(time.Now().Add(90 * time.Second))
		f, err := proto.ReadFrame(ctrl)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.TypePing:
			_ = a.sendFrame(&proto.Frame{Type: proto.TypePong, TS: time.Now().Unix()})
		case proto.TypePong:
		case proto.TypeDialFailed:
			// The agent is connected but its inner address refused. Record it
			// so the tunnel reports 目标不可达 rather than 就绪.
			s.markTargetDown(node.ID, f.Code, f.Message, f.TS)
		case proto.TypeProbeResult:
			a.deliverProbeResult(f)
		}
	}
}

// markTargetDown remembers that an agent could not reach a tunnel's inner
// address. The flag clears as soon as a connection succeeds or the tunnel is
// reconfigured, so a fixed local service recovers on its own.
func (s *Server) markTargetDown(nodeID, tunnelID, message string, ts int64) {
	if tunnelID == "" {
		return
	}
	at := time.Now()
	if ts > 0 {
		at = time.Unix(ts, 0)
	}
	s.mu.Lock()
	if s.targetDown == nil {
		s.targetDown = make(map[string]targetFailure)
	}
	s.targetDown[tunnelID] = targetFailure{node: nodeID, message: message, at: at}
	s.mu.Unlock()
	s.lg.Warn("agent cannot reach local service", "node", nodeID, "tunnel", tunnelID, "detail", message)
	if s.hub != nil {
		s.hub.publish(api.Event{Type: "tunnel-target-down", At: at, Message: message})
	}
}

// clearTargetDown forgets a failure once traffic gets through again.
func (s *Server) clearTargetDown(tunnelIDs ...string) {
	if len(tunnelIDs) == 0 {
		return
	}
	s.mu.Lock()
	for _, id := range tunnelIDs {
		delete(s.targetDown, id)
	}
	s.mu.Unlock()
}

// targetFailure is one agent's report that a tunnel endpoint refused.
type targetFailure struct {
	node    string
	message string
	at      time.Time
}

func remoteIP(addr net.Addr) string {
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

// configFrame renders the desired tunnel set of a node.
func (s *Server) configFrame(nodeID string, rev uint64, tunnels []store.Tunnel) *proto.Frame {
	specs := make([]proto.TunnelSpec, 0, len(tunnels))
	host := s.publicHost()
	if host == "" {
		if h, _, err := net.SplitHostPort(s.cfg.ControlListen); err == nil {
			host = transport.BestHost("", h)
		}
	}
	for _, t := range tunnels {
		pub := ""
		if t.Type == store.TunnelHTTP && len(t.Hosts) > 0 {
			pub = "http://" + t.Hosts[0]
		} else if t.RemotePort > 0 {
			addr := fmt.Sprintf(":%d", t.RemotePort)
			if host != "" {
				addr = net.JoinHostPort(host, fmt.Sprintf("%d", t.RemotePort))
			}
			if t.Type == store.TunnelHTTP {
				pub = "http://" + addr
			} else {
				pub = addr
			}
		}
		specs = append(specs, proto.TunnelSpec{
			ID:         t.ID,
			Name:       t.Name,
			Type:       t.Type,
			Local:      t.Local,
			PublicAddr: pub,
		})
	}
	return &proto.Frame{Type: proto.TypeConfig, NodeID: nodeID, Revision: rev, Tunnels: specs, TS: time.Now().Unix()}
}

// pushAll refreshes every connected agent.
func (s *Server) pushAll() {
	st := s.st.Snapshot()
	s.mu.Lock()
	sessions := make([]*agentSession, 0, len(s.agents))
	for _, a := range s.agents {
		sessions = append(sessions, a)
	}
	s.mu.Unlock()
	for _, a := range sessions {
		f := s.configFrame(a.nodeID, st.Revision, st.TunnelsForNode(a.nodeID))
		_ = a.sendFrame(f)
	}
}

// pushConfig refreshes one agent, if connected.
func (s *Server) pushConfig(nodeID string) {
	a := s.sessionFor(nodeID)
	if a == nil {
		return
	}
	st := s.st.Snapshot()
	_ = a.sendFrame(s.configFrame(nodeID, st.Revision, st.TunnelsForNode(nodeID)))
}

func (s *Server) sessionFor(nodeID string) *agentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agents[nodeID]
}

func (s *Server) setSession(nodeID string, a *agentSession) {
	s.mu.Lock()
	old := s.agents[nodeID]
	s.agents[nodeID] = a
	s.mu.Unlock()
	if old != nil && old != a {
		old.close()
	}
	s.dropHTTPProxiesForNode(nodeID)
}

func (s *Server) dropSession(nodeID string, a *agentSession) {
	dropped := false
	s.mu.Lock()
	if cur, ok := s.agents[nodeID]; ok && cur == a {
		delete(s.agents, nodeID)
		dropped = true
	}
	s.mu.Unlock()
	if dropped {
		s.dropHTTPProxiesForNode(nodeID)
	}
}

// openStream dials one stream to the named node's agent.
func (s *Server) openStream(nodeID string, h *proto.StreamHeader) (net.Conn, error) {
	a := s.sessionFor(nodeID)
	if a == nil || a.sess == nil {
		return nil, fmt.Errorf("node offline")
	}
	st, err := a.sess.Open()
	if err != nil {
		return nil, err
	}
	_ = st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := proto.WriteStreamHeader(st, h); err != nil {
		st.Close()
		return nil, err
	}
	_ = st.SetWriteDeadline(time.Time{})
	return st, nil
}

// syncDeclarativeProxies reconciles tunnels declared in a client's TOML/YAML
// config file onto nodeID. Existing tunnels with the same name are updated in
// place; missing ones are created and tagged ManagedByConfig=true. When prune
// is true, previously config-managed tunnels on nodeID that were removed from
// the client's proxies list are cleaned up without ever touching tunnels
// created manually through the web console or API.
func (s *Server) syncDeclarativeProxies(nodeID string, proxies []proto.ProxySpec, prune bool) error {
	now := time.Now().UTC()
	wanted := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		wanted[strings.ToLower(strings.TrimSpace(p.Name))] = true
	}
	var removedIDs []string
	changed, err := s.st.UpdateIf(func(st *store.State) (bool, error) {
		mutated := false
		for _, p := range proxies {
			name := strings.TrimSpace(p.Name)
			typ := strings.ToLower(strings.TrimSpace(p.Type))
			if typ == "" {
				typ = store.TunnelTCP
			}
			var existing *store.Tunnel
			for i := range st.Tunnels {
				if (st.Tunnels[i].NodeID == nodeID || st.Tunnels[i].NodeID == "") && strings.EqualFold(st.Tunnels[i].Name, name) {
					existing = &st.Tunnels[i]
					break
				}
			}
			// If an unbound panel-created standalone tunnel matches by name,
			// claim it in place while keeping panel ownership (ManagedByConfig=false).
			if existing != nil && existing.NodeID == "" && !existing.ManagedByConfig {
				existing.NodeID = nodeID
				existing.UpdatedAt = now
				mutated = true
				continue
			}
			// If an HTTP proxy has no hosts, no remote_port, and server has no
			// domain configured, auto-assign a free public port so IP-only VPS
			// deployments work out of the box.
			port := p.RemotePort
			if typ == store.TunnelHTTP && port == 0 && len(p.Hosts) == 0 && st.Settings.Domain == "" {
				if existing != nil && existing.RemotePort > 0 {
					port = existing.RemotePort
				} else {
					picked, perr := pickPort(st, "", "tcp")
					if perr != nil {
						return false, perr
					}
					port = picked
				}
			}
			en := true
			clearQuotaPause := false
			if existing != nil {
				en = existing.Enabled
				if existing.QuotaPaused {
					if p.QuotaMB == 0 || existing.UsedBytes <= p.QuotaMB<<20 {
						en = true
						clearQuotaPause = true
					} else {
						en = false
					}
				}
			}
			bw := p.BandwidthKB
			quota := p.QuotaMB
			req := api.TunnelRequest{
				Name:        name,
				NodeID:      nodeID,
				Type:        typ,
				Local:       p.Local,
				RemotePort:  port,
				Hosts:       p.Hosts,
				Enabled:     &en,
				BandwidthKB: &bw,
				QuotaMB:     &quota,
			}
			built, err := buildTunnel(st, req, existing, st.Settings.Domain, false)
			if err != nil {
				return false, fmt.Errorf("proxy %q: %w", name, err)
			}
			if existing != nil {
				if existing.NodeID == built.NodeID &&
					existing.Type == built.Type &&
					existing.Local == built.Local &&
					existing.RemotePort == built.RemotePort &&
					strings.Join(existing.Hosts, ",") == strings.Join(built.Hosts, ",") &&
					existing.Enabled == built.Enabled &&
					existing.BandwidthKB == built.BandwidthKB &&
					existing.QuotaMB == built.QuotaMB &&
					!clearQuotaPause &&
					existing.ManagedByConfig {
					continue
				}
				existing.NodeID = built.NodeID
				existing.Type = built.Type
				existing.Local = built.Local
				existing.RemotePort = built.RemotePort
				existing.Hosts = built.Hosts
				existing.Enabled = built.Enabled
				if clearQuotaPause {
					existing.QuotaPaused = false
				}
				existing.BandwidthKB = built.BandwidthKB
				existing.QuotaMB = built.QuotaMB
				existing.ManagedByConfig = true
				existing.UpdatedAt = now
				mutated = true
			} else {
				built.ID = store.NewID()
				built.ManagedByConfig = true
				built.CreatedAt = now
				built.UpdatedAt = now
				st.Tunnels = append(st.Tunnels, *built)
				mutated = true
			}
		}
		if prune {
			kept := st.Tunnels[:0]
			for _, t := range st.Tunnels {
				if t.NodeID == nodeID && t.ManagedByConfig && !wanted[strings.ToLower(t.Name)] {
					removedIDs = append(removedIDs, t.ID)
					mutated = true
					continue
				}
				kept = append(kept, t)
			}
			st.Tunnels = kept
		}
		return mutated, nil
	})
	if err != nil {
		return err
	}
	for _, id := range removedIDs {
		s.clearTargetDown(id)
		s.met.Drop(id)
		s.dropLimit(id)
	}
	if changed {
		s.reconcile()
		s.notify(fmt.Sprintf("synced %d config proxy(ies) for node %s", len(proxies), nodeID))
	}
	return nil
}
