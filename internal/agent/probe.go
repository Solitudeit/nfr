package agent

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/proto"
)

const (
	maxConcurrentProbes = 4
	probeDialTimeout    = 2 * time.Second
)

// handleProbe dispatches a server-initiated local reachability probe into a
// bounded worker pool so slow local dials never block the control read loop
// or heartbeat replies.
func (a *Agent) handleProbe(ctx context.Context, f *proto.Frame) {
	if f == nil || f.Code == "" {
		return
	}
	sem := a.probeSem
	if sem == nil {
		sem = make(chan struct{}, maxConcurrentProbes)
	}
	select {
	case sem <- struct{}{}:
		go func() {
			defer func() { <-sem }()
			a.runProbe(ctx, f)
		}()
	default:
		// Pool is saturated; report transient busy without blocking the read loop.
		go func() {
			_ = a.sendFrame(&proto.Frame{
				Type:    proto.TypeProbeResult,
				Code:    f.Code,
				Status:  proto.ProbeBusy,
				Message: "probe busy",
			})
		}()
	}
}

func (a *Agent) runProbe(ctx context.Context, f *proto.Frame) {
	local := strings.TrimSpace(f.Message)
	if local == "" {
		_ = a.sendFrame(&proto.Frame{
			Type:    proto.TypeProbeResult,
			Code:    f.Code,
			Status:  proto.ProbeFailed,
			Message: "empty local address",
		})
		return
	}

	if strings.EqualFold(strings.TrimSpace(f.Protocol), "udp") {
		if _, err := net.ResolveUDPAddr("udp", local); err != nil {
			_ = a.sendFrame(&proto.Frame{
				Type:    proto.TypeProbeResult,
				Code:    f.Code,
				Status:  proto.ProbeFailed,
				Message: err.Error(),
			})
			return
		}
		_ = a.sendFrame(&proto.Frame{
			Type:    proto.TypeProbeResult,
			Code:    f.Code,
			Status:  proto.ProbeOK,
			Message: "udp address valid",
		})
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	defer cancel()
	start := time.Now()
	dialer := &net.Dialer{Timeout: probeDialTimeout}
	conn, err := dialer.DialContext(dialCtx, "tcp", local)
	elapsedMs := time.Since(start).Milliseconds()
	if err != nil {
		_ = a.sendFrame(&proto.Frame{
			Type:    proto.TypeProbeResult,
			Code:    f.Code,
			Status:  proto.ProbeFailed,
			TS:      elapsedMs,
			Message: err.Error(),
		})
		return
	}
	_ = conn.Close()
	_ = a.sendFrame(&proto.Frame{
		Type:   proto.TypeProbeResult,
		Code:   f.Code,
		Status: proto.ProbeOK,
		TS:     elapsedMs,
	})
}
