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

// RecentEventsForOwner returns recent events visible to ownerID. Admins see
// their own events plus global system events; regular users see only their own.
func (s *Server) RecentEventsForOwner(ownerID string, isAdmin bool) []api.Event {
	all := s.RecentEvents()
	out := make([]api.Event, 0, len(all))
	for _, ev := range all {
		if ev.OwnerID == ownerID || (isAdmin && ev.OwnerID == "") {
			out = append(out, ev)
		}
	}
	return out
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
		nodeID  string
		ownerID string
		token   string
		errMsg  string
		bound   string
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
		ownerID = defaultOwnerID(st, jc.OwnerID)
		if u, ok := st.UserByID(ownerID); ok && u.Disabled {
			errMsg = "账号已被停用，请联系管理员"
			return errors.New(errMsg)
		}
		token = auth.NewSecret("nfr_n_", 24)
		nodeID = store.NewID()
		st.Nodes = append(st.Nodes, store.Node{
			ID:         nodeID,
			OwnerID:    ownerID,
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
		if jc.TunnelID != "" {
			if t, ok := st.TunnelByID(jc.TunnelID); ok && t.NodeID == "" && (t.OwnerID == "" || t.OwnerID == ownerID) {
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
	s.hub.publish(api.Event{Type: "node-joined", At: now, Message: name, OwnerID: ownerID})
	if bound != "" {
		s.hub.publish(api.Event{Type: "tunnel-bound", At: now, Message: bound, OwnerID: ownerID})
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
		type tokenCandidate struct {
			userID   string
			token    string
			disabled bool
		}
		snap := s.st.Snapshot()
		pinnedAdminTok := strings.TrimSpace(s.cfg.AuthToken)
		candidates := make([]tokenCandidate, 0, len(snap.Users)+1)
		for _, u := range snap.Users {
			tok := u.AuthToken
			if u.Role == store.RoleAdmin {
				if pinnedAdminTok != "" {
					tok = pinnedAdminTok
				} else if tok == "" {
					tok = snap.Settings.AuthToken
				}
			}
			if tok != "" {
				candidates = append(candidates, tokenCandidate{userID: u.ID, token: tok, disabled: u.Disabled})
			}
		}
		if len(candidates) == 0 {
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
		if err != nil || authFrame.Type != proto.TypeAuth {
			s.joinFailed(ip)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "authentication failed: invalid auth_token"})
			sess.Close()
			return
		}
		var matched *tokenCandidate
		for i := range candidates {
			if auth.CheckMAC(auth.Hash(candidates[i].token), nonce, authFrame.MAC) {
				matched = &candidates[i]
				break
			}
		}
		if matched == nil {
			s.joinFailed(ip)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "authentication failed: invalid auth_token"})
			sess.Close()
			return
		}
		if matched.disabled {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "账号已被停用，请联系管理员"})
			sess.Close()
			return
		}
		_ = ctrl.SetReadDeadline(time.Time{})
		authedUserID := matched.userID

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
			u, ok := st.UserByID(authedUserID)
			if !ok || u.Disabled {
				return errForbidden("user_disabled", "账号已被停用，请联系管理员")
			}

			// Panel-only tunnel authority + strict per-user isolation: clients
			// may NEVER create tunnels or claim another user's tunnel.
			var existingNode *store.Node
			for i := range st.Nodes {
				if st.Nodes[i].OwnerID == authedUserID && strings.EqualFold(st.Nodes[i].Name, name) {
					existingNode = &st.Nodes[i]
					break
				}
			}
			hasNode := existingNode != nil
			var candidateNodeID string
			if hasNode {
				candidateNodeID = existingNode.ID
			}

			var targetTun *store.Tunnel
			var matchedProxies []*store.Tunnel
			if isPerTunnel {
				t, ok := st.TunnelByID(perTunnelID)
				if !ok || t.OwnerID != authedUserID {
					return errNotFound("隧道不存在或无权访问，请检查账号密钥与隧道配置")
				}
				targetTun = t
			} else if len(hello.Proxies) > 0 {
				for _, p := range hello.Proxies {
					pname := strings.TrimSpace(p.Name)
					if pname == "" {
						return errBad("name", "客户端配置中存在空隧道名称")
					}
					var match *store.Tunnel
					for i := range st.Tunnels {
						t := &st.Tunnels[i]
						if t.OwnerID != authedUserID || !strings.EqualFold(t.Name, pname) {
							continue
						}
						if t.NodeID == "" || (candidateNodeID != "" && t.NodeID == candidateNodeID) {
							match = t
							break
						}
						return errConflict("name_conflict", fmt.Sprintf("隧道 [%s] 已绑定到其他客户端节点，拒绝抢占", pname))
					}
					if match == nil {
						return errNotFound(fmt.Sprintf("隧道 [%s] 未在当前账号面板中创建，禁止客户端私自创建或跨账号绑定隧道", pname))
					}
					matchedProxies = append(matchedProxies, match)
				}
			}

			if hasNode {
				existingNode.OS, existingNode.Arch, existingNode.Version = hello.OS, hello.Arch, hello.Version
				existingNode.LastSeen = &now
				existingNode.LastRemote = ip
				node = *existingNode
			} else {
				node = store.Node{
					ID:         store.NewID(),
					OwnerID:    authedUserID,
					Name:       name,
					TokenHash:  "", // shared-token nodes authenticate via user auth_token, never reusable per-node TokenHash
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
			for _, t := range matchedProxies {
				if t.NodeID != node.ID || t.ManagedByConfig {
					t.NodeID = node.ID
					t.ManagedByConfig = false
					t.UpdatedAt = now
					claimedChanged = true
				}
			}
			return nil
		}); err != nil {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: err.Error()})
			sess.Close()
			return
		}
		if created {
			s.lg.Info("node registered via config", "node", node.Name, "id", node.ID, "owner", authedUserID, "remote", ip)
			s.hub.publish(api.Event{Type: "node-joined", At: now, Message: node.Name, OwnerID: authedUserID})
		}
		if claimedChanged {
			s.reconcile()
			s.notifyOwner(authedUserID, "tunnel claimed: "+perTunnelID)
		}
	} else {
		if !s.joinAllowed(ip) {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "too many attempts, try later"})
			sess.Close()
			return
		}
		var ownerDisabled bool
		_ = s.st.View(func(st *store.State) error {
			if n, ok := st.NodeByID(hello.NodeID); ok {
				node, found = *n, true
				if u, uok := st.UserByID(n.OwnerID); uok && u.Disabled {
					ownerDisabled = true
				}
			}
			return nil
		})
		if !found || node.TokenHash == "" {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "unknown node; configure auth_token in client.toml / client.yaml or re-join"})
			sess.Close()
			return
		}
		if ownerDisabled {
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "账号已被停用，请联系管理员"})
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

	// Shared-token configs already validated and claimed their panel-owned
	// tunnels atomically above; legacy per-node configs with declared proxies
	// still pass through strict panel-only binding below.
	if !usingSharedToken && len(hello.Proxies) > 0 {
		if err := s.syncDeclarativeProxies(node.ID, hello.Proxies, false); err != nil {
			s.lg.Warn("bind client proxies failed", "node", node.Name, "err", err)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: err.Error()})
			sess.Close()
			return
		}
	}

	st := s.st.Snapshot()
	if err := proto.WriteFrame(ctrl, &proto.Frame{
		Type:          proto.TypeWelcome,
		NodeID:        node.ID,
		Revision:      st.Revision,
		LatestVersion: s.LatestClientVersion(),
	}); err != nil {
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
	s.hub.publish(api.Event{Type: "node-online", At: now, Message: node.Name, OwnerID: node.OwnerID})
	defer func() {
		s.hub.publish(api.Event{Type: "node-offline", At: time.Now().UTC(), Message: node.Name, OwnerID: node.OwnerID})
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
		var ownerID string
		_ = s.st.View(func(st *store.State) error {
			if t, ok := st.TunnelByID(tunnelID); ok {
				ownerID = t.OwnerID
			}
			return nil
		})
		s.hub.publish(api.Event{Type: "tunnel-target-down", At: at, Message: message, OwnerID: ownerID})
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

// syncDeclarativeProxies strictly binds client-declared proxies to EXISTING
// panel-created tunnels only. Clients are strictly prohibited from creating new
// tunnels or hijacking tunnels bound to other nodes; panel settings (Local,
// RemotePort, Hosts, Enabled, QuotaPaused, QuotaMB, BandwidthKB) always remain
// authoritative.
func (s *Server) syncDeclarativeProxies(nodeID string, proxies []proto.ProxySpec, _ bool) error {
	if len(proxies) == 0 {
		return nil
	}
	now := time.Now().UTC()
	var nodeOwner string
	changed, err := s.st.UpdateIf(func(st *store.State) (bool, error) {
		n, ok := st.NodeByID(nodeID)
		if !ok {
			return false, errNotFound("node not found")
		}
		nodeOwner = n.OwnerID
		mutated := false
		for _, p := range proxies {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				return false, errBad("name", "客户端配置中存在空隧道名称")
			}
			var existing *store.Tunnel
			for i := range st.Tunnels {
				t := &st.Tunnels[i]
				if (nodeOwner != "" && t.OwnerID != nodeOwner) || !strings.EqualFold(t.Name, name) {
					continue
				}
				if t.NodeID == nodeID || t.NodeID == "" {
					existing = t
					break
				}
				return false, errConflict("name_conflict", fmt.Sprintf("隧道 [%s] 已绑定到其他客户端节点，拒绝抢占", name))
			}
			if existing == nil {
				return false, errNotFound(fmt.Sprintf("隧道 [%s] 未在服务端面板创建，禁止客户端私自创建隧道，请先在面板创建后再启动", name))
			}
			if existing.NodeID != nodeID || existing.ManagedByConfig {
				existing.NodeID = nodeID
				existing.ManagedByConfig = false
				existing.UpdatedAt = now
				mutated = true
			}
		}
		return mutated, nil
	})
	if err != nil {
		return err
	}
	if changed {
		s.reconcile()
		s.notifyOwner(nodeOwner, fmt.Sprintf("bound %d panel tunnel(s) for node %s", len(proxies), nodeID))
	}
	return nil
}
