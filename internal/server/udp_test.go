package server

import (
	"net"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/store"
)

func TestUDPSessionCapEvictsOldest(t *testing.T) {
	oldCap := maxUDPSessionsPerTunnel
	maxUDPSessionsPerTunnel = 3
	defer func() { maxUDPSessionsPerTunnel = oldCap }()

	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}

	tun := store.Tunnel{ID: "u1", Name: "dns", NodeID: "n1", Type: store.TunnelUDP}
	// Seed 3 sessions with increasing timestamps.
	now := time.Now()
	var sOld *udpSession
	for i := 0; i < 3; i++ {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000 + i}
		sess := &udpSession{
			srv:    srv,
			tunnel: tun,
			src:    addr,
			stream: c1,
			ctr:    srv.met.For(tun.ID),
			sendCh: make(chan udpPacket, udpSendQueueCap),
			done:   make(chan struct{}),
			last:   now.Add(time.Duration(i) * time.Second),
		}
		if i == 0 {
			sOld = sess
		}
		srv.udpSess[tun.ID+"|"+addr.String()] = sess
	}

	// Simulate eviction check on a 4th visitor arriving (even when openStream
	// fails because node is offline, the oldest session must be evicted to
	// maintain the cap).
	newAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49999}
	srv.udpDeliver(nil, newAddr, tun, []byte("ping"))

	srv.mu.Lock()
	remaining := len(srv.udpSess)
	_, stillHasOld := srv.udpSess[tun.ID+"|"+sOld.src.String()]
	srv.mu.Unlock()
	if remaining != 2 || stillHasOld {
		t.Fatalf("expected oldest session evicted (remaining=2, stillHasOld=false), got remaining=%d stillHasOld=%v", remaining, stillHasOld)
	}
}

func TestUDPEnqueueNeverBlocksWhenQueueFull(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	sess := &udpSession{
		srv:    srv,
		tunnel: store.Tunnel{ID: "u2", Name: "wg"},
		src:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51820},
		stream: c1,
		ctr:    srv.met.For("u2"),
		sendCh: make(chan udpPacket, 2),
		done:   make(chan struct{}),
		last:   time.Now(),
	}
	// Do NOT start writeLoop; fill queue beyond capacity and ensure enqueue returns immediately.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			sess.enqueue([]byte("datagram"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("udpSession.enqueue blocked on full queue")
	}
	if got := sess.ctr.Errs.Load(); got != 8 {
		t.Fatalf("expected 8 dropped datagrams recorded as errs, got %d", got)
	}
}
