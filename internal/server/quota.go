package server

import (
	"time"

	"github.com/solitudeit/nfr/internal/store"
)

// Traffic quotas are enforced per calendar month. In-memory counters reset on
// restart, so the slow ticker folds the delta since the last pass into the
// persisted state; restarts lose at most one interval of accounting, which is
// fine for self-hosted "borrow my spare bandwidth" use. When a tunnel runs
// over its quota it is disabled and flagged QuotaPaused, and the guard
// re-enables it on the next month rollover — nobody has to remember to flip
// the switch themselves.

// applyUsage folds delta bytes into a tunnel's monthly usage, rolling the
// counter over (and un-pausing a quota pause) at month boundaries. It reports
// whether the tunnel is now over its quota.
func applyUsage(t *store.Tunnel, delta int64, month string) bool {
	if t.UsageMonth != month {
		t.UsageMonth = month
		t.UsedBytes = 0
		if t.QuotaPaused {
			t.QuotaPaused = false
			t.Enabled = true
		}
	}
	t.UsedBytes += delta
	return t.QuotaMB > 0 && t.UsedBytes > t.QuotaMB<<20
}

// flushQuotas persists the traffic deltas and disables tunnels over quota.
func (s *Server) flushQuotas() {
	snaps := s.met.Snapshot()
	if len(snaps) == 0 {
		return
	}
	now := time.Now().UTC()
	month := now.Format("2006-01")

	s.mu.Lock()
	deltas := make(map[string]int64, len(snaps))
	for _, sn := range snaps {
		total := sn.In + sn.Out
		base := s.flushed[sn.TunnelID]
		s.flushed[sn.TunnelID] = total
		if d := total - base; d > 0 {
			deltas[sn.TunnelID] = d
		}
	}
	s.mu.Unlock()
	if len(deltas) == 0 {
		return
	}

	type pausedTunnel struct{ id, node, name string }
	var paused []pausedTunnel
	var resumed []pausedTunnel
	_ = s.st.Update(func(st *store.State) error {
		for id, d := range deltas {
			t, ok := st.TunnelByID(id)
			if !ok {
				continue
			}
			wasPaused := t.QuotaPaused
			if applyUsage(t, d, month) && t.Enabled {
				t.Enabled = false
				t.QuotaPaused = true
				t.UpdatedAt = now
				paused = append(paused, pausedTunnel{id, t.NodeID, t.Name})
			} else if wasPaused && !t.QuotaPaused && t.Enabled {
				resumed = append(resumed, pausedTunnel{id, t.NodeID, t.Name})
			}
		}
		return nil
	})

	for _, p := range paused {
		s.dropLimit(p.id)
		s.reconcile()
		s.pushConfig(p.node)
		s.notify("quota exceeded: " + p.name + " paused until next month")
	}
	for _, p := range resumed {
		s.reconcile()
		s.pushConfig(p.node)
		s.notify("month rolled over: " + p.name + " resumed")
	}
}
