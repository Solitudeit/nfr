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
// counter over (and un-pausing a tunnel-level quota pause) at month boundaries.
// It reports whether the tunnel is now over its own quota.
func applyUsage(t *store.Tunnel, delta int64, month string) bool {
	if t.UsageMonth != month {
		t.UsageMonth = month
		t.UsedBytes = 0
		if t.QuotaPaused && t.QuotaReason != "user" {
			t.QuotaPaused = false
			t.QuotaReason = ""
			t.Enabled = true
		}
	}
	t.UsedBytes += delta
	return t.QuotaMB > 0 && t.UsedBytes > t.QuotaMB<<20
}

// applyUserUsage folds delta bytes into an account's monthly usage. Because
// UsedBytes lives on the User record, deleting or recreating tunnels never
// resets monthly consumption.
func applyUserUsage(u *store.User, delta int64, month string) (over bool, rolled bool) {
	if u.UsageMonth != month {
		u.UsageMonth = month
		u.UsedBytes = 0
		rolled = true
	}
	u.UsedBytes += delta
	over = u.Role != store.RoleAdmin && u.MonthlyQuotaMB > 0 && u.UsedBytes > u.MonthlyQuotaMB<<20
	return over, rolled
}

// flushQuotas persists traffic deltas into both tunnel and user usage counters,
// pausing tunnels when either their own quota or their owner's monthly quota is
// exceeded, and resuming paused tunnels on month rollover.
func (s *Server) flushQuotas() {
	snaps := s.met.LiveSnapshot()
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

	type pausedTunnel struct{ id, node, name, owner string }
	var paused []pausedTunnel
	var resumed []pausedTunnel
	_, _ = s.st.UpdateIf(func(st *store.State) (bool, error) {
		changed := false
		userDeltas := make(map[string]int64)
		for id, d := range deltas {
			t, ok := st.TunnelByID(id)
			if !ok {
				continue
			}
			changed = true
			if t.OwnerID != "" {
				userDeltas[t.OwnerID] += d
			}
			wasPaused := t.QuotaPaused
			if applyUsage(t, d, month) && t.Enabled {
				t.Enabled = false
				t.QuotaPaused = true
				t.QuotaReason = "tunnel"
				t.UpdatedAt = now
				paused = append(paused, pausedTunnel{id, t.NodeID, t.Name, t.OwnerID})
			} else if wasPaused && !t.QuotaPaused && t.Enabled {
				resumed = append(resumed, pausedTunnel{id, t.NodeID, t.Name, t.OwnerID})
			}
		}

		// Check month rollovers on any tunnel currently paused by tunnel quota
		// even if it had zero traffic in this tick.
		for i := range st.Tunnels {
			t := &st.Tunnels[i]
			if t.QuotaPaused && t.QuotaReason != "user" && t.UsageMonth != month {
				t.UsageMonth = month
				t.UsedBytes = 0
				t.QuotaPaused = false
				t.QuotaReason = ""
				t.Enabled = true
				t.UpdatedAt = now
				changed = true
				resumed = append(resumed, pausedTunnel{t.ID, t.NodeID, t.Name, t.OwnerID})
			}
		}

		// Fold owner traffic into per-user monthly usage and enforce account-level
		// monthly quotas across all of that user's tunnels.
		for i := range st.Users {
			u := &st.Users[i]
			d := userDeltas[u.ID]
			needRoll := u.UsageMonth != "" && u.UsageMonth != month
			if d == 0 && !needRoll {
				continue
			}
			changed = true
			over, rolled := applyUserUsage(u, d, month)
			if rolled {
				for ti := range st.Tunnels {
					t := &st.Tunnels[ti]
					if t.OwnerID != u.ID || !t.QuotaPaused || t.QuotaReason != "user" {
						continue
					}
					if u.Disabled {
						continue
					}
					if t.QuotaMB > 0 && t.UsageMonth == month && t.UsedBytes > t.QuotaMB<<20 {
						continue
					}
					t.QuotaPaused = false
					t.QuotaReason = ""
					t.Enabled = true
					t.UpdatedAt = now
					resumed = append(resumed, pausedTunnel{t.ID, t.NodeID, t.Name, t.OwnerID})
				}
			}
			if over {
				for ti := range st.Tunnels {
					t := &st.Tunnels[ti]
					if t.OwnerID != u.ID || !t.Enabled {
						continue
					}
					t.Enabled = false
					t.QuotaPaused = true
					t.QuotaReason = "user"
					t.UpdatedAt = now
					paused = append(paused, pausedTunnel{t.ID, t.NodeID, t.Name, t.OwnerID})
				}
			}
		}
		return changed, nil
	})

	if len(paused) > 0 || len(resumed) > 0 {
		for _, p := range paused {
			s.clearLimitBucket(p.id)
		}
		s.reconcile()
		for _, p := range paused {
			s.pushConfig(p.node)
			s.notifyOwner(p.owner, "quota exceeded: "+p.name+" paused until next month")
		}
		for _, p := range resumed {
			s.pushConfig(p.node)
			s.notifyOwner(p.owner, "month rolled over: "+p.name+" resumed")
		}
	}
}
