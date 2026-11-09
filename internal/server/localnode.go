package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/version"
)

// localNodeID is the node every control node ships with: the machine running
// NFR is itself an endpoint, so a tunnel can point at a service on this host
// without installing an agent first. The node is synthetic and never enters
// the store, which keeps seedSettings able to recognise a fresh data dir.
const localNodeID = "local"

// localNode renders the built-in node for the API and the panel. It is always
// online: nothing has to connect for it to exist.
func (s *Server) localNode(st store.State) api.Node {
	n := api.Node{
		ID: localNodeID, Name: st.Name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version.Version, Online: true, Local: true, CreatedAt: s.started,
	}
	if n.Name == "" {
		n.Name = "本机"
	}
	for _, t := range st.Tunnels {
		if t.NodeID == localNodeID {
			n.Tunnels++
		}
	}
	return n
}

// localStream dials the published service on this host directly. There is no
// agent hop to speak to, so the stream is the local connection itself and the
// stream header is only used to find the tunnel.
func (s *Server) localStream(h *proto.StreamHeader) (net.Conn, error) {
	var t store.Tunnel
	found := false
	_ = s.st.View(func(st *store.State) error {
		if v, ok := st.TunnelByID(h.Tunnel); ok {
			t, found = *v, true
		}
		return nil
	})
	if !found || !t.Enabled {
		return nil, fmt.Errorf("tunnel not found")
	}
	if t.Type == store.TunnelUDP {
		return s.localUDPStream(t)
	}
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial("tcp", t.Local)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// localUDPStream hands the ingress one end of an in-memory pipe; the other end
// runs the same framing the agent would run for a remote node.
func (s *Server) localUDPStream(t store.Tunnel) (net.Conn, error) {
	ingress, local := net.Pipe()
	go s.serveLocalUDP(local, t)
	return ingress, nil
}

// serveLocalUDP is the built-in agent's UDP relay: it unwraps the framed
// datagrams the ingress writes, forwards them to the local service and frames
// the replies back. Both directions go quiet after udpIdleTimeout.
func (s *Server) serveLocalUDP(st net.Conn, t store.Tunnel) {
	target, err := net.ResolveUDPAddr("udp", t.Local)
	if err != nil {
		s.lg.Warn("bad udp local address", "tunnel", t.Name, "local", t.Local, "err", err)
		st.Close()
		return
	}
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		s.lg.Warn("cannot reach local udp service", "tunnel", t.Name, "local", t.Local, "err", err)
		st.Close()
		return
	}
	defer conn.Close()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	// Ingress -> local service: framed visitor datagrams.
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

	// Local service -> ingress: replies, framed.
	go func() {
		defer stop()
		buf := make([]byte, udpMaxPacket)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := conn.Read(buf)
			if err != nil {
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
