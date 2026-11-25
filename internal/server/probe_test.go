package server

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

func seedProbeServer(t *testing.T) (*Server, store.Node) {
	t.Helper()
	s, _ := newTestServer(t, nil)
	now := time.Now().UTC()
	node := store.Node{ID: "node-1", Name: "home-nas", CreatedAt: now}
	if err := s.st.Update(func(st *store.State) error {
		st.Nodes = append(st.Nodes, node)
		return nil
	}); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return s, node
}

func makeFakeSession(nodeID string) *agentSession {
	return &agentSession{
		nodeID:        nodeID,
		send:          make(chan *proto.Frame, 64),
		done:          make(chan struct{}),
		connected:     time.Now().UTC(),
		probeSem:      make(chan struct{}, probeMaxPerNode),
		probes:        make(map[string]chan probeReply),
		probeInflight: make(map[string]*inflightProbe),
	}
}

func TestProbeTunnelFastReturnStates(t *testing.T) {
	s, node := seedProbeServer(t)
	now := time.Now().UTC()
	if err := s.st.Update(func(st *store.State) error {
		st.Tunnels = []store.Tunnel{
			{ID: "t-await", Name: "await", Type: store.TunnelTCP, Local: "127.0.0.1:8080", RemotePort: 20001, Enabled: true, CreatedAt: now, UpdatedAt: now},
			{ID: "t-off", Name: "off", NodeID: node.ID, Type: store.TunnelTCP, Local: "127.0.0.1:8081", RemotePort: 20002, Enabled: false, CreatedAt: now, UpdatedAt: now},
			{ID: "t-node-off", Name: "nodeoff", NodeID: node.ID, Type: store.TunnelTCP, Local: "127.0.0.1:8082", RemotePort: 20003, Enabled: true, CreatedAt: now, UpdatedAt: now},
			{ID: "t-udp", Name: "dns", NodeID: node.ID, Type: store.TunnelUDP, Local: "127.0.0.1:5353", RemotePort: 20004, Enabled: true, CreatedAt: now, UpdatedAt: now},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed tunnels: %v", err)
	}

	ctx := context.Background()
	res, err := s.ProbeTunnel(ctx, "t-await")
	if err != nil || res.Status != "awaiting" {
		t.Fatalf("awaiting probe = (%+v, %v), want awaiting", res, err)
	}

	res, err = s.ProbeTunnel(ctx, "t-off")
	if err != nil || res.Status != "disabled" {
		t.Fatalf("disabled probe = (%+v, %v), want disabled", res, err)
	}

	res, err = s.ProbeTunnel(ctx, "t-node-off")
	if err != nil || res.Status != "offline" {
		t.Fatalf("offline probe = (%+v, %v), want offline", res, err)
	}

	// Bring node online and verify UDP returns honest unsupported without mutating targetDown.
	sess := makeFakeSession(node.ID)
	s.setSession(node.ID, sess)
	defer sess.close()
	s.markTargetDown(node.ID, "t-udp", "prior error", time.Now().Unix())

	res, err = s.ProbeTunnel(ctx, "t-udp")
	if err != nil || res.Status != "unsupported" || res.RTTMs != 0 || res.LocalMs != 0 {
		t.Fatalf("udp probe = (%+v, %v), want unsupported with 0 RTT", res, err)
	}
	if len(sess.send) != 0 {
		t.Fatalf("expected zero frames sent for UDP probe, got %d", len(sess.send))
	}
	s.mu.Lock()
	_, stillDown := s.targetDown["t-udp"]
	s.mu.Unlock()
	if !stillDown {
		t.Fatal("UDP probe should not clear or mutate targetDown")
	}
}

func TestProbeTunnelOKFailedBusyDisconnectTimeoutAndDedup(t *testing.T) {
	s, node := seedProbeServer(t)
	now := time.Now().UTC()
	if err := s.st.Update(func(st *store.State) error {
		st.Tunnels = []store.Tunnel{
			{ID: "t1", Name: "web", NodeID: node.ID, Type: store.TunnelTCP, Local: "127.0.0.1:8080", RemotePort: 20010, Enabled: true, CreatedAt: now, UpdatedAt: now},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed tunnel: %v", err)
	}

	// 1. OK clears targetDown and returns rtt_ms + local_ms.
	sess := makeFakeSession(node.ID)
	s.setSession(node.ID, sess)
	s.markTargetDown(node.ID, "t1", "old err", time.Now().Unix())

	go func() {
		f := <-sess.send
		if f.Type != proto.TypeProbe || f.Protocol != store.TunnelTCP || f.Message != "127.0.0.1:8080" {
			t.Errorf("unexpected probe frame: %+v", f)
			return
		}
		time.Sleep(5 * time.Millisecond)
		sess.deliverProbeResult(&proto.Frame{
			Type:   proto.TypeProbeResult,
			Code:   f.Code,
			Status: proto.ProbeOK,
			TS:     2,
		})
	}()

	res, err := s.ProbeTunnel(context.Background(), "t1")
	if err != nil || res.Status != "ok" || res.LocalMs != 2 || res.RTTMs < 2 {
		t.Fatalf("ok probe = (%+v, %v)", res, err)
	}
	s.mu.Lock()
	_, isDown := s.targetDown["t1"]
	s.mu.Unlock()
	if isDown {
		t.Fatal("ok probe did not clear targetDown")
	}

	// 2. Failed marks targetDown.
	go func() {
		f := <-sess.send
		sess.deliverProbeResult(&proto.Frame{
			Type:    proto.TypeProbeResult,
			Code:    f.Code,
			Status:  proto.ProbeFailed,
			TS:      1,
			Message: "connection refused",
		})
	}()
	res, err = s.ProbeTunnel(context.Background(), "t1")
	if err != nil || res.Status != "failed" || res.Message != "connection refused" {
		t.Fatalf("failed probe = (%+v, %v)", res, err)
	}
	s.mu.Lock()
	_, isDown = s.targetDown["t1"]
	s.mu.Unlock()
	if !isDown {
		t.Fatal("failed probe did not mark targetDown")
	}
	s.clearTargetDown("t1")

	// 3. Agent ProbeBusy never marks targetDown and never closes session.
	go func() {
		f := <-sess.send
		sess.deliverProbeResult(&proto.Frame{
			Type:    proto.TypeProbeResult,
			Code:    f.Code,
			Status:  proto.ProbeBusy,
			Message: "probe busy",
		})
	}()
	res, err = s.ProbeTunnel(context.Background(), "t1")
	if err != nil || res.Status != "busy" {
		t.Fatalf("agent busy probe = (%+v, %v)", res, err)
	}
	s.mu.Lock()
	_, isDown = s.targetDown["t1"]
	s.mu.Unlock()
	if isDown {
		t.Fatal("busy probe must NOT mark targetDown")
	}

	// 4. Send-queue headroom protection: filling sess.send past cap-8 returns
	// busy WITHOUT closing the live agentSession!
	for len(sess.send) < cap(sess.send)-probeQueueHeadroom {
		sess.send <- &proto.Frame{Type: proto.TypePing}
	}
	res, err = s.ProbeTunnel(context.Background(), "t1")
	if err != nil || res.Status != "busy" {
		t.Fatalf("queue-full probe = (%+v, %v), want busy", res, err)
	}
	select {
	case <-sess.done:
		t.Fatal("trySendProbe closed live agentSession on full queue!")
	default:
	}
	// Drain ping frames for subsequent subtests.
	for len(sess.send) > 0 {
		<-sess.send
	}

	// 5. Concurrent singleflight dedup: 2 simultaneous calls emit only 1 frame.
	var frameCount atomic.Int32
	go func() {
		f := <-sess.send
		frameCount.Add(1)
		time.Sleep(30 * time.Millisecond)
		sess.deliverProbeResult(&proto.Frame{
			Type:   proto.TypeProbeResult,
			Code:   f.Code,
			Status: proto.ProbeOK,
			TS:     3,
		})
	}()
	var wg sync.WaitGroup
	outs := make([]api.ProbeResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, _ := s.ProbeTunnel(context.Background(), "t1")
			outs[idx] = r
		}(i)
	}
	wg.Wait()
	if frameCount.Load() != 1 {
		t.Fatalf("singleflight expected 1 probe frame, sent %d", frameCount.Load())
	}
	if outs[0].Status != "ok" || outs[1].Status != "ok" {
		t.Fatalf("singleflight results mismatch: %+v", outs)
	}

	// 6. Old client timeout returns timeout cleanly.
	go func() { <-sess.send }() // consume and ignore like an older agent
	res, err = s.probeTunnelWithTimeout(context.Background(), "t1", 60*time.Millisecond)
	if err != nil || res.Status != "timeout" {
		t.Fatalf("old client probe = (%+v, %v), want timeout", res, err)
	}

	// 7. Mid-probe disconnect immediately wakes via <-a.done returning offline.
	go func() {
		<-sess.send
		time.Sleep(20 * time.Millisecond)
		sess.close()
	}()
	start := time.Now()
	res, err = s.probeTunnelWithTimeout(context.Background(), "t1", 3*time.Second)
	elapsed := time.Since(start)
	if err != nil || res.Status != "offline" {
		t.Fatalf("disconnect probe = (%+v, %v), want offline", res, err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("disconnect probe waited %v instead of immediate wakeup", elapsed)
	}
}

func TestProbeAllTunnelsAndRealTCPListener(t *testing.T) {
	// Start a real local TCP listener to verify real dial semantics via ProbeAllTunnels.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	s, node := seedProbeServer(t)
	now := time.Now().UTC()
	if err := s.st.Update(func(st *store.State) error {
		st.Tunnels = []store.Tunnel{
			{ID: "t-ok", Name: "ok", NodeID: node.ID, Type: store.TunnelTCP, Local: ln.Addr().String(), RemotePort: 20020, Enabled: true, CreatedAt: now, UpdatedAt: now},
			{ID: "t-udp", Name: "udp", NodeID: node.ID, Type: store.TunnelUDP, Local: "127.0.0.1:9999", RemotePort: 20021, Enabled: true, CreatedAt: now, UpdatedAt: now},
			{ID: "t-dis", Name: "dis", NodeID: node.ID, Type: store.TunnelTCP, Local: "127.0.0.1:9998", RemotePort: 20022, Enabled: false, CreatedAt: now, UpdatedAt: now},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sess := makeFakeSession(node.ID)
	s.setSession(node.ID, sess)
	defer sess.close()
	go func() {
		for {
			select {
			case f := <-sess.send:
				if f.Type == proto.TypeProbe {
					c, derr := net.DialTimeout("tcp", f.Message, time.Second)
					if derr != nil {
						sess.deliverProbeResult(&proto.Frame{Type: proto.TypeProbeResult, Code: f.Code, Status: proto.ProbeFailed, Message: derr.Error()})
					} else {
						_ = c.Close()
						sess.deliverProbeResult(&proto.Frame{Type: proto.TypeProbeResult, Code: f.Code, Status: proto.ProbeOK, TS: 1})
					}
				}
			case <-sess.done:
				return
			}
		}
	}()

	results := s.ProbeAllTunnels(context.Background())
	if len(results) != 3 {
		t.Fatalf("probe-all len = %d, want 3", len(results))
	}
	if results[0].TunnelID != "t-ok" || results[0].Status != "ok" {
		t.Fatalf("result[0] = %+v, want ok", results[0])
	}
	if results[1].TunnelID != "t-udp" || results[1].Status != "unsupported" {
		t.Fatalf("result[1] = %+v, want unsupported", results[1])
	}
	if results[2].TunnelID != "t-dis" || results[2].Status != "disabled" {
		t.Fatalf("result[2] = %+v, want disabled", results[2])
	}
}
