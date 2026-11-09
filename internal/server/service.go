package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/federation"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
)

// CreateTunnel validates and stores a new tunnel, then tells the agent.
func (s *Server) CreateTunnel(req api.TunnelRequest) (api.Tunnel, error) {
	var created store.Tunnel
	err := s.st.Update(func(st *store.State) error {
		t, err := buildTunnel(st, req, nil, s.cfg.Domain)
		if err != nil {
			return err
		}
		t.ID = store.NewID()
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
	s.notify("tunnel created: " + created.Name)
	if v, ok := s.Tunnel(created.ID); ok {
		return v, nil
	}
	return api.Tunnel{}, nil
}

// UpdateTunnel replaces the editable fields of a tunnel.
func (s *Server) UpdateTunnel(id string, req api.TunnelRequest) (api.Tunnel, error) {
	var node string
	err := s.st.Update(func(st *store.State) error {
		cur, ok := st.TunnelByID(id)
		if !ok {
			return errNotFound("tunnel not found")
		}
		next, err := buildTunnel(st, req, cur, s.cfg.Domain)
		if err != nil {
			return err
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
		node = cur.NodeID
		return nil
	})
	if err != nil {
		return api.Tunnel{}, err
	}
	s.reconcile()
	s.notify("tunnel updated: " + id)
	s.pushConfig(node)
	if v, ok := s.Tunnel(id); ok {
		return v, nil
	}
	return api.Tunnel{}, nil
}

// DeleteTunnel removes a tunnel and closes its public port.
func (s *Server) DeleteTunnel(id string) error {
	var name string
	err := s.st.Update(func(st *store.State) error {
		for i := range st.Tunnels {
			if st.Tunnels[i].ID == id {
				name = st.Tunnels[i].Name
				st.Tunnels = append(st.Tunnels[:i], st.Tunnels[i+1:]...)
				leases := st.Leases[:0]
				for _, l := range st.Leases {
					if l.TunnelID != id {
						leases = append(leases, l)
					}
				}
				st.Leases = leases
				return nil
			}
		}
		return errNotFound("tunnel not found")
	})
	if err != nil {
		return err
	}
	s.met.Drop(id)
	s.dropLimit(id)
	s.reconcile()
	s.notify("tunnel deleted: " + name)
	return nil
}

// SetTunnelEnabled flips the enabled switch.
func (s *Server) SetTunnelEnabled(id string, on bool) error {
	var node string
	err := s.st.Update(func(st *store.State) error {
		t, ok := st.TunnelByID(id)
		if !ok {
			return errNotFound("tunnel not found")
		}
		t.Enabled = on
		t.UpdatedAt = time.Now().UTC()
		node = t.NodeID
		return nil
	})
	if err != nil {
		return err
	}
	s.reconcile()
	s.notify("tunnel toggled: " + id)
	s.pushConfig(node)
	return nil
}

// buildTunnel normalises and validates a tunnel description. It mutates the
// lease list only when a valid lease is supplied.
func buildTunnel(st *store.State, req api.TunnelRequest, existing *store.Tunnel, domain string) (*store.Tunnel, error) {
	t := &store.Tunnel{Enabled: true}
	name := strings.TrimSpace(req.Name)
	nodeID := strings.TrimSpace(req.NodeID)
	typ := strings.TrimSpace(req.Type)
	local := strings.TrimSpace(req.Local)
	port := req.RemotePort
	hosts := req.Hosts

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
		if port == 0 {
			port = existing.RemotePort
		}
		if len(hosts) == 0 {
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
	if _, ok := st.NodeByID(nodeID); !ok {
		return nil, errBad("node_id", "choose a node that has joined")
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
			return 0, errConflict("port_taken", fmt.Sprintf("port %d is already used by %s", port, o.Name))
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

// CreateJoinCode mints a six character invitation.
func (s *Server) CreateJoinCode(req api.JoinCodeRequest) (api.JoinCodeResponse, error) {
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
		st.PruneJoins(now)
		st.Joins = append(st.Joins, auth.JoinCode{
			Code:      code,
			NodeName:  req.Name,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(ttl) * time.Second),
		})
		return nil
	})
	if err != nil {
		return api.JoinCodeResponse{}, err
	}
	return api.JoinCodeResponse{
		Code:      code,
		ExpiresAt: now.Add(time.Duration(ttl) * time.Second),
		Command:   fmt.Sprintf("nfr join %s %s", s.joinHost(), code),
	}, nil
}

// joinHost renders the address an operator should type on the agent machine.
func (s *Server) joinHost() string {
	host, port, err := net.SplitHostPort(s.cfg.ControlListen)
	if err != nil {
		port = ""
	}
	host = transport.BestHost(s.cfg.Domain, host)
	if port == "443" || port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// AddRemote registers another instance for management.
func (s *Server) AddRemote(ctx context.Context, req api.RemoteRequest) (api.Remote, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" || req.APIKey == "" {
		return api.Remote{}, errBad("url", "address and API key are required")
	}
	pin := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(req.Pin)), ":", "")
	caMode := false
	if pin == "" {
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
	return s.st.Update(func(st *store.State) error {
		for i := range st.APIKeys {
			if st.APIKeys[i].ID == id {
				st.APIKeys = append(st.APIKeys[:i], st.APIKeys[i+1:]...)
				return nil
			}
		}
		return errNotFound("api key not found")
	})
}

// UpdateSettings changes instance wide behaviour.
func (s *Server) UpdateSettings(domain string, apiEnabled bool, portRange [2]int, email string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimSuffix(domain, "/")
	if portRange[0] <= 0 || portRange[1] <= portRange[0] || portRange[1] > 65535 {
		return errBad("port_range", "port range must be a valid ascending pair")
	}
	err := s.st.Update(func(st *store.State) error {
		st.Settings.Domain = domain
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

// ToggleAPI flips the REST API switch.
func (s *Server) ToggleAPI(on bool) error {
	return s.st.Update(func(st *store.State) error {
		st.Settings.APIEnabled = on
		return nil
	})
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

// RemoveNode forgets an agent and its tunnels.
func (s *Server) RemoveNode(id string) error {
	err := s.st.Update(func(st *store.State) error {
		found := false
		nodes := st.Nodes[:0]
		for _, n := range st.Nodes {
			if n.ID == id {
				found = true
				continue
			}
			nodes = append(nodes, n)
		}
		if !found {
			return errNotFound("node not found")
		}
		st.Nodes = nodes
		tunnels := st.Tunnels[:0]
		for _, t := range st.Tunnels {
			if t.NodeID != id {
				tunnels = append(tunnels, t)
			}
		}
		st.Tunnels = tunnels
		return nil
	})
	if err == nil {
		s.reconcile()
		s.notify("node removed")
	}
	return err
}
