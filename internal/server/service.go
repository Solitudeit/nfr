package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/federation"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
)

func defaultOwnerID(st *store.State, ownerID string) string {
	if ownerID != "" {
		return ownerID
	}
	if admin, ok := st.AdminUser(); ok {
		return admin.ID
	}
	return ""
}

func checkUserMonthlyQuota(st *store.State, ownerID string, now time.Time) error {
	u, ok := st.UserByID(ownerID)
	if !ok || u.Role == store.RoleAdmin {
		return nil
	}
	if u.Disabled {
		return errForbidden("user_disabled", "该账号已被管理员停用")
	}
	month := now.Format("2006-01")
	if u.MonthlyQuotaMB > 0 && u.UsageMonth == month && u.UsedBytes > u.MonthlyQuotaMB<<20 {
		return errBad("quota_exceeded", "账号本月流量配额已用尽，请联系管理员提升配额或等待下月重置")
	}
	return nil
}

// CreateTunnel validates and stores a new tunnel owned by the default admin.
func (s *Server) CreateTunnel(req api.TunnelRequest) (api.Tunnel, error) {
	return s.CreateTunnelForOwner("", req)
}

// CreateTunnelForOwner validates and stores a new tunnel owned by ownerID.
func (s *Server) CreateTunnelForOwner(ownerID string, req api.TunnelRequest) (api.Tunnel, error) {
	var created store.Tunnel
	err := s.st.Update(func(st *store.State) error {
		oid := defaultOwnerID(st, ownerID)
		t, err := buildTunnel(st, oid, req, nil, st.Settings.Domain, false)
		if err != nil {
			return err
		}
		if t.Enabled {
			if err := checkUserMonthlyQuota(st, oid, time.Now().UTC()); err != nil {
				return err
			}
		}
		t.ID = store.NewID()
		t.OwnerID = oid
		t.CreatedAt = time.Now().UTC()
		t.UpdatedAt = t.CreatedAt
		st.Tunnels = append(st.Tunnels, *t)
		created = *t
		return nil
	})
	if err != nil {
		return api.Tunnel{}, err
	}
	s.reconcile()
	s.notifyOwner(created.OwnerID, "tunnel created: "+created.Name)
	if v, ok := s.Tunnel(created.ID); ok {
		return v, nil
	}
	return api.Tunnel{}, nil
}

// CreateStandaloneTunnel creates a panel-owned tunnel directly on this control
// node owned by the default admin.
func (s *Server) CreateStandaloneTunnel(req api.TunnelRequest) (api.Tunnel, error) {
	return s.CreateStandaloneTunnelForOwner("", req)
}

// CreateStandaloneTunnelForOwner creates a panel-owned tunnel owned by ownerID
// without requiring a pre-registered edge node.
func (s *Server) CreateStandaloneTunnelForOwner(ownerID string, req api.TunnelRequest) (api.Tunnel, error) {
	req.NodeID = ""
	var created store.Tunnel
	err := s.st.Update(func(st *store.State) error {
		oid := defaultOwnerID(st, ownerID)
		t, err := buildTunnel(st, oid, req, nil, st.Settings.Domain, true)
		if err != nil {
			return err
		}
		if t.Enabled {
			if err := checkUserMonthlyQuota(st, oid, time.Now().UTC()); err != nil {
				return err
			}
		}
		t.ID = store.NewID()
		t.OwnerID = oid
		t.ManagedByConfig = false
		t.CreatedAt = time.Now().UTC()
		t.UpdatedAt = t.CreatedAt
		st.Tunnels = append(st.Tunnels, *t)
		created = *t
		return nil
	})
	if err != nil {
		return api.Tunnel{}, err
	}
	s.reconcile()
	s.notifyOwner(created.OwnerID, "tunnel created: "+created.Name)
	if v, ok := s.Tunnel(created.ID); ok {
		return v, nil
	}
	return api.Tunnel{}, nil
}

// KickTunnel sends a kick control frame to the active client carrying tunnel id.
func (s *Server) KickTunnel(id string) (bool, error) {
	return s.KickTunnelForOwner("", id)
}

// KickTunnelForOwner kicks the active client carrying tunnel id if owned by
// ownerID (or any tunnel when ownerID is empty).
func (s *Server) KickTunnelForOwner(ownerID, id string) (bool, error) {
	st := s.st.Snapshot()
	t, ok := st.TunnelByID(id)
	if !ok || (ownerID != "" && t.OwnerID != ownerID) {
		return false, errNotFound("tunnel not found")
	}
	if t.NodeID == "" {
		return false, nil
	}
	a := s.sessionFor(t.NodeID)
	if a == nil {
		return false, nil
	}
	a.kick("强制下线")
	s.notifyOwner(t.OwnerID, "tunnel kicked: "+t.Name)
	return true, nil
}

// ClientsDir returns the directory scanned for downloadable nfrc binaries.
func (s *Server) ClientsDir() string {
	if v := strings.TrimSpace(os.Getenv("NFR_CLIENTS_DIR")); v != "" {
		return v
	}
	if s.cfg.DataDir != "" {
		return filepath.Join(s.cfg.DataDir, "clients")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "clients")
	}
	return "clients"
}

// CreateTunnelAwaiting creates a tunnel that has no node yet and mints a join
// code bound to it for the default admin.
func (s *Server) CreateTunnelAwaiting(req api.TunnelRequest, ttl time.Duration) (api.Tunnel, api.JoinCodeResponse, error) {
	return s.CreateTunnelAwaitingForOwner("", req, ttl)
}

// CreateTunnelAwaitingForOwner creates an awaiting tunnel + join code owned by ownerID.
func (s *Server) CreateTunnelAwaitingForOwner(ownerID string, req api.TunnelRequest, ttl time.Duration) (api.Tunnel, api.JoinCodeResponse, error) {
	if strings.TrimSpace(req.NodeID) != "new" {
		return api.Tunnel{}, api.JoinCodeResponse{}, errBad("node_id", "awaiting tunnels are created with node_id \"new\"")
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if ttl > time.Hour {
		ttl = time.Hour
	}

	var created store.Tunnel
	var code string
	var expiresAt time.Time
	err := s.st.Update(func(st *store.State) error {
		oid := defaultOwnerID(st, ownerID)
		if req.RemotePort == 0 {
			port, err := pickPort(st, "", store.TunnelNetwork(req.Type))
			if err != nil {
				return err
			}
			req.RemotePort = port
		}
		if strings.TrimSpace(req.Name) == "" {
			name := fmt.Sprintf("tunnel-%d", req.RemotePort)
			for _, o := range st.Tunnels {
				if o.OwnerID == oid && strings.EqualFold(o.Name, name) {
					name = fmt.Sprintf("tunnel-%d-%s", req.RemotePort, store.NewID()[:4])
					break
				}
			}
			req.Name = name
		}
		req.NodeID = ""
		t, err := buildTunnel(st, oid, req, nil, st.Settings.Domain, true)
		if err != nil {
			return err
		}
		if t.Enabled {
			if err := checkUserMonthlyQuota(st, oid, time.Now().UTC()); err != nil {
				return err
			}
		}
		t.ID = store.NewID()
		t.OwnerID = oid
		t.CreatedAt = time.Now().UTC()
		t.UpdatedAt = t.CreatedAt
		st.Tunnels = append(st.Tunnels, *t)
		created = *t

		now := time.Now().UTC()
		code = auth.NewJoinCode()
		expiresAt = now.Add(ttl)
		st.PruneJoins(now)
		st.Joins = append(st.Joins, auth.JoinCode{
			Code:      code,
			OwnerID:   oid,
			NodeName:  t.Name,
			TunnelID:  t.ID,
			CreatedAt: now,
			ExpiresAt: expiresAt,
		})
		return nil
	})
	if err != nil {
		return api.Tunnel{}, api.JoinCodeResponse{}, err
	}
	host, warning := s.joinHost()
	resp := api.JoinCodeResponse{
		Code:      code,
		ExpiresAt: expiresAt,
		Command:   fmt.Sprintf("nfrc join %s %s", host, code),
		Warning:   warning,
	}
	s.reconcile()
	s.notifyOwner(created.OwnerID, "awaiting tunnel created: "+created.Name)
	if v, ok := s.Tunnel(created.ID); ok {
		return v, resp, nil
	}
	return api.Tunnel{}, resp, nil
}

// UpdateTunnel replaces the editable fields of a tunnel.
func (s *Server) UpdateTunnel(id string, req api.TunnelRequest) (api.Tunnel, error) {
	return s.UpdateTunnelForOwner("", id, req)
}

// UpdateTunnelForOwner replaces editable fields of a tunnel owned by ownerID
// (or any tunnel when ownerID is empty).
func (s *Server) UpdateTunnelForOwner(ownerID, id string, req api.TunnelRequest) (api.Tunnel, error) {
	var tunOwner string
	err := s.st.Update(func(st *store.State) error {
		cur, ok := st.TunnelByID(id)
		if !ok || (ownerID != "" && cur.OwnerID != ownerID) {
			return errNotFound("tunnel not found")
		}
		tunOwner = cur.OwnerID
		allowNoNode := cur.NodeID == "" && strings.TrimSpace(req.NodeID) == ""
		next, err := buildTunnel(st, cur.OwnerID, req, cur, st.Settings.Domain, allowNoNode)
		if err != nil {
			return err
		}
		if next.Enabled && !cur.Enabled {
			if err := checkUserMonthlyQuota(st, cur.OwnerID, time.Now().UTC()); err != nil {
				return err
			}
		}
		cur.Name = next.Name
		cur.NodeID = next.NodeID
		cur.Type = next.Type
		cur.Local = next.Local
		cur.RemotePort = next.RemotePort
		cur.Hosts = next.Hosts
		cur.Enabled = next.Enabled
		cur.BandwidthKB = next.BandwidthKB
		cur.QuotaMB = next.QuotaMB
		cur.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return api.Tunnel{}, err
	}
	s.clearTargetDown(id)
	s.clearLimitBucket(id)
	s.reconcile()
	s.notifyOwner(tunOwner, "tunnel updated: "+id)
	if v, ok := s.Tunnel(id); ok {
		return v, nil
	}
	return api.Tunnel{}, nil
}

// DeleteTunnel removes a tunnel, closes its public port, and cleans up any
// synthetic per-tunnel client node ("t-<id>") bound exclusively to it.
func (s *Server) DeleteTunnel(id string) error {
	return s.DeleteTunnelForOwner("", id)
}

// DeleteTunnelForOwner removes a tunnel owned by ownerID (or any tunnel when
// ownerID is empty).
func (s *Server) DeleteTunnelForOwner(ownerID, id string) error {
	// Fold any unflushed bytes into User.UsedBytes BEFORE removing the tunnel
	// and dropping its metrics counter so users cannot bypass monthly quotas
	// by deleting tunnels inside the 30-second flush interval.
	s.flushQuotas()
	s.clearTargetDown(id)
	var name string
	var tunOwner string
	var kickNodeID string
	err := s.st.Update(func(st *store.State) error {
		for i := range st.Tunnels {
			if st.Tunnels[i].ID == id {
				if ownerID != "" && st.Tunnels[i].OwnerID != ownerID {
					return errNotFound("tunnel not found")
				}
				name = st.Tunnels[i].Name
				tunOwner = st.Tunnels[i].OwnerID
				boundNodeID := st.Tunnels[i].NodeID
				st.Tunnels = append(st.Tunnels[:i], st.Tunnels[i+1:]...)
				leases := st.Leases[:0]
				for _, l := range st.Leases {
					if l.TunnelID != id {
						leases = append(leases, l)
					}
				}
				st.Leases = leases
				store.DeleteJoinsByTunnel(st, id)
				if boundNodeID != "" {
					for ni := range st.Nodes {
						if st.Nodes[ni].ID == boundNodeID && st.Nodes[ni].Name == "t-"+id {
							kickNodeID = boundNodeID
							st.Nodes = append(st.Nodes[:ni], st.Nodes[ni+1:]...)
							break
						}
					}
				}
				return nil
			}
		}
		return errNotFound("tunnel not found")
	})
	if err != nil {
		return err
	}
	if kickNodeID != "" {
		if a := s.sessionFor(kickNodeID); a != nil {
			a.kick("隧道已删除")
		}
	}
	s.met.Drop(id)
	s.dropLimit(id)
	s.reconcile()
	s.notifyOwner(tunOwner, "tunnel deleted: "+name)
	return nil
}

// RegenerateTunnelCode replaces the join code bound to an awaiting tunnel.
func (s *Server) RegenerateTunnelCode(id string) (api.JoinCodeResponse, error) {
	return s.RegenerateTunnelCodeForOwner("", id)
}

// RegenerateTunnelCodeForOwner replaces the join code bound to an awaiting tunnel owned by ownerID.
func (s *Server) RegenerateTunnelCodeForOwner(ownerID, id string) (api.JoinCodeResponse, error) {
	var code string
	var expiresAt time.Time
	err := s.st.Update(func(st *store.State) error {
		t, ok := st.TunnelByID(id)
		if !ok || (ownerID != "" && t.OwnerID != ownerID) {
			return errNotFound("tunnel not found")
		}
		if t.NodeID != "" {
			return errConflict("already_joined", "this tunnel already has a node")
		}
		store.DeleteJoinsByTunnel(st, id)
		now := time.Now().UTC()
		st.PruneJoins(now)
		code = auth.NewJoinCode()
		expiresAt = now.Add(30 * time.Minute)
		st.Joins = append(st.Joins, auth.JoinCode{
			Code:      code,
			OwnerID:   t.OwnerID,
			NodeName:  t.Name,
			TunnelID:  t.ID,
			CreatedAt: now,
			ExpiresAt: expiresAt,
		})
		return nil
	})
	if err != nil {
		return api.JoinCodeResponse{}, err
	}
	host, warning := s.joinHost()
	return api.JoinCodeResponse{
		Code:      code,
		ExpiresAt: expiresAt,
		Command:   fmt.Sprintf("nfrc join %s %s", host, code),
		Warning:   warning,
	}, nil
}

// TunnelJoinCode returns the live join code bound to an awaiting tunnel.
func (s *Server) TunnelJoinCode(id string) (api.JoinCodeResponse, bool) {
	return s.TunnelJoinCodeForOwner("", id)
}

// TunnelJoinCodeForOwner returns the live join code bound to an awaiting tunnel owned by ownerID.
func (s *Server) TunnelJoinCodeForOwner(ownerID, id string) (api.JoinCodeResponse, bool) {
	st := s.st.Snapshot()
	if ownerID != "" {
		t, ok := st.TunnelByID(id)
		if !ok || t.OwnerID != ownerID {
			return api.JoinCodeResponse{}, false
		}
	}
	jc, ok := store.FindJoinCodeByTunnel(&st, id)
	if !ok {
		return api.JoinCodeResponse{}, false
	}
	host, warning := s.joinHost()
	return api.JoinCodeResponse{
		Code:      jc.Code,
		ExpiresAt: jc.ExpiresAt,
		Command:   fmt.Sprintf("nfrc join %s %s", host, jc.Code),
		Warning:   warning,
	}, true
}

// SetTunnelEnabled flips the enabled switch.
func (s *Server) SetTunnelEnabled(id string, on bool) error {
	return s.SetTunnelEnabledForOwner("", id, on)
}

// SetTunnelEnabledForOwner flips the enabled switch for a tunnel owned by
// ownerID (or any tunnel when ownerID is empty), enforcing non-bypassable
// monthly user quotas when re-enabling.
func (s *Server) SetTunnelEnabledForOwner(ownerID, id string, on bool) error {
	s.clearTargetDown(id)
	var tunOwner string
	err := s.st.Update(func(st *store.State) error {
		t, ok := st.TunnelByID(id)
		if !ok || (ownerID != "" && t.OwnerID != ownerID) {
			return errNotFound("tunnel not found")
		}
		if on {
			if err := checkUserMonthlyQuota(st, t.OwnerID, time.Now().UTC()); err != nil {
				return err
			}
		}
		tunOwner = t.OwnerID
		t.Enabled = on
		if on {
			t.QuotaPaused = false
			t.QuotaReason = ""
		}
		t.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	s.reconcile()
	s.notifyOwner(tunOwner, "tunnel toggled: "+id)
	return nil
}

// buildTunnel normalises and validates a tunnel description. It mutates the
// lease list only when a valid lease is supplied.
func buildTunnel(st *store.State, ownerID string, req api.TunnelRequest, existing *store.Tunnel, domain string, allowAwaiting bool) (*store.Tunnel, error) {
	t := &store.Tunnel{Enabled: true, OwnerID: ownerID}
	name := strings.TrimSpace(req.Name)
	nodeID := strings.TrimSpace(req.NodeID)
	typ := strings.TrimSpace(req.Type)
	local := strings.TrimSpace(req.Local)
	port := req.RemotePort
	hosts := req.Hosts

	var owner *store.User
	if ownerID != "" {
		owner, _ = st.UserByID(ownerID)
	}
	if existing == nil && owner != nil && owner.Role != store.RoleAdmin && owner.MaxTunnels > 0 {
		count := 0
		for _, o := range st.Tunnels {
			if o.OwnerID == ownerID {
				count++
			}
		}
		if count >= owner.MaxTunnels {
			return nil, errBad("quota_tunnels", fmt.Sprintf("已达到账号隧道数量上限 (%d 条)", owner.MaxTunnels))
		}
	}

	if existing != nil {
		t.ID = existing.ID
		if name == "" {
			name = existing.Name
		}
		if nodeID == "" {
			nodeID = existing.NodeID
		}
		if typ == "" {
			typ = existing.Type
		}
		if local == "" {
			local = existing.Local
		}
		if port == 0 && !req.ClearRemotePort {
			port = existing.RemotePort
		}
		if hosts == nil {
			hosts = existing.Hosts
		}
		t.Enabled = existing.Enabled
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}

	// Bandwidth and quota follow the pointer protocol: nil keeps the current
	// value (also the default for creation), 0 explicitly removes the cap.
	bw := 0
	var quota int64 = 0
	if existing != nil {
		bw = existing.BandwidthKB
		quota = existing.QuotaMB
	}
	if req.BandwidthKB != nil {
		bw = *req.BandwidthKB
	}
	if req.QuotaMB != nil {
		quota = *req.QuotaMB
	}
	if bw < 0 || bw > 4<<20 {
		return nil, errBad("bandwidth_kb", "bandwidth must be between 0 and 4194304 KB/s")
	}
	if quota < 0 || quota > 10<<20 {
		return nil, errBad("quota_mb", "quota must be between 0 and 10485760 MB")
	}
	if owner != nil && owner.Role != store.RoleAdmin && owner.MaxBandwidthKB > 0 && bw > owner.MaxBandwidthKB {
		return nil, errBad("quota_bandwidth", fmt.Sprintf("带宽上限不能超过账号配额 (%d KB/s)", owner.MaxBandwidthKB))
	}
	t.BandwidthKB = bw
	t.QuotaMB = quota

	if name == "" {
		return nil, errBad("name", "tunnel name is required")
	}
	if len(name) > 63 {
		return nil, errBad("name", "tunnel name is too long")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return nil, errBad("name", "tunnel name may only contain letters, digits, dot, dash and underscore")
		}
	}
	if allowAwaiting && nodeID == "" {
		// The awaiting flow: the tunnel is created before any node exists.
	} else {
		n, ok := st.NodeByID(nodeID)
		if !ok || (ownerID != "" && n.OwnerID != "" && n.OwnerID != ownerID) {
			return nil, errBad("node_id", "choose a node that has joined")
		}
	}
	if typ == "" {
		typ = store.TunnelTCP
	}
	if typ != store.TunnelTCP && typ != store.TunnelHTTP && typ != store.TunnelUDP {
		return nil, errBad("type", "type must be tcp, http or udp")
	}
	normLocal, err := normalizeLocal(local)
	if err != nil {
		return nil, errBad("local", err.Error())
	}
	t.Name, t.NodeID, t.Type, t.Local = name, nodeID, typ, normLocal

	for _, o := range st.Tunnels {
		if existing != nil && o.ID == existing.ID {
			continue
		}
		if ownerID != "" && o.OwnerID != ownerID {
			continue
		}
		if strings.EqualFold(o.Name, name) {
			return nil, errConflict("name_taken", "another tunnel already uses that name")
		}
	}

	switch typ {
	case store.TunnelTCP:
		p, err := bindPort(st, port, existing, req.LeaseID, "tcp")
		if err != nil {
			return nil, err
		}
		t.RemotePort = p
		t.Hosts = nil
	case store.TunnelHTTP:
		norm := normalizeHosts(hosts)
		if port > 0 {
			// Port based HTTP: reachable as http://<server-ip>:<port>/ .
			p, err := bindPort(st, port, existing, req.LeaseID, "tcp")
			if err != nil {
				return nil, err
			}
			t.RemotePort = p
			t.Hosts = norm
			break
		}
		if len(norm) == 0 {
			if domain == "" {
				return nil, errBad("hosts", "set a domain in settings, list hostnames, or give the tunnel a public port")
			}
			norm = []string{strings.ToLower(name) + "." + strings.ToLower(domain)}
		}
		for _, h := range norm {
			if strings.ContainsAny(h, " /\\") {
				return nil, errBad("hosts", "invalid hostname: "+h)
			}
		}
		for _, o := range st.Tunnels {
			if existing != nil && o.ID == existing.ID {
				continue
			}
			if o.Type != store.TunnelHTTP {
				continue
			}
			for _, oh := range o.Hosts {
				for _, nh := range norm {
					if strings.EqualFold(oh, nh) {
						if ownerID != "" && o.OwnerID != ownerID {
							return nil, errConflict("host_taken", "hostname "+nh+" is already in use")
						}
						return nil, errConflict("host_taken", "hostname "+nh+" is already routed to "+o.Name)
					}
				}
			}
		}
		t.Hosts = norm
		t.RemotePort = 0
		if req.LeaseID != "" {
			return nil, errBad("lease", "domain tunnels do not need a port reservation")
		}
	case store.TunnelUDP:
		// UDP has no Host header or SNI, so it is always a plain forwarded
		// port: DNS, WireGuard, game servers, syslog and friends.
		if len(normalizeHosts(hosts)) > 0 {
			return nil, errBad("hosts", "udp tunnels are port based only and cannot use domains")
		}
		p, err := bindPort(st, port, existing, req.LeaseID, "udp")
		if err != nil {
			return nil, err
		}
		t.RemotePort = p
		t.Hosts = nil
	}
	return t, nil
}

// bindPort validates and reserves a public port for TCP, HTTP and UDP
// tunnels: it must be inside the configured range, free in the store within
// the same port space, not already bound on this host, and match any lease
// handed out earlier.
func bindPort(st *store.State, port int, existing *store.Tunnel, leaseID, network string) (int, error) {
	lo, hi := st.Settings.PortRange[0], st.Settings.PortRange[1]
	if lo == 0 {
		lo, hi = 20000, 29999
	}
	if port == 0 {
		p, err := pickPort(st, idOf(existing), network)
		if err != nil {
			return 0, err
		}
		port = p
	}
	if port < lo || port > hi {
		return 0, errBad("remote_port", fmt.Sprintf("port must be between %d and %d", lo, hi))
	}
	for _, o := range st.Tunnels {
		if existing != nil && o.ID == existing.ID {
			continue
		}
		// TCP/HTTP and UDP listen in independent spaces, so a clash only
		// matters within the same protocol.
		if o.RemotePort == port && store.TunnelNetwork(o.Type) == network {
			return 0, errConflict("port_taken", fmt.Sprintf("port %d is already in use", port))
		}
	}
	samePort := existing != nil && existing.RemotePort == port
	if !samePort && !canListen(network, port) {
		return 0, errConflict("port_busy", fmt.Sprintf("port %d is already in use on this host", port))
	}
	if err := consumeLease(st, leaseID, port, idOf(existing), network); err != nil {
		return 0, err
	}
	return port, nil
}

func idOf(t *store.Tunnel) string {
	if t == nil {
		return ""
	}
	return t.ID
}

func normalizeHosts(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(h, ".")
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// CreateJoinCode mints a six character invitation for the default admin.
func (s *Server) CreateJoinCode(req api.JoinCodeRequest) (api.JoinCodeResponse, error) {
	return s.CreateJoinCodeForOwner("", req)
}

// CreateJoinCodeForOwner mints a six character invitation owned by ownerID.
func (s *Server) CreateJoinCodeForOwner(ownerID string, req api.JoinCodeRequest) (api.JoinCodeResponse, error) {
	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = 300
	}
	if ttl > 3600 {
		ttl = 3600
	}
	now := time.Now().UTC()
	code := auth.NewJoinCode()
	err := s.st.Update(func(st *store.State) error {
		oid := defaultOwnerID(st, ownerID)
		st.PruneJoins(now)
		st.Joins = append(st.Joins, auth.JoinCode{
			Code:      code,
			OwnerID:   oid,
			NodeName:  req.Name,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(ttl) * time.Second),
		})
		return nil
	})
	if err != nil {
		return api.JoinCodeResponse{}, err
	}
	host, warning := s.joinHost()
	return api.JoinCodeResponse{
		Code:      code,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second),
		Command:   fmt.Sprintf("nfrc join %s %s", host, code),
		Warning:   warning,
	}, nil
}

// joinHost renders the address an operator should type on the agent machine,
// plus a warning when nothing better than a local address exists. Silently
// printing "localhost" is how machines end up with a command that cannot work.
func (s *Server) joinHost() (addr, warning string) {
	host, port, err := net.SplitHostPort(s.cfg.ControlListen)
	if err != nil {
		host, port = "", "8443"
	}
	// The default control_listen is a wildcard bind, which says nothing about
	// which address a machine on the internet should dial. Domain wins, then
	// the operator's public address, then this host's interface addresses.
	host = transport.BestHost(s.publicHost(), host)
	if port == "443" {
		addr = host
	} else {
		addr = net.JoinHostPort(host, port)
	}
	if isLocalOnlyHost(host) {
		warning = "当前地址仅本机可用，请先在设置里填「对外地址」或「公网域名」，再加入别的机器。"
	}
	return addr, warning
}

// isLocalOnlyHost reports whether host can only be reached from this machine.
func isLocalOnlyHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// AddRemote registers another instance for management.
func (s *Server) AddRemote(ctx context.Context, req api.RemoteRequest) (api.Remote, error) {
	url := federation.NormalizeBaseURL(req.URL)
	if url == "" || req.APIKey == "" {
		return api.Remote{}, errBad("url", "address and API key are required")
	}
	pin := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(req.Pin)), ":", "")
	caMode := false

	// Direct NFR control ports always accept TLS on the same port even when the
	// web console defaults to plain HTTP. If the operator pasted http://host:8443
	// (e.g. from the banner or browser URL bar), upgrade to https:// whenever
	// TLS discovery succeeds so API keys and certificate pinning stay protected.
	if strings.HasPrefix(strings.ToLower(url), "http://") {
		httpsURL := "https://" + url[len("http://"):]
		if pin == "" {
			if mode, p, err := federation.VerifyMode(ctx, httpsURL); err == nil {
				url = httpsURL
				if mode == "ca" {
					caMode = true
				} else {
					pin = p
				}
			}
		} else {
			if _, err := federation.New(httpsURL, req.APIKey, pin).Probe(ctx); err == nil {
				url = httpsURL
			}
		}
	}
	if pin == "" && !caMode {
		mode, p, err := federation.VerifyMode(ctx, url)
		if err != nil {
			return api.Remote{}, errBad("unreachable", "cannot reach "+url+": "+err.Error())
		}
		if mode == "ca" {
			caMode = true
		} else {
			pin = p
		}
	}
	var client *federation.Client
	if caMode {
		client = federation.NewTrusted(url, req.APIKey)
	} else {
		client = federation.New(url, req.APIKey, pin)
	}
	info, err := client.Probe(ctx)
	if err != nil {
		return api.Remote{}, errBad("unreachable", err.Error())
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = info.Name
	}
	now := time.Now().UTC()
	rec := store.Remote{
		ID:        store.NewID(),
		Name:      name,
		URL:       url,
		APIKey:    req.APIKey,
		Pin:       pin,
		CA:        caMode,
		Version:   info.Version,
		CreatedAt: now,
		LastSeen:  &now,
	}
	if err := s.st.Update(func(st *store.State) error {
		st.Remotes = append(st.Remotes, rec)
		return nil
	}); err != nil {
		return api.Remote{}, err
	}
	s.notify("remote added: " + name)
	return api.Remote{
		ID: rec.ID, Name: rec.Name, URL: rec.URL, Version: rec.Version,
		Reachable: true, LastSeen: rec.LastSeen, CreatedAt: rec.CreatedAt,
	}, nil
}

// RemoveRemote forgets a managed instance.
func (s *Server) RemoveRemote(id string) error {
	err := s.st.Update(func(st *store.State) error {
		for i := range st.Remotes {
			if st.Remotes[i].ID == id {
				st.Remotes = append(st.Remotes[:i], st.Remotes[i+1:]...)
				return nil
			}
		}
		return errNotFound("remote not found")
	})
	if err == nil {
		s.notify("remote removed")
	}
	return err
}

// RemoteClient returns a federation client for a stored remote.
func (s *Server) RemoteClient(id string) (*federation.Client, store.Remote, error) {
	var rec store.Remote
	var ok bool
	_ = s.st.View(func(st *store.State) error {
		if r, found := st.RemoteByID(id); found {
			rec, ok = *r, true
		}
		return nil
	})
	if !ok {
		return nil, rec, errNotFound("remote not found")
	}
	if rec.CA {
		return federation.NewTrusted(rec.URL, rec.APIKey), rec, nil
	}
	return federation.New(rec.URL, rec.APIKey, rec.Pin), rec, nil
}

// CreateAPIKey mints a credential, returning the secret exactly once.
func (s *Server) CreateAPIKey(req api.APIKeyRequest) (api.APIKeyResponse, error) {
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	switch scope {
	case "read", "write", "admin":
	default:
		return api.APIKeyResponse{}, errBad("scope", "scope must be read, write or admin")
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = scope + " key"
	}
	secret := auth.NewSecret("nfr_", 24)
	now := time.Now().UTC()
	rec := auth.APIKey{
		ID:        store.NewID(),
		Label:     label,
		Hash:      auth.Hash(secret),
		Scope:     scope,
		CreatedAt: now,
		AllowFrom: req.AllowFrom,
	}
	if err := s.st.Update(func(st *store.State) error {
		st.APIKeys = append(st.APIKeys, rec)
		return nil
	}); err != nil {
		return api.APIKeyResponse{}, err
	}
	return api.APIKeyResponse{
		APIKey: api.APIKey{ID: rec.ID, Label: rec.Label, Scope: rec.Scope, CreatedAt: rec.CreatedAt},
		Key:    secret,
	}, nil
}

// DeleteAPIKey revokes a credential.
func (s *Server) DeleteAPIKey(id string) error {
	err := s.st.Update(func(st *store.State) error {
		for i := range st.APIKeys {
			if st.APIKeys[i].ID == id {
				st.APIKeys = append(st.APIKeys[:i], st.APIKeys[i+1:]...)
				return nil
			}
		}
		return errNotFound("api key not found")
	})
	if err == nil {
		s.mu.Lock()
		delete(s.lastUsed, id)
		s.mu.Unlock()
	}
	return err
}

// UpdateSettings changes instance wide behaviour.
func (s *Server) UpdateSettings(domain string, publicHost string, apiEnabled bool, portRange [2]int, email string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimSuffix(domain, "/")
	publicHost = normaliseHost(publicHost)
	if portRange[0] <= 0 || portRange[1] <= portRange[0] || portRange[1] > 65535 {
		return errBad("port_range", "port range must be a valid ascending pair")
	}
	err := s.st.Update(func(st *store.State) error {
		st.Settings.Domain = domain
		st.Settings.PublicHost = publicHost
		st.Settings.APIEnabled = apiEnabled
		st.Settings.PortRange = portRange
		st.Settings.HTTPSEmail = email
		return nil
	})
	if err == nil {
		s.notify("settings updated")
	}
	return err
}

// normaliseHost cleans an operator-entered public address: strips a scheme and
// any trailing slash or path, so "https://nfr.example.com/" becomes
// "nfr.example.com". A port is kept if present.
func normaliseHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimPrefix(h, "https://")
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// publicHost is the address visitors should dial: the configured domain wins,
// then the operator's "public address" setting, then nothing (callers fall back
// to probing this host's interfaces).
func (s *Server) publicHost() string {
	st := s.st.Snapshot()
	if st.Settings.Domain != "" {
		return st.Settings.Domain
	}
	return st.Settings.PublicHost
}

// ToggleAPI flips the REST API switch.
func (s *Server) ToggleAPI(on bool) error {
	err := s.st.Update(func(st *store.State) error {
		st.Settings.APIEnabled = on
		return nil
	})
	if err == nil {
		s.notify("settings updated")
	}
	return err
}

// SetInstanceName renames this node.
func (s *Server) SetInstanceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errBad("name", "name is required")
	}
	return s.st.Update(func(st *store.State) error {
		st.Name = name
		return nil
	})
}

// RemoveNode forgets an agent, disconnects any live session, and drops its tunnels.
func (s *Server) RemoveNode(id string) error {
	return s.RemoveNodeForOwner("", id)
}

// RemoveNodeForOwner forgets an agent owned by ownerID (or any node when ownerID is empty).
func (s *Server) RemoveNodeForOwner(ownerID, id string) error {
	s.flushQuotas()
	var removedTunnels []string
	var nodeOwner string
	err := s.st.Update(func(st *store.State) error {
		found := false
		nodes := make([]store.Node, 0, len(st.Nodes))
		for _, n := range st.Nodes {
			if n.ID == id {
				if ownerID != "" && n.OwnerID != ownerID {
					return errNotFound("node not found")
				}
				found = true
				nodeOwner = n.OwnerID
				continue
			}
			nodes = append(nodes, n)
		}
		if !found {
			return errNotFound("node not found")
		}
		st.Nodes = nodes
		tunnels := make([]store.Tunnel, 0, len(st.Tunnels))
		removedSet := map[string]bool{}
		for _, t := range st.Tunnels {
			if t.NodeID == id {
				removedTunnels = append(removedTunnels, t.ID)
				removedSet[t.ID] = true
				continue
			}
			tunnels = append(tunnels, t)
		}
		st.Tunnels = tunnels
		if len(removedSet) > 0 && len(st.Leases) > 0 {
			leases := make([]store.Lease, 0, len(st.Leases))
			for _, l := range st.Leases {
				if !removedSet[l.TunnelID] {
					leases = append(leases, l)
				}
			}
			st.Leases = leases
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sess := s.sessionFor(id); sess != nil {
		s.dropSession(id, sess)
		sess.close()
	}
	for _, tid := range removedTunnels {
		s.clearTargetDown(tid)
		s.met.Drop(tid)
		s.dropLimit(tid)
	}
	s.reconcile()
	s.notifyOwner(nodeOwner, "node removed")
	return nil
}

// SetNodeName renames an agent node.
func (s *Server) SetNodeName(id, name string) error {
	return s.SetNodeNameForOwner("", id, name)
}

// SetNodeNameForOwner renames an agent node owned by ownerID (or any node when ownerID is empty).
func (s *Server) SetNodeNameForOwner(ownerID, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errBad("name", "name is required")
	}
	var nodeOwner string
	err := s.st.Update(func(st *store.State) error {
		n, ok := st.NodeByID(id)
		if !ok || (ownerID != "" && n.OwnerID != ownerID) {
			return errNotFound("node not found")
		}
		nodeOwner = n.OwnerID
		n.Name = name
		return nil
	})
	if err == nil {
		s.notifyOwner(nodeOwner, "node renamed")
	}
	return err
}
