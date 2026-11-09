package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

// hub fan-outs change events to console subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[chan api.Event]struct{}
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
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
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
	}
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = 256
	return cfg
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

func (s *Server) joinFailed(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[ip]
	if a == nil || time.Now().After(a.until) {
		a = &attempt{}
		s.attempts[ip] = a
	}
	a.count++
	if a.count >= 5 {
		a.until = time.Now().Add(15 * time.Minute)
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
		return nil
	})
	if err != nil {
		s.joinFailed(ip)
		_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: errMsg})
		return
	}
	s.lg.Info("node joined", "node", name, "id", nodeID, "remote", ip)
	s.hub.publish(api.Event{Type: "node-joined", At: now, Message: name})
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
	_ = s.st.View(func(st *store.State) error {
		if n, ok := st.NodeByID(hello.NodeID); ok {
			node, found = *n, true
		}
		return nil
	})
	if !found {
		_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeError, Message: "unknown node, run nfr join again"})
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

	st := s.st.Snapshot()
	if err := proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeWelcome, NodeID: node.ID, Revision: st.Revision}); err != nil {
		sess.Close()
		return
	}

	a := &agentSession{
		nodeID:    node.ID,
		sess:      sess,
		send:      make(chan *proto.Frame, 64),
		done:      make(chan struct{}),
		remote:    ip,
		connected: time.Now().UTC(),
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

	// Serialised writer.
	go func() {
		for {
			select {
			case f := <-a.send:
				if err := proto.WriteFrame(ctrl, f); err != nil {
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
		}
	}
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
	for _, t := range tunnels {
		specs = append(specs, proto.TunnelSpec{ID: t.ID, Name: t.Name, Type: t.Type, Local: t.Local})
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
}

func (s *Server) dropSession(nodeID string, a *agentSession) {
	s.mu.Lock()
	if cur, ok := s.agents[nodeID]; ok && cur == a {
		delete(s.agents, nodeID)
	}
	s.mu.Unlock()
}

// openStream dials one stream to the named node's agent. The built-in node has
// no agent, so its streams go straight to the local service.
func (s *Server) openStream(nodeID string, h *proto.StreamHeader) (net.Conn, error) {
	if nodeID == localNodeID {
		return s.localStream(h)
	}
	a := s.sessionFor(nodeID)
	if a == nil || a.sess == nil {
		return nil, fmt.Errorf("node offline")
	}
	st, err := a.sess.Open()
	if err != nil {
		return nil, err
	}
	if err := proto.WriteStreamHeader(st, h); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}
