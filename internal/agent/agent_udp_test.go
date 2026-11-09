package agent

import (
	"encoding/binary"
	"io"
	"net"

	"github.com/solitudeit/nfr/internal/log"
	"testing"
	"time"
)

// startUDPEcho binds a UDP socket that echoes every datagram back.
func startUDPEcho(t *testing.T) *net.UDPAddr {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp echo: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteToUDP(buf[:n], src); err != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

// frameWriter prefixes payload with its 2 byte big endian length.
func frameWrite(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	if _, err := c.Write(buf); err != nil {
		t.Fatalf("frame write: %v", err)
	}
}

// frameRead reads one length prefixed packet.
func frameRead(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var hdr [2]byte
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("frame header: %v", err)
	}
	size := int(binary.BigEndian.Uint16(hdr[:]))
	body := make([]byte, size)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("frame body: %v", err)
	}
	return body
}

// TestUDPStreamEcho drives serveUDPStream through a net.Pipe: framed packets
// go in, must come back echoed by the local UDP service.
func TestUDPStreamEcho(t *testing.T) {
	echo := startUDPEcho(t)
	a := New(Options{Logger: log.New("error", "")})

	client, agentSide := net.Pipe()
	go a.serveUDPStream(agentSide, "t1", echo.String())
	defer client.Close()

	payload := []byte("hello over udp tunnel")
	frameWrite(t, client, payload)
	got := frameRead(t, client)
	if string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}

	// A second packet on the same visitor session must also round trip.
	frameWrite(t, client, []byte("second"))
	if got := frameRead(t, client); string(got) != "second" {
		t.Fatalf("second echo = %q", got)
	}
}

// TestUDPStreamBadLocal ensures a bogus destination fails cleanly instead of
// wedging the stream.
func TestUDPStreamBadLocal(t *testing.T) {
	a := New(Options{Logger: log.New("error", "")})
	client, agentSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		a.serveUDPStream(agentSide, "t1", "not-an-address")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveUDPStream did not return for an invalid local address")
	}
	client.Close()
}
