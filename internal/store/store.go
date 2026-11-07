// Package store persists the small amount of durable state a control node
// owns. Everything is kept in one JSON document that is rewritten atomically,
// which is more than enough for the tens of tunnels a self-hosted node serves.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/auth"
)

// SchemaVersion is bumped when the on-disk shape changes.
const SchemaVersion = 1

// Tunnel types.
const (
	TunnelTCP  = "tcp"
	TunnelHTTP = "http"
)

// State is the whole persisted document.
type State struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
	Revision      uint64    `json:"revision"`

	PanelPassword string          `json:"panel_password"`
	Settings      Settings        `json:"settings"`
	Tunnels       []Tunnel        `json:"tunnels"`
	Nodes         []Node          `json:"nodes"`
	Joins         []auth.JoinCode `json:"joins"`
	Leases        []Lease         `json:"leases"`
	Remotes       []Remote        `json:"remotes"`
	APIKeys       []auth.APIKey   `json:"api_keys"`
}

// Settings are operator tunables exposed in the console.
type Settings struct {
	Domain     string `json:"domain"`
	PortRange  [2]int `json:"port_range"`
	APIEnabled bool   `json:"api_enabled"`
	HTTPSEmail string `json:"https_email"`
}

// Tunnel is one published service.
type Tunnel struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	NodeID     string    `json:"node_id"`
	Type       string    `json:"type"`
	Local      string    `json:"local"`
	RemotePort int       `json:"remote_port"`
	Hosts      []string  `json:"hosts,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Node is a registered edge agent.
type Node struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	TokenHash  string     `json:"token_hash"`
	OS         string     `json:"os,omitempty"`
	Arch       string     `json:"arch,omitempty"`
	Version    string     `json:"version,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	LastRemote string     `json:"last_remote,omitempty"`
}

// Lease reserves a remote port while an operator fills in a form.
type Lease struct {
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	TunnelID  string    `json:"tunnel_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Remote is another NFR instance this one manages through its API.
type Remote struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	Pin    string `json:"pin,omitempty"`
	// CA records that this instance serves a publicly trusted certificate,
	// so renewals do not invalidate the pin.
	CA        bool       `json:"ca,omitempty"`
	Version   string     `json:"version,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Store serialises access to the state document.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
}

// Open loads the document in dir, creating a fresh one when absent.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = State{
			SchemaVersion: SchemaVersion,
			ID:            NewID(),
			Name:          "nfr",
			CreatedAt:     time.Now().UTC(),
			Settings: Settings{
				PortRange: [2]int{20000, 29999},
			},
		}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
		if s.state.Settings.PortRange[1] == 0 {
			s.state.Settings.PortRange = [2]int{20000, 29999}
		}
	}
	return s, nil
}

// Path is the backing file.
func (s *Store) Path() string { return s.path }

// View runs fn with a read lock held. The state must not be retained.
func (s *Store) View(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&s.state)
}

// Update applies fn and persists the result. On error nothing is written and
// the in-memory state is rolled back, so a handler that fails halfway through
// cannot leave the process disagreeing with its own file.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup, err := deepCopy(s.state)
	if err != nil {
		return err
	}
	if err := fn(&s.state); err != nil {
		s.state = backup
		return err
	}
	s.state.Revision = backup.Revision + 1
	if err := s.saveLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

// Snapshot returns a deep copy safe to hand to HTTP handlers.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	dup, err := deepCopy(s.state)
	if err != nil {
		return s.state
	}
	return dup
}

func deepCopy(in State) (State, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return in, err
	}
	var out State
	err = json.Unmarshal(b, &out)
	return out, err
}

func (s *Store) saveLocked() error {
	body, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := os.Chmod(name, 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// NewID returns a short, lowercase, sortable-ish identifier.
func NewID() string {
	b := auth.RandomBytes(8)
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	out := make([]byte, len(b))
	for i := range out {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}

// TunnelByID finds a tunnel.
func (st *State) TunnelByID(id string) (*Tunnel, bool) {
	for i := range st.Tunnels {
		if st.Tunnels[i].ID == id {
			return &st.Tunnels[i], true
		}
	}
	return nil, false
}

// NodeByID finds a node.
func (st *State) NodeByID(id string) (*Node, bool) {
	for i := range st.Nodes {
		if st.Nodes[i].ID == id {
			return &st.Nodes[i], true
		}
	}
	return nil, false
}

// NodeByTokenHash finds a node by the stored digest.
func (st *State) NodeByTokenHash(h string) (*Node, bool) {
	for i := range st.Nodes {
		if st.Nodes[i].TokenHash == h {
			return &st.Nodes[i], true
		}
	}
	return nil, false
}

// RemoteByID finds a managed remote.
func (st *State) RemoteByID(id string) (*Remote, bool) {
	for i := range st.Remotes {
		if st.Remotes[i].ID == id {
			return &st.Remotes[i], true
		}
	}
	return nil, false
}

// TunnelOnPort reports whether a tunnel already claims a port.
func (st *State) TunnelOnPort(port int, exclude string) (*Tunnel, bool) {
	for i := range st.Tunnels {
		t := &st.Tunnels[i]
		if t.RemotePort == port && t.ID != exclude {
			return t, true
		}
	}
	return nil, false
}

// TunnelsForNode returns the enabled tunnels of a node, sorted by name.
func (st *State) TunnelsForNode(nodeID string) []Tunnel {
	var out []Tunnel
	for _, t := range st.Tunnels {
		if t.NodeID == nodeID && t.Enabled {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PruneLeases drops expired reservations.
func (st *State) PruneLeases(now time.Time) {
	kept := st.Leases[:0]
	for _, l := range st.Leases {
		if now.Before(l.ExpiresAt) {
			kept = append(kept, l)
		}
	}
	st.Leases = kept
}

// JoinCodeByValue finds a pending join code.
func (st *State) JoinCodeByValue(code string) (*auth.JoinCode, bool) {
	for i := range st.Joins {
		if st.Joins[i].Code == code {
			return &st.Joins[i], true
		}
	}
	return nil, false
}

// APIKeyByHash finds an API key record.
func (st *State) APIKeyByHash(h string) (*auth.APIKey, bool) {
	for i := range st.APIKeys {
		if st.APIKeys[i].Hash == h {
			return &st.APIKeys[i], true
		}
	}
	return nil, false
}

// PruneJoins removes codes that expired more than an hour ago.
func (st *State) PruneJoins(now time.Time) {
	kept := st.Joins[:0]
	for _, j := range st.Joins {
		if j.UsedAt == nil && now.After(j.ExpiresAt) {
			continue
		}
		if j.UsedAt != nil && now.Sub(*j.UsedAt) > time.Hour {
			continue
		}
		kept = append(kept, j)
	}
	st.Joins = kept
}
