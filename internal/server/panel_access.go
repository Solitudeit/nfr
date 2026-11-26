package server

import (
	"context"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/store"
)

// Settings is a read-only view of instance behaviour for the console.
type Settings struct {
	Domain                 string
	APIEnabled             bool
	PortRange              [2]int
	HTTPSEmail             string
	PublicHost             string
	DisableAutoUpdateCheck bool
	UpdateChannel          string
}

// Settings returns the current settings.
func (s *Server) Settings() Settings {
	st := s.st.Snapshot()
	return Settings{
		Domain:                 st.Settings.Domain,
		APIEnabled:             st.Settings.APIEnabled,
		PortRange:              st.Settings.PortRange,
		HTTPSEmail:             st.Settings.HTTPSEmail,
		PublicHost:             st.Settings.PublicHost,
		DisableAutoUpdateCheck: st.Settings.DisableAutoUpdateCheck,
		UpdateChannel:          st.Settings.UpdateChannel,
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

const maxPersistedPanelSessions = 32

// PanelSessions returns all non-expired persisted console sessions.
func (s *Server) PanelSessions() []store.PanelSession {
	now := time.Now().UTC()
	var out []store.PanelSession
	_ = s.st.View(func(st *store.State) error {
		for _, ps := range st.PanelSessions {
			if ps.TokenHash != "" && now.Before(ps.ExpiresAt) {
				out = append(out, ps)
			}
		}
		return nil
	})
	return out
}

// SavePanelSession persists a hashed "remember me" console session so it
// survives control node restarts while keeping state bounded.
func (s *Server) SavePanelSession(sess store.PanelSession) error {
	if sess.TokenHash == "" {
		return nil
	}
	now := time.Now().UTC()
	return s.st.Update(func(st *store.State) error {
		kept := make([]store.PanelSession, 0, len(st.PanelSessions)+1)
		for _, ps := range st.PanelSessions {
			if ps.TokenHash == "" || ps.TokenHash == sess.TokenHash || !now.Before(ps.ExpiresAt) {
				continue
			}
			kept = append(kept, ps)
		}
		kept = append(kept, sess)
		if len(kept) > maxPersistedPanelSessions {
			kept = kept[len(kept)-maxPersistedPanelSessions:]
		}
		st.PanelSessions = kept
		return nil
	})
}

// DeletePanelSession removes a single persisted console session on logout.
func (s *Server) DeletePanelSession(tokenHash string) error {
	if tokenHash == "" {
		return nil
	}
	_, err := s.st.UpdateIf(func(st *store.State) (bool, error) {
		kept := make([]store.PanelSession, 0, len(st.PanelSessions))
		changed := false
		for _, ps := range st.PanelSessions {
			if ps.TokenHash == tokenHash {
				changed = true
				continue
			}
			kept = append(kept, ps)
		}
		if changed {
			st.PanelSessions = kept
		}
		return changed, nil
	})
	return err
}

// RevokeOtherPanelSessions removes all persisted console sessions except keepHash.
func (s *Server) RevokeOtherPanelSessions(keepHash string) error {
	_, err := s.st.UpdateIf(func(st *store.State) (bool, error) {
		kept := make([]store.PanelSession, 0, 1)
		changed := false
		for _, ps := range st.PanelSessions {
			if keepHash != "" && ps.TokenHash == keepHash {
				kept = append(kept, ps)
				continue
			}
			changed = true
		}
		if changed {
			st.PanelSessions = kept
		}
		return changed, nil
	})
	return err
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
