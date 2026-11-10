package server

import (
	"context"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/store"
)

// Settings is a read-only view of instance behaviour for the console.
type Settings struct {
	Domain     string
	APIEnabled bool
	PortRange  [2]int
	HTTPSEmail string
	PublicHost string
	LocalNode  bool
}

// Settings returns the current settings.
func (s *Server) Settings() Settings {
	st := s.st.Snapshot()
	return Settings{
		Domain:     st.Settings.Domain,
		APIEnabled: st.Settings.APIEnabled,
		PortRange:  st.Settings.PortRange,
		HTTPSEmail: st.Settings.HTTPSEmail,
		PublicHost: st.Settings.PublicHost,
		LocalNode:  st.Settings.LocalNode,
	}
}

// PanelPasswordHash returns the stored bcrypt hash of the console password.
func (s *Server) PanelPasswordHash() string {
	var hash string
	_ = s.st.View(func(st *store.State) error {
		hash = st.PanelPassword
		return nil
	})
	return hash
}

// SetPanelPasswordHash stores a new bcrypt hash.
func (s *Server) SetPanelPasswordHash(hash string) error {
	return s.st.Update(func(st *store.State) error {
		st.PanelPassword = hash
		return nil
	})
}

// RefreshRemote probes a managed instance and records reachability.
func (s *Server) RefreshRemote(ctx context.Context, id string) (api.Info, error) {
	c, _, err := s.RemoteClient(id)
	if err != nil {
		return api.Info{}, err
	}
	info, probeErr := c.Probe(ctx)
	now := time.Now().UTC()
	_ = s.st.Update(func(st *store.State) error {
		if r, ok := st.RemoteByID(id); ok {
			r.LastSeen = &now
			if probeErr != nil {
				r.LastError = probeErr.Error()
			} else {
				r.LastError = ""
				r.Version = info.Version
				r.Nodes = info.Nodes
				r.Tunnels = info.Tunnels
			}
		}
		return nil
	})
	if probeErr != nil {
		return api.Info{}, probeErr
	}
	return info, nil
}

// RemoteRecord returns the stored record of a managed instance.
func (s *Server) RemoteRecord(id string) (store.Remote, bool) {
	var out store.Remote
	var ok bool
	_ = s.st.View(func(st *store.State) error {
		if r, found := st.RemoteByID(id); found {
			out, ok = *r, true
		}
		return nil
	})
	return out, ok
}
