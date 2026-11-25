package agent

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// The server sends one yamux stream per visitor address; every datagram the
// visitor sent is framed with a 2 byte big endian length prefix. The agent
// owns a single UDP socket towards the local service for the lifetime of the
// stream: framed bytes go out as datagrams, replies come back framed the
// same way.
const (
	udpIdleTimeout  = 90 * time.Second // no local replies -> tear the session down
	udpWriteTimeout = 5 * time.Second
	udpMaxPacket    = 65_507 // largest possible UDP payload
)

var agentUDPBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 2+udpMaxPacket)
		return &b
	},
}

// serveUDPStream proxies one visitor session between the server stream and
// the local UDP service. It returns when either side goes quiet or breaks.
func (a *Agent) serveUDPStream(st net.Conn, tunnelID, local string) {
	target, err := net.ResolveUDPAddr("udp", local)
	if err != nil {
		a.lg.Warn("bad udp local address", "tunnel", tunnelID, "local", local, "err", err)
		return
	}
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		a.lg.Warn("cannot reach local udp service", "tunnel", tunnelID, "local", local, "err", err)
		return
	}
	defer conn.Close()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	// Server -> local service: framed visitor datagrams.
	go func() {
		defer stop()
		bp := agentUDPBufPool.Get().(*[]byte)
		defer agentUDPBufPool.Put(bp)
		buf := *bp
		for {
			_ = st.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			if _, err := io.ReadFull(st, buf[:2]); err != nil {
				return
			}
			size := int(binary.BigEndian.Uint16(buf[:2]))
			if size == 0 || size > udpMaxPacket {
				return
			}
			if _, err := io.ReadFull(st, buf[2:2+size]); err != nil {
				return
			}
			if _, err := conn.Write(buf[2 : 2+size]); err != nil {
				return
			}
		}
	}()

	// Local service -> server: local datagrams, framed in-place without extra allocs.
	go func() {
		defer stop()
		bp := agentUDPBufPool.Get().(*[]byte)
		defer agentUDPBufPool.Put(bp)
		buf := *bp
		for {
			_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := conn.Read(buf[2:])
			if err != nil {
				if errors.Is(err, net.ErrClosed) || isTimeout(err) {
					return
				}
				return
			}
			binary.BigEndian.PutUint16(buf[:2], uint16(n))
			_ = st.SetWriteDeadline(time.Now().Add(udpWriteTimeout))
			if _, err := st.Write(buf[:2+n]); err != nil {
				return
			}
		}
	}()

	<-done
	_ = conn.SetDeadline(time.Now())
	_ = st.SetDeadline(time.Now())
	conn.Close()
	st.Close()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
