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

type flushedBytes struct {
	in  int64
	out int64
}

// unflushedTrafficForOwner returns live in-memory inbound/outbound byte deltas
// not yet folded into persisted User.TotalInBytes / User.TotalOutBytes.
func (s *Server) unflushedTrafficForOwner(st *store.State, ownerID string) (in, out int64) {
	if s.met == nil || st == nil || ownerID == "" {
		return 0, 0
	}
	owned := make(map[string]struct{})
	for _, t := range st.Tunnels {
		if t.OwnerID == ownerID {
			owned[t.ID] = struct{}{}
		}
	}
	if len(owned) == 0 {
		return 0, 0
	}
	snaps := s.met.LiveSnapshot()
	s.mu.Lock()
	for _, sn := range snaps {
		if _, ok := owned[sn.TunnelID]; !ok {
			continue
		}
		base := s.flushed[sn.TunnelID]
		if d := sn.In - base.in; d > 0 {
			in += d
		}
		if d := sn.Out - base.out; d > 0 {
			out += d
		}
	}
	s.mu.Unlock()
	return in, out
}

// flushQuotas persists traffic deltas into both tunnel and user usage counters
// (including per-user cumulative inbound/outbound totals), pausing tunnels when
// either their own quota or their owner's monthly quota is exceeded, and
// resuming paused tunnels on month rollover.
func (s *Server) flushQuotas() {
	snaps := s.met.LiveSnapshot()
	now := time.Now().UTC()
	month := now.Format("2006-01")

	s.mu.Lock()
	deltas := make(map[string]flushedBytes, len(snaps))
	for _, sn := range snaps {
		base := s.flushed[sn.TunnelID]
		dIn := sn.In - base.in
		dOut := sn.Out - base.out
		if dIn < 0 {
			dIn = 0
		}
		if dOut < 0 {
			dOut = 0
		}
		s.flushed[sn.TunnelID] = flushedBytes{in: sn.In, out: sn.Out}
		if dIn > 0 || dOut > 0 {
			deltas[sn.TunnelID] = flushedBytes{in: dIn, out: dOut}
		}
	}
	s.mu.Unlock()

	type pausedTunnel struct{ id, node, name, owner string }
	var paused []pausedTunnel
	var resumed []pausedTunnel
	_, _ = s.st.UpdateIf(func(st *store.State) (bool, error) {
		changed := false
		userDeltas := make(map[string]flushedBytes)
		for id, d := range deltas {
			t, ok := st.TunnelByID(id)
			if !ok {
				continue
			}
			changed = true
			if t.OwnerID != "" {
				cur := userDeltas[t.OwnerID]
				cur.in += d.in
				cur.out += d.out
				userDeltas[t.OwnerID] = cur
			}
			wasPaused := t.QuotaPaused
			if applyUsage(t, d.in+d.out, month) && t.Enabled {
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

		// Fold owner traffic into per-user cumulative in/out totals + monthly
		// usage, and enforce account-level monthly quotas across all tunnels.
		for i := range st.Users {
			u := &st.Users[i]
			d := userDeltas[u.ID]
			totalDelta := d.in + d.out
			needRoll := u.UsageMonth != "" && u.UsageMonth != month
			if totalDelta == 0 && !needRoll {
				continue
			}
			changed = true
			u.TotalInBytes += d.in
			u.TotalOutBytes += d.out
			over, rolled := applyUserUsage(u, totalDelta, month)
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
