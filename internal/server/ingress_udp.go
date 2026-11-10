package server

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"github.com/solitudeit/nfr/internal/metrics"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

// UDP tunnels forward datagrams, not connections. One public socket serves
// every visitor: each visitor address is pinned to one yamux stream for as
// long as replies keep coming back, which mirrors how a NAT maps an outbound
// flow. The stream payload uses a 2 byte big endian length prefix per packet,
// so the agent can recover datagram boundaries inside the byte stream.
//
// Session liveness is the reply loop's read deadline, so nothing pings here:
// yamux keepalive keeps the underlying link healthy and a silent visitor
// simply re-sessions with its next packet.
const (
	udpIdleTimeout = 90 * time.Second // close a visitor session after this much silence
	udpMaxPacket   = 65_507           // largest possible UDP payload
)

// serveUDPPort accepts datagrams on one public tunnel port and fans them out
// to per-visitor agent streams. The tunnel is looked up per packet so
// toggling or deleting it takes effect immediately.
func (s *Server) serveUDPPort(port int, conn *net.UDPConn, _ store.Tunnel) {
	buf := make([]byte, udpMaxPacket)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.lg.Debug("udp read error", "port", port, "err", err)
			continue
		}
		t, ok := s.tunnelUDPByPort(port)
		if !ok || !t.Enabled {
			continue
		}
		s.udpDeliver(conn, src, t, buf[:n])
	}
}

// tunnelUDPByPort finds the enabled UDP tunnel bound to a public port.
func (s *Server) tunnelUDPByPort(port int) (store.Tunnel, bool) {
	var out store.Tunnel
	okf := false
	_ = s.st.View(func(st *store.State) error {
		for _, t := range st.Tunnels {
			if t.Type == store.TunnelUDP && t.RemotePort == port {
				out, okf = t, true
				return nil
			}
		}
		return nil
	})
	return out, okf
}

// udpSession is one visitor address pinned to one agent stream.
type udpSession struct {
	srv    *Server
	tunnel store.Tunnel
	src    *net.UDPAddr
	conn   *net.UDPConn
	stream net.Conn
	ctr    *metrics.Counter
	lim    *tunnelLimit // nil when the tunnel has no bandwidth cap
	last   time.Time    // guarded by srv.mu
	closed bool
}

// udpDeliver routes one datagram from a visitor into (or into) the session
// for its source address, creating the agent stream on first contact.
func (s *Server) udpDeliver(conn *net.UDPConn, src *net.UDPAddr, t store.Tunnel, payload []byte) {
	key := t.ID + "|" + src.String()
	s.mu.Lock()
	sess, ok := s.udpSess[key]
	if ok {
		sess.last = time.Now()
	}
	s.mu.Unlock()
	if ok {
		sess.send(payload)
		return
	}

	ctr := s.met.For(t.ID)
	ctr.Conns.Add(1)
	ctr.Active.Add(1)
	stream, err := s.openStream(t.NodeID, &proto.StreamHeader{Tunnel: t.ID, Remote: src.String()})
	if err == nil {
		s.clearTargetDown(t.ID)
	}
	if err != nil {
		ctr.Active.Add(-1)
		ctr.Errs.Add(1)
		s.lg.Debug("udp tunnel offline", "tunnel", t.Name, "err", err)
		return
	}
	sess = &udpSession{
		srv:    s,
		tunnel: t,
		src:    src,
		conn:   conn,
		stream: stream,
		ctr:    ctr,
		lim:    s.limiterFor(t),
		last:   time.Now(),
	}
	s.mu.Lock()
	// A concurrent packet may have raced us to create the session; the older
	// one wins and this stream is discarded.
	if old, exists := s.udpSess[key]; exists {
		s.mu.Unlock()
		ctr.Active.Add(-1)
		stream.Close()
		old.send(payload)
		return
	}
	s.udpSess[key] = sess
	s.mu.Unlock()

	go sess.replyLoop()
	sess.send(payload)
}

// send frames one visitor datagram onto the agent stream.
func (sess *udpSession) send(payload []byte) {
	if len(payload) > udpMaxPacket {
		return
	}
	if sess.lim != nil {
		sess.lim.up.take(len(payload))
	}
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	if _, err := sess.stream.Write(buf); err != nil {
		sess.close()
		return
	}
	sess.ctr.In.Add(int64(len(payload)))
}

// replyLoop reads framed datagrams from the agent and writes them back to the
// visitor address. Read deadlines double as an idle timeout: with no replies
// the session is torn down and the visitor re-sessions on the next packet.
func (sess *udpSession) replyLoop() {
	defer sess.close()
	buf := make([]byte, 2+udpMaxPacket)
	for {
		_ = sess.stream.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		if _, err := io.ReadFull(sess.stream, buf[:2]); err != nil {
			return
		}
		size := int(binary.BigEndian.Uint16(buf[:2]))
		if size == 0 || size > udpMaxPacket {
			return
		}
		if _, err := io.ReadFull(sess.stream, buf[2:2+size]); err != nil {
			return
		}
		if sess.lim != nil {
			sess.lim.down.take(size)
		}
		if _, err := sess.conn.WriteToUDP(buf[2:2+size], sess.src); err != nil {
			return
		}
		sess.ctr.Out.Add(int64(size))
		sess.srv.mu.Lock()
		sess.last = time.Now()
		sess.srv.mu.Unlock()
	}
}

// close tears the session down exactly once.
func (sess *udpSession) close() {
	sess.srv.mu.Lock()
	if sess.closed {
		sess.srv.mu.Unlock()
		return
	}
	sess.closed = true
	key := sess.tunnel.ID + "|" + sess.src.String()
	if cur, ok := sess.srv.udpSess[key]; ok && cur == sess {
		delete(sess.srv.udpSess, key)
	}
	sess.srv.mu.Unlock()

	sess.stream.Close()
	sess.ctr.Active.Add(-1)
}

// pruneUDPSessions is called from the slow ticker to report and clean up
// sessions that have been quiet for too long (their reply loop exits via the
// read deadline on its own; this pass only guards against drift).
func (s *Server) pruneUDPSessions() {
	cutoff := time.Now().Add(-2 * udpIdleTimeout)
	var stale []*udpSession
	s.mu.Lock()
	for _, sess := range s.udpSess {
		if sess.last.Before(cutoff) {
			stale = append(stale, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range stale {
		sess.close()
	}
}

// closeUDPSessions drops every session, e.g. when the server shuts down.
func (s *Server) closeUDPSessions() {
	s.mu.Lock()
	sessions := make([]*udpSession, 0, len(s.udpSess))
	for _, sess := range s.udpSess {
		sessions = append(sessions, sess)
	}
	s.udpSess = map[string]*udpSession{}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
}
