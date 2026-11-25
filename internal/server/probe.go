package server

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
)

const (
	probeMaxPerNode    = 4
	probeQueueHeadroom = 8
	defaultProbeTimeout = 3 * time.Second
	probeSemWait        = 200 * time.Millisecond
)

type probeReply struct {
	status  string
	localMs int64
	message string
}

type inflightProbe struct {
	done chan struct{}
	res  api.ProbeResult
}

func (a *agentSession) ensureProbeState() {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.probeSem == nil {
		a.probeSem = make(chan struct{}, probeMaxPerNode)
	}
	if a.probes == nil {
		a.probes = make(map[string]chan probeReply)
	}
	if a.probeInflight == nil {
		a.probeInflight = make(map[string]*inflightProbe)
	}
}

// trySendProbe queues a probe frame without ever closing the agent session.
// It leaves probeQueueHeadroom slots reserved for config and heartbeat frames
// so probe bursts can never starve or disconnect a live node.
func (a *agentSession) trySendProbe(f *proto.Frame) bool {
	select {
	case <-a.done:
		return false
	default:
	}
	if cap(a.send)-len(a.send) <= probeQueueHeadroom {
		return false
	}
	select {
	case a.send <- f:
		return true
	case <-a.done:
		return false
	default:
		return false
	}
}

func (a *agentSession) deliverProbeResult(f *proto.Frame) {
	if f == nil || f.Code == "" {
		return
	}
	a.probeMu.Lock()
	ch, ok := a.probes[f.Code]
	if ok {
		delete(a.probes, f.Code)
	}
	a.probeMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- probeReply{status: f.Status, localMs: f.TS, message: f.Message}:
	default:
	}
}

// ProbeTunnel tests whether the local service behind one tunnel is reachable
// from its edge agent. Fast-returns for awaiting, disabled, offline, and UDP
// tunnels without sending misleading frames.
func (s *Server) ProbeTunnel(ctx context.Context, tunnelID string) (api.ProbeResult, error) {
	return s.probeTunnelWithTimeout(ctx, tunnelID, defaultProbeTimeout)
}

func (s *Server) probeTunnelWithTimeout(ctx context.Context, tunnelID string, timeout time.Duration) (api.ProbeResult, error) {
	st := s.st.Snapshot()
	t, ok := st.TunnelByID(tunnelID)
	if !ok {
		return api.ProbeResult{}, errNotFound("tunnel")
	}
	if t.NodeID == "" {
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "awaiting",
			Message:  "隧道尚未绑定在线节点",
		}, nil
	}
	if !t.Enabled {
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "disabled",
			Message:  "隧道已停用，启用后可测通",
		}, nil
	}
	a := s.sessionFor(t.NodeID)
	if a == nil {
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "offline",
			Message:  "所属节点当前离线",
		}, nil
	}
	if t.Type == store.TunnelUDP {
		if _, err := net.ResolveUDPAddr("udp", t.Local); err != nil {
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "failed",
				Message:  "UDP 本地地址格式无效: " + err.Error(),
			}, nil
		}
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "unsupported",
			Message:  "UDP 为无连接协议，不支持主动握手探测（节点在线、配置格式有效）",
		}, nil
	}

	a.ensureProbeState()

	// Coalesce duplicate concurrent probe requests targeting the same tunnel
	// on the same live agent session.
	a.probeMu.Lock()
	if existing, ok := a.probeInflight[t.ID]; ok {
		a.probeMu.Unlock()
		select {
		case <-existing.done:
			return existing.res, nil
		case <-a.done:
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "offline",
				Message:  "探测期间节点断开连接",
			}, nil
		case <-ctx.Done():
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "timeout",
				Message:  "探测已取消",
			}, ctx.Err()
		}
	}
	call := &inflightProbe{done: make(chan struct{})}
	a.probeInflight[t.ID] = call
	a.probeMu.Unlock()

	res, err := s.executeProbe(ctx, a, *t, timeout)
	a.probeMu.Lock()
	if cur, ok := a.probeInflight[t.ID]; ok && cur == call {
		delete(a.probeInflight, t.ID)
	}
	a.probeMu.Unlock()
	call.res = res
	close(call.done)
	return res, err
}

func (s *Server) executeProbe(ctx context.Context, a *agentSession, t store.Tunnel, timeout time.Duration) (api.ProbeResult, error) {
	semTimer := time.NewTimer(probeSemWait)
	select {
	case a.probeSem <- struct{}{}:
		semTimer.Stop()
		defer func() { <-a.probeSem }()
	case <-a.done:
		semTimer.Stop()
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "offline",
			Message:  "探测期间节点断开连接",
		}, nil
	case <-ctx.Done():
		semTimer.Stop()
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "timeout",
			Message:  "探测已取消",
		}, ctx.Err()
	case <-semTimer.C:
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "busy",
			Message:  "节点探测繁忙，请稍后重试",
		}, nil
	}

	probeID := store.NewID()
	resCh := make(chan probeReply, 1)
	a.probeMu.Lock()
	a.probes[probeID] = resCh
	a.probeMu.Unlock()
	defer func() {
		a.probeMu.Lock()
		delete(a.probes, probeID)
		a.probeMu.Unlock()
	}()

	start := time.Now()
	frame := &proto.Frame{
		Type:     proto.TypeProbe,
		Code:     probeID,
		NodeID:   t.ID,
		Message:  t.Local,
		Protocol: t.Type,
	}
	if !a.trySendProbe(frame) {
		select {
		case <-a.done:
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "offline",
				Message:  "所属节点当前离线",
			}, nil
		default:
		}
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "busy",
			Message:  "节点探测繁忙，请稍后重试",
		}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case rep := <-resCh:
		rttMs := time.Since(start).Milliseconds()
		switch rep.status {
		case proto.ProbeOK:
			s.clearTargetDown(t.ID)
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "ok",
				RTTMs:    rttMs,
				LocalMs:  rep.localMs,
				Message:  "本地服务已连通",
			}, nil
		case proto.ProbeBusy:
			// Never mark targetDown on transient worker saturation.
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "busy",
				Message:  "节点探测繁忙，请稍后重试",
			}, nil
		default:
			msg := rep.message
			if msg == "" {
				msg = "连接本地服务失败"
			}
			s.markTargetDown(t.NodeID, t.ID, msg, time.Now().Unix())
			return api.ProbeResult{
				TunnelID: t.ID,
				Status:   "failed",
				RTTMs:    rttMs,
				LocalMs:  rep.localMs,
				Message:  msg,
			}, nil
		}
	case <-a.done:
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "offline",
			Message:  "探测期间节点断开连接",
		}, nil
	case <-ctx.Done():
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "timeout",
			Message:  "探测已取消",
		}, ctx.Err()
	case <-timer.C:
		return api.ProbeResult{
			TunnelID: t.ID,
			Status:   "timeout",
			Message:  "客户端版本较旧或响应超时，请升级 nfrc",
		}, nil
	}
}

// ProbeAllTunnels probes every configured tunnel with bounded per-node
// concurrency (max probeMaxPerNode per node), returning results in tunnel order.
func (s *Server) ProbeAllTunnels(ctx context.Context) []api.ProbeResult {
	st := s.st.Snapshot()
	results := make([]api.ProbeResult, len(st.Tunnels))
	if len(st.Tunnels) == 0 {
		return results
	}

	var mu sync.Mutex
	nodeSems := make(map[string]chan struct{})
	getSem := func(nodeID string) chan struct{} {
		if nodeID == "" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		ch, ok := nodeSems[nodeID]
		if !ok {
			ch = make(chan struct{}, probeMaxPerNode)
			nodeSems[nodeID] = ch
		}
		return ch
	}

	var wg sync.WaitGroup
	for i, tun := range st.Tunnels {
		wg.Add(1)
		go func(idx int, t store.Tunnel) {
			defer wg.Done()
			if sem := getSem(t.NodeID); sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results[idx] = api.ProbeResult{
						TunnelID: t.ID,
						Status:   "timeout",
						Message:  "探测已取消",
					}
					return
				}
			}
			res, err := s.ProbeTunnel(ctx, t.ID)
			if err != nil && res.Status == "" {
				res = api.ProbeResult{
					TunnelID: t.ID,
					Status:   "failed",
					Message:  err.Error(),
				}
			}
			results[idx] = res
		}(i, tun)
	}
	wg.Wait()
	return results
}
