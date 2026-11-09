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
// same way. This mirrors frp's UDP model but reuses the stream plumbing the
// TCP tunnels already have.

const (
	udpIdleTimeout = 90 * time.Second // no local replies -> tear the session down
	udpMaxPacket   = 65_507           // largest possible UDP payload
)

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
		buf := make([]byte, 2+udpMaxPacket)
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

	// Local service -> server: local datagrams, framed.
	go func() {
		defer stop()
		buf := make([]byte, udpMaxPacket)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := conn.Read(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) || isTimeout(err) {
					return
				}
				return
			}
			frame := make([]byte, 2+n)
			binary.BigEndian.PutUint16(frame[:2], uint16(n))
			copy(frame[2:], buf[:n])
			if _, err := st.Write(frame); err != nil {
				return
			}
		}
	}()

	<-done
	_ = st.SetDeadline(time.Now())
	st.Close()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
