package server

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
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
const (
	udpIdleTimeout  = 90 * time.Second // close a visitor session after this much silence
	udpWriteTimeout = 5 * time.Second  // bound stalled stream writes
	udpMaxPacket    = 65_507           // largest possible UDP payload
	udpSendQueueCap = 64               // buffered datagrams per visitor session
)

// maxUDPSessionsPerTunnel bounds active visitor sessions per UDP tunnel to
// protect against spoofed source-port floods. Overridable in tests.
var maxUDPSessionsPerTunnel = 2048

// udpBufPool recycles framed packet buffers ([2-byte len][payload]) to avoid
// per-datagram heap allocations under high-PPS UDP workloads (WireGuard, DNS).
var udpBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 2+udpMaxPacket)
		return &b
	},
}

type udpPacket struct {
	buf *[]byte
	n   int // total framed bytes (2 + payload len)
}

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

// udpSession is one visitor address pinned to one agent stream. Outbound writes
// run in a dedicated writer goroutine fed by a bounded queue so one stalled
// stream can never block serveUDPPort for other visitors on the same port.
type udpSession struct {
	srv    *Server
	tunnel store.Tunnel
	src    *net.UDPAddr
	conn   *net.UDPConn
	stream net.Conn
	ctr    *metrics.Counter
	lim    *tunnelLimit // nil when the tunnel has no bandwidth cap
	sendCh chan udpPacket
	done   chan struct{}
	last   time.Time // guarded by srv.mu
	closed bool
}

// udpDeliver routes one datagram from a visitor into the session for its source
// address, creating the agent stream on first contact.
func (s *Server) udpDeliver(conn *net.UDPConn, src *net.UDPAddr, t store.Tunnel, payload []byte) {
	if len(payload) > udpMaxPacket {
		return
	}
	key := t.ID + "|" + src.String()
	s.mu.Lock()
	sess, ok := s.udpSess[key]
	if ok {
		sess.last = time.Now()
	}
	s.mu.Unlock()
	if ok {
		sess.enqueue(payload)
		return
	}

	// Enforce per-tunnel session cap before dialing a new agent stream: if the
	// tunnel is already at capacity, evict its oldest idle session.
	var evict *udpSession
	s.mu.Lock()
	count := 0
	for _, cur := range s.udpSess {
		if cur.tunnel.ID == t.ID {
			count++
			if evict == nil || cur.last.Before(evict.last) {
				evict = cur
			}
		}
	}
	if count < maxUDPSessionsPerTunnel {
		evict = nil
	}
	s.mu.Unlock()
	if evict != nil {
		evict.close()
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
		sendCh: make(chan udpPacket, udpSendQueueCap),
		done:   make(chan struct{}),
		last:   time.Now(),
	}
	s.mu.Lock()
	// A concurrent packet may have raced us to create the session; the older
	// one wins and this stream is discarded.
	if old, exists := s.udpSess[key]; exists {
		s.mu.Unlock()
		ctr.Active.Add(-1)
		stream.Close()
		old.enqueue(payload)
		return
	}
	s.udpSess[key] = sess
	s.mu.Unlock()

	go sess.writeLoop()
	go sess.replyLoop()
	sess.enqueue(payload)
}

// enqueue copies payload into a pooled framed buffer and hands it to the
// session writer without blocking the public UDP reader loop.
func (sess *udpSession) enqueue(payload []byte) {
	bp := udpBufPool.Get().(*[]byte)
	buf := *bp
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	pkt := udpPacket{buf: bp, n: 2 + len(payload)}
	select {
	case <-sess.done:
		udpBufPool.Put(bp)
	case sess.sendCh <- pkt:
	default:
		// Queue full for this visitor flow; drop datagram cleanly (UDP semantics)
		// rather than stalling other visitors on the shared public socket.
		udpBufPool.Put(bp)
		sess.ctr.Errs.Add(1)
	}
}

func (sess *udpSession) writeLoop() {
	defer sess.close()
	for {
		select {
		case <-sess.done:
			// Drain any queued buffers back into the pool.
			for {
				select {
				case pkt := <-sess.sendCh:
					udpBufPool.Put(pkt.buf)
				default:
					return
				}
			}
		case pkt := <-sess.sendCh:
			payloadLen := pkt.n - 2
			if sess.lim != nil {
				sess.lim.up.take(payloadLen)
			}
			_ = sess.stream.SetWriteDeadline(time.Now().Add(udpWriteTimeout))
			_, err := sess.stream.Write((*pkt.buf)[:pkt.n])
			udpBufPool.Put(pkt.buf)
			if err != nil {
				return
			}
			sess.ctr.In.Add(int64(payloadLen))
		}
	}
}

// replyLoop reads framed datagrams from the agent and writes them back to the
// visitor address. Read deadlines double as an idle timeout: with no replies
// the session is torn down and the visitor re-sessions on the next packet.
func (sess *udpSession) replyLoop() {
	defer sess.close()
	bp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bp)
	buf := *bp
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
	close(sess.done)
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

// pruneUDPSessionsForActive immediately closes active visitor UDP sessions
// belonging to tunnels that were disabled, quota-paused, or deleted.
func (s *Server) pruneUDPSessionsForActive(active map[int]store.Tunnel) {
	activeIDs := make(map[string]bool, len(active))
	for _, t := range active {
		activeIDs[t.ID] = true
	}
	var drop []*udpSession
	s.mu.Lock()
	for _, sess := range s.udpSess {
		if !activeIDs[sess.tunnel.ID] {
			drop = append(drop, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range drop {
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
