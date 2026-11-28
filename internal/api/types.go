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
	ID          string   `json:"id"`
	OwnerID     string   `json:"owner_id,omitempty"`
	Name        string   `json:"name"`
	NodeID      string   `json:"node_id"`
	NodeName    string   `json:"node_name,omitempty"`
	Type        string   `json:"type"`
	Local       string   `json:"local"`
	RemotePort  int      `json:"remote_port"`
	Hosts       []string `json:"hosts,omitempty"`
	Enabled     bool     `json:"enabled"`
	BandwidthKB int      `json:"bandwidth_kb,omitempty"`
	QuotaMB     int64    `json:"quota_mb,omitempty"`
	UsedBytes   int64    `json:"used_bytes,omitempty"`
	QuotaPaused bool     `json:"quota_paused,omitempty"`
	QuotaReason string   `json:"quota_reason,omitempty"`
	Online      bool     `json:"online"`
	// NodeMissing marks a tunnel whose node no longer exists in the registry.
	NodeMissing bool `json:"node_missing,omitempty"`
	// Awaiting marks a tunnel that was created before its node exists and
	// waits for a device to redeem the join code minted at creation.
	Awaiting bool `json:"awaiting,omitempty"`
	// TargetDown reports that the agent is connected but could not reach the
	// inner address.
	TargetDown bool `json:"target_down,omitempty"`
	// PublicHost is the address visitors dial for this tunnel: the configured
	// domain, else this machine's public address. A bare port like ":20001"
	// tells an operator nothing they can paste into a browser.
	PublicHost string `json:"public_host,omitempty"`
	// Status is the one word a person reads: 已停用 / 配额停用 / 等待接入 /
	// 节点离线 / 需重新选择节点 / 就绪 / 活跃 / 目标不可达. StatusKind maps it
	// to a dot and a chip class so the console and its poller never disagree.
	Status     string    `json:"status"`
	StatusKind string    `json:"status_kind"`
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
	OwnerID    string     `json:"owner_id,omitempty"`
	Name       string     `json:"name"`
	OS         string     `json:"os,omitempty"`
	Arch       string     `json:"arch,omitempty"`
	Version    string     `json:"version,omitempty"`
	Online     bool       `json:"online"`
	Region     string     `json:"region,omitempty"` // country name, filled in by the console
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
	RemotePort      int      `json:"remote_port"`
	ClearRemotePort bool     `json:"clear_remote_port,omitempty"`
	Hosts           []string `json:"hosts,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
	LeaseID    string   `json:"lease_id,omitempty"`
	// BandwidthKB caps each direction of the tunnel in kilobytes per second.
	// nil leaves the value unchanged, 0 explicitly removes the limit.
	BandwidthKB *int `json:"bandwidth_kb,omitempty"`
	// QuotaMB caps the tunnel's calendar month traffic. nil leaves it
	// unchanged, 0 explicitly removes the quota.
	QuotaMB *int64 `json:"quota_mb,omitempty"`
}

// AllocateRequest asks for a free remote port.
type AllocateRequest struct {
	TunnelID string `json:"tunnel_id,omitempty"`
	// Network selects the port space: "tcp" (default, also for http tunnels)
	// or "udp". TCP and UDP ports are independent, like on a normal host.
	Network string `json:"network,omitempty"`
}

// AllocateResponse carries a short lived port reservation.
type AllocateResponse struct {
	Port      int       `json:"port"`
	LeaseID   string    `json:"lease_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ProbeResult reports the outcome of one active tunnel reachability check.
type ProbeResult struct {
	TunnelID string `json:"tunnel_id"`
	Status   string `json:"status"` // ok | failed | offline | disabled | awaiting | unsupported | busy | timeout
	RTTMs    int64  `json:"rtt_ms,omitempty"`
	LocalMs  int64  `json:"local_ms,omitempty"`
	Message  string `json:"message,omitempty"`
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
	// Warning explains why the command above may not work from another
	// machine, e.g. the server only knows a loopback address.
	Warning string `json:"warning,omitempty"`
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
	Nodes     int        `json:"nodes"`
	Tunnels   int        `json:"tunnels"`
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
	OwnerID string    `json:"-"`
}

// MirrorNode describes one GitHub Release acceleration mirror.
type MirrorNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	ProbeURL string `json:"probe_url"`
}

// UpdateMirrorProbe reports the fastest reachable mirror measured from the server host.
type UpdateMirrorProbe struct {
	OK      bool       `json:"ok"`
	Mirror  MirrorNode `json:"mirror,omitempty"`
	RTTMs   int64      `json:"rtt_ms,omitempty"`
	Message string     `json:"message,omitempty"`
}

// UpgradeProgress reports live progress of an in-place nfrs self-upgrade.
type UpgradeProgress struct {
	State          string `json:"state"` // idle | downloading | verifying | installing | restarting | done | error
	Percent        int    `json:"percent"`
	TargetVersion  string `json:"target_version,omitempty"`
	Source         string `json:"source,omitempty"`
	ChecksumSource string `json:"checksum_source,omitempty"` // official | mirror
	Message        string `json:"message,omitempty"`
	Error          string `json:"error,omitempty"`
}

// UpdateStatus is returned by /nfr/api/update and /nfr/api/update/check.
type UpdateStatus struct {
	CurrentVersion     string             `json:"current_version"`
	LatestVersion      string             `json:"latest_version,omitempty"`
	HasUpdate          bool               `json:"has_update"`
	IsPrerelease       bool               `json:"is_prerelease,omitempty"`
	PublishedAt        string             `json:"published_at,omitempty"`
	Notes              string             `json:"notes,omitempty"`
	ReleaseURL         string             `json:"release_url,omitempty"`
	CheckedAt          *time.Time         `json:"checked_at,omitempty"`
	LastError          string             `json:"last_error,omitempty"`
	AutoCheck          bool               `json:"auto_check"`
	Channel            string             `json:"channel"` // auto | preview | stable
	CanSelfUpdate      bool               `json:"can_self_update"`
	CannotUpdateReason string             `json:"cannot_update_reason,omitempty"`
	SourceUsed         string             `json:"source_used,omitempty"`
	MirrorUsed         string             `json:"mirror_used,omitempty"`
	Mirrors            []MirrorNode       `json:"mirrors"`
	Upgrade            UpgradeProgress    `json:"upgrade"`
}

// User is the panel's view of an account. AuthToken is only populated on self
// profile responses.
type User struct {
	ID             string    `json:"id"`
	Username       string    `json:"username"`
	Role           string    `json:"role"` // "admin" | "user"
	Disabled       bool      `json:"disabled"`
	MaxTunnels     int       `json:"max_tunnels"`
	MaxBandwidthKB int       `json:"max_bandwidth_kb"`
	MonthlyQuotaMB int64     `json:"monthly_quota_mb"`
	UsedBytes      int64     `json:"used_bytes"`
	UsageMonth     string    `json:"usage_month,omitempty"`
	TunnelCount    int       `json:"tunnel_count"`
	OnlineTunnels  int       `json:"online_tunnels"`
	HasAvatar      bool      `json:"has_avatar"`
	AvatarURL      string    `json:"avatar_url,omitempty"`
	AuthToken      string    `json:"auth_token,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CreateUserRequest creates a new standard user account.
type CreateUserRequest struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	MaxTunnels     int    `json:"max_tunnels"`
	MaxBandwidthKB int    `json:"max_bandwidth_kb"`
	MonthlyQuotaMB int64  `json:"monthly_quota_mb"`
}

// UpdateUserRequest modifies a user's quotas or disabled state.
type UpdateUserRequest struct {
	Disabled       *bool  `json:"disabled,omitempty"`
	MaxTunnels     *int   `json:"max_tunnels,omitempty"`
	MaxBandwidthKB *int   `json:"max_bandwidth_kb,omitempty"`
	MonthlyQuotaMB *int64 `json:"monthly_quota_mb,omitempty"`
}

// UserDetail returns one user together with their tunnels and nodes for the
// admin detail view.
type UserDetail struct {
	User    User     `json:"user"`
	Tunnels []Tunnel `json:"tunnels"`
	Nodes   []Node   `json:"nodes"`
}

// ProfileUpdateRequest edits the signed-in user's username and/or password.
type ProfileUpdateRequest struct {
	Username    string `json:"username,omitempty"`
	OldPassword string `json:"old_password,omitempty"`
	NewPassword string `json:"new_password,omitempty"`
}

// DBStatus describes the active persistence backend for admin setup & settings.
type DBStatus struct {
	Mode           string `json:"mode"` // "file" | "mysql"
	Configured     bool   `json:"configured"`
	FromConfig     bool   `json:"from_config"`
	MandatorySetup bool   `json:"mandatory_setup"`
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	Database       string `json:"database,omitempty"`
	Username       string `json:"username,omitempty"`
	HasPassword    bool   `json:"has_password,omitempty"`
	Version        string `json:"version,omitempty"`
	Error          string `json:"error,omitempty"`
}

// DBConfigRequest supplies MySQL connection details from the admin panel.
type DBConfigRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// DBTestResponse reports whether a MySQL connectivity & privilege probe passed.
type DBTestResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Message string `json:"message,omitempty"`
}

// DBSetupResponse reports the outcome of switching to MySQL storage.
type DBSetupResponse struct {
	OK              bool     `json:"ok"`
	AdoptedExisting bool     `json:"adopted_existing"`
	ReauthRequired  bool     `json:"reauth_required"`
	Status          DBStatus `json:"status"`
	Message         string   `json:"message,omitempty"`
}
