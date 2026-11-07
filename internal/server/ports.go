package server

import (
	"fmt"
	"net"
	"time"

	"github.com/nfr-tunnel/nfr/internal/api"
	"github.com/nfr-tunnel/nfr/internal/auth"
	"github.com/nfr-tunnel/nfr/internal/store"
)

// leaseTTL is how long a reserved port survives while a form is filled in.
const leaseTTL = 10 * time.Minute

// AllocatePort reserves a free remote port for ten minutes.
func (s *Server) AllocatePort(tunnelID string) (api.AllocateResponse, error) {
	var out api.AllocateResponse
	err := s.st.Update(func(st *store.State) error {
		now := time.Now().UTC()
		st.PruneLeases(now)
		port, err := pickPort(st, tunnelID)
		if err != nil {
			return err
		}
		lease := store.Lease{
			ID:        store.NewID(),
			Port:      port,
			TunnelID:  tunnelID,
			ExpiresAt: now.Add(leaseTTL),
		}
		st.Leases = append(st.Leases, lease)
		out = api.AllocateResponse{Port: port, LeaseID: lease.ID, ExpiresAt: lease.ExpiresAt}
		return nil
	})
	return out, err
}

// pickPort scans the configured range for a port that is neither registered
// nor leased nor currently bound by another process.
func pickPort(st *store.State, excludeTunnel string) (int, error) {
	lo, hi := st.Settings.PortRange[0], st.Settings.PortRange[1]
	if lo <= 0 || hi < lo {
		lo, hi = 20000, 29999
	}
	taken := make(map[int]bool, len(st.Tunnels)+len(st.Leases))
	for _, t := range st.Tunnels {
		if t.ID != excludeTunnel && t.RemotePort > 0 {
			taken[t.RemotePort] = true
		}
	}
	for _, l := range st.Leases {
		taken[l.Port] = true
	}
	// Start somewhere random so concurrent nodes do not race for the same
	// low port, then wrap around the whole range.
	span := hi - lo + 1
	start := int(auth.RandomBytes(1)[0]) % span
	for i := 0; i < span; i++ {
		p := lo + (start+i)%span
		if taken[p] || !canListen(p) {
			continue
		}
		return p, nil
	}
	return 0, errConflict("port_range_exhausted", fmt.Sprintf("no free port in range %d-%d", lo, hi))
}

// canListen reports whether a wildcard bind on port succeeds right now.
func canListen(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// consumeLease validates and removes a reservation, returning an error the
// console can show when the port moved on.
func consumeLease(st *store.State, leaseID string, port int, tunnelID string) error {
	if leaseID == "" {
		return nil
	}
	now := time.Now().UTC()
	for i := range st.Leases {
		l := st.Leases[i]
		if l.ID != leaseID {
			continue
		}
		if now.After(l.ExpiresAt) {
			return errConflict("lease_expired", "the reserved port expired, request a new one")
		}
		if l.Port != port {
			return errConflict("lease_mismatch", "the reserved port does not match the requested port")
		}
		if l.TunnelID != "" && l.TunnelID != tunnelID {
			return errConflict("lease_mismatch", "the reserved port belongs to another tunnel")
		}
		st.Leases = append(st.Leases[:i], st.Leases[i+1:]...)
		return nil
	}
	return errConflict("lease_missing", "the reserved port is unknown or already used")
}
