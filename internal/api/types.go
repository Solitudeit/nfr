// Package api defines the JSON shapes shared by the console, the REST API
// and the federation client. Keeping them here avoids the server and client
// packages importing each other.
package api

import "time"

// Info describes one instance.
type Info struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Revision    uint64    `json:"revision"`
	Domain      string    `json:"domain"`
	PortRange   [2]int    `json:"port_range"`
	APIEnabled  bool      `json:"api_enabled"`
	ControlAddr string    `json:"control_addr"`
	PanelURL    string    `json:"panel_url"`
	Pinned      string    `json:"pin"`
	StartedAt   time.Time `json:"started_at"`
	Nodes       int       `json:"nodes"`
	Tunnels     int       `json:"tunnels"`
}

// Tunnel is the console's view of a published service.
type Tunnel struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	NodeID     string    `json:"node_id"`
	NodeName   string    `json:"node_name,omitempty"`
	Type       string    `json:"type"`
	Local      string    `json:"local"`
	RemotePort int       `json:"remote_port"`
	Hosts      []string  `json:"hosts,omitempty"`
	Enabled    bool      `json:"enabled"`
	Online     bool      `json:"online"`
	In         int64     `json:"in"`
	Out        int64     `json:"out"`
	Conns      int64     `json:"conns"`
	Active     int64     `json:"active"`
	Errs       int64     `json:"errs"`
	InRate     float64   `json:"in_rate"`
	OutRate    float64   `json:"out_rate"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Node is an edge agent as seen by the console.
type Node struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OS         string     `json:"os,omitempty"`
	Arch       string     `json:"arch,omitempty"`
	Version    string     `json:"version,omitempty"`
	Online     bool       `json:"online"`
	RemoteAddr string     `json:"remote_addr,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	Tunnels    int        `json:"tunnels"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TunnelRequest creates or updates a tunnel.
type TunnelRequest struct {
	Name       string   `json:"name"`
	NodeID     string   `json:"node_id"`
	Type       string   `json:"type"`
	Local      string   `json:"local"`
	RemotePort int      `json:"remote_port"`
	Hosts      []string `json:"hosts,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
	LeaseID    string   `json:"lease_id,omitempty"`
}

// AllocateRequest asks for a free remote port.
type AllocateRequest struct {
	TunnelID string `json:"tunnel_id,omitempty"`
}

// AllocateResponse carries a short lived port reservation.
type AllocateResponse struct {
	Port      int       `json:"port"`
	LeaseID   string    `json:"lease_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// JoinCodeRequest mints a one-time invitation.
type JoinCodeRequest struct {
	Name       string `json:"name"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// JoinCodeResponse is shown to the operator.
type JoinCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	Command   string    `json:"command"`
}

// RemoteRequest registers another instance.
type RemoteRequest struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	Pin    string `json:"pin,omitempty"`
}

// Remote is a managed instance.
type Remote struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	URL       string     `json:"url"`
	Version   string     `json:"version,omitempty"`
	Reachable bool       `json:"reachable"`
	CA        bool       `json:"ca,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// APIKey is a credential listing entry.
type APIKey struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Scope     string     `json:"scope"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// APIKeyRequest creates a key. The secret is returned once.
type APIKeyRequest struct {
	Label     string   `json:"label"`
	Scope     string   `json:"scope"`
	AllowFrom []string `json:"allow_from,omitempty"`
}

// APIKeyResponse includes the plaintext key exactly once.
type APIKeyResponse struct {
	APIKey
	Key string `json:"key"`
}

// Error is the uniform error body.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Event is pushed over server sent events.
type Event struct {
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
	Message string    `json:"message,omitempty"`
}
