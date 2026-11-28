// Package store persists the small amount of durable state a control node
// owns. Everything is kept in one JSON document that is rewritten atomically,
// which is more than enough for the tens of tunnels a self-hosted node serves.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
)

// SchemaVersion is bumped when the on-disk shape changes.
const SchemaVersion = 2

// User roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is a console account with isolated tunnels, nodes, quotas, and auth token.
type User struct {
	ID             string    `json:"id"`
	Username       string    `json:"username"`
	Role           string    `json:"role"` // admin | user
	PasswordHash   string    `json:"password_hash"`
	AuthToken      string    `json:"auth_token"`
	Disabled       bool      `json:"disabled,omitempty"`
	MaxTunnels     int       `json:"max_tunnels,omitempty"`
	MaxBandwidthKB int       `json:"max_bandwidth_kb,omitempty"`
	MonthlyQuotaMB int64     `json:"monthly_quota_mb,omitempty"`
	UsedBytes      int64     `json:"used_bytes,omitempty"`
	UsageMonth     string    `json:"usage_month,omitempty"`
	AvatarMime     string    `json:"avatar_mime,omitempty"`
	AvatarData     []byte    `json:"avatar_data,omitempty"`
	AvatarVer      int64     `json:"avatar_ver,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Tunnel types.
const (
	TunnelTCP  = "tcp"
	TunnelHTTP = "http"
	TunnelUDP  = "udp"
)

// TunnelNetwork maps a tunnel type onto the IP protocol its public port
// listens on. TCP and HTTP share the TCP space; UDP tunnels get their own,
// so tcp:53 and udp:53 can happily coexist like on any normal host.
func TunnelNetwork(typ string) string {
	if typ == TunnelUDP {
		return "udp"
	}
	return "tcp"
}

// State is the whole persisted document.
type State struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
	Revision      uint64    `json:"revision"`

	PanelPassword string          `json:"panel_password"`
	PanelSessions []PanelSession  `json:"panel_sessions,omitempty"`
	Users         []User          `json:"users,omitempty"`
	Settings      Settings        `json:"settings"`
	Tunnels       []Tunnel        `json:"tunnels"`
	Nodes         []Node          `json:"nodes"`
	Joins         []auth.JoinCode `json:"joins"`
	Leases        []Lease         `json:"leases"`
	Remotes       []Remote        `json:"remotes"`
	APIKeys       []auth.APIKey   `json:"api_keys"`
}

// PanelSession stores a persisted "remember me" console session keyed by the
// SHA-256 hash of its HttpOnly cookie token so sessions survive nfrs restarts
// without ever saving plaintext passwords in the browser.
type PanelSession struct {
	TokenHash string    `json:"token_hash"`
	UserID    string    `json:"user_id,omitempty"`
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Settings are operator tunables exposed in the console.
type Settings struct {
	Domain     string `json:"domain"`
	PortRange  [2]int `json:"port_range"`
	APIEnabled bool   `json:"api_enabled"`
	HTTPSEmail string `json:"https_email"`
	// PublicHost is the address visitors dial to reach published tunnels, e.g.
	// "nfr.example.com" or "203.0.113.7". Domain still wins for HTTP tunnels;
	// this covers the case where the operator has no domain but does know the
	// public address of a NAT'd host.
	PublicHost string `json:"public_host,omitempty"`
	// AuthToken is the shared secret clients can put in client.toml / client.yaml
	// to connect directly without minting an expiring join code. Encrypted at rest.
	AuthToken string `json:"auth_token,omitempty"`
	// DisableAutoUpdateCheck stops background update checks when true.
	DisableAutoUpdateCheck bool `json:"disable_auto_update_check,omitempty"`
	// UpdateChannel selects "auto" (default), "preview", or "stable".
	UpdateChannel string `json:"update_channel,omitempty"`
}

// Tunnel is one published service.
type Tunnel struct {
	ID              string    `json:"id"`
	OwnerID         string    `json:"owner_id,omitempty"`
	Name            string    `json:"name"`
	NodeID          string    `json:"node_id"`
	Type            string    `json:"type"`
	Local           string    `json:"local"`
	RemotePort      int       `json:"remote_port"`
	Hosts           []string  `json:"hosts,omitempty"`
	Enabled         bool      `json:"enabled"`
	BandwidthKB     int       `json:"bandwidth_kb,omitempty"`      // per direction, 0 = unlimited
	QuotaMB         int64     `json:"quota_mb,omitempty"`          // per calendar month, 0 = unlimited
	UsedBytes       int64     `json:"used_bytes,omitempty"`        // bytes used in UsageMonth
	UsageMonth      string    `json:"usage_month,omitempty"`       // "2006-01", resets the counters
	QuotaPaused     bool      `json:"quota_paused,omitempty"`      // disabled by the quota guard
	QuotaReason     string    `json:"quota_reason,omitempty"`      // "" | "tunnel" | "user"
	ManagedByConfig bool      `json:"managed_by_config,omitempty"` // synced from client.toml / client.yaml proxies
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Node is a registered edge agent.
type Node struct {
	ID         string     `json:"id"`
	OwnerID    string     `json:"owner_id,omitempty"`
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
	Network   string    `json:"network,omitempty"` // tcp (default) | udp
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
	Nodes     int        `json:"nodes,omitempty"`
	Tunnels   int        `json:"tunnels,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Store serialises access to the state document, persisting either to
// data_dir/state.json (bootstrap / file mode) or a MySQL database.
type Store struct {
	mu           sync.Mutex
	dir          string
	path         string
	key          []byte
	state        State
	mysql        *mysqlBackend
	dbCfg        DBConfig
	dbFromConfig bool
}

// Open loads the store in dir using file mode or an existing dir/db.json.
func Open(dir string) (*Store, error) {
	return OpenWithDB(dir, config.Database{})
}

// OpenWithDB loads the store in dir. Precedence: cfgDB (from server.toml /
// NFR_DB_*) > dir/db.json > dir/state.json.
func OpenWithDB(dir string, cfgDB config.Database) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, path: filepath.Join(dir, "state.json"), key: key}

	var mysqlCfg DBConfig
	var useMySQL bool
	if cfgDB.Configured() {
		mysqlCfg = dbConfigFromServer(cfgDB)
		if err := mysqlCfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid database config: %w", err)
		}
		useMySQL = true
		s.dbFromConfig = true
	} else if os.Getenv("NFR_STORAGE") != "file" {
		if fcfg, ok, err := loadDBConfigFile(dir, key); err != nil {
			return nil, err
		} else if ok {
			mysqlCfg = fcfg
			useMySQL = true
		}
	}

	if useMySQL {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		db, ver, err := openMySQLWithRetry(ctx, mysqlCfg, 3)
		if err != nil {
			return nil, fmt.Errorf("MySQL 启动连接失败（请检查数据库服务状态与连接配置）: %w", err)
		}
		if err := ensureMySQLSchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
		exists, _, err := checkMySQLMeta(ctx, db)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		s.mysql = &mysqlBackend{db: db, cfg: mysqlCfg, version: ver}
		s.dbCfg = mysqlCfg
		if exists {
			loaded, err := loadStateFromMySQL(ctx, db, key)
			if err != nil {
				_ = db.Close()
				return nil, err
			}
			s.state = loaded
			return s, nil
		}
		// Empty MySQL DB on startup: seed from existing state.json if present, else fresh state.
		if err := s.loadFileIntoMemory(); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := saveStateToMySQL(ctx, db, key, s.state); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	}

	if err := s.loadFileIntoMemory(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadFileIntoMemory() error {
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = State{
			SchemaVersion: SchemaVersion,
			ID:            NewID(),
			Name:          "NFR",
			CreatedAt:     time.Now().UTC(),
			Settings: Settings{
				PortRange: [2]int{20000, 29999},
			},
		}
		if s.mysql == nil {
			return s.saveFileLocked()
		}
		return nil
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return fmt.Errorf("parse %s: %w", s.path, err)
		}
		if s.state.Settings.PortRange[1] == 0 {
			s.state.Settings.PortRange = [2]int{20000, 29999}
		}
		needsSave := false
		if s.state.Settings.AuthToken != "" {
			if strings.HasPrefix(s.state.Settings.AuthToken, encPrefix) {
				plain, err := decryptSecret(s.key, s.state.Settings.AuthToken)
				if err != nil {
					return fmt.Errorf("decrypt auth_token: %w", err)
				}
				s.state.Settings.AuthToken = plain
			} else {
				needsSave = true
			}
		}
		for i := range s.state.Remotes {
			ak := s.state.Remotes[i].APIKey
			if ak == "" {
				continue
			}
			if !strings.HasPrefix(ak, encPrefix) {
				needsSave = true
				continue
			}
			plain, err := decryptSecret(s.key, ak)
			if err != nil {
				return fmt.Errorf("decrypt remote %s api key: %w", s.state.Remotes[i].ID, err)
			}
			s.state.Remotes[i].APIKey = plain
		}
		for i := range s.state.Users {
			ut := s.state.Users[i].AuthToken
			if ut == "" {
				continue
			}
			if !strings.HasPrefix(ut, encPrefix) {
				needsSave = true
				continue
			}
			plain, err := decryptSecret(s.key, ut)
			if err != nil {
				return fmt.Errorf("decrypt user %s auth_token: %w", s.state.Users[i].ID, err)
			}
			s.state.Users[i].AuthToken = plain
		}
		if s.state.NormalizeLegacyUsers() {
			needsSave = true
		}
		if s.state.SchemaVersion < SchemaVersion {
			s.state.SchemaVersion = SchemaVersion
			needsSave = true
		}
		if s.state.Name == "nfr" {
			s.state.Name = "NFR"
			needsSave = true
		}
		if needsSave && s.mysql == nil {
			return s.saveFileLocked()
		}
		return nil
	}
}

// Close closes the active MySQL pool if any.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mysql != nil && s.mysql.db != nil {
		return s.mysql.db.Close()
	}
	return nil
}

// DBStatus returns current storage mode and masked MySQL connection summary.
func (s *Store) DBStatus() DBStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mysql != nil {
		return DBStatus{
			Mode:           "mysql",
			Configured:     true,
			FromConfig:     s.dbFromConfig,
			MandatorySetup: false,
			Host:           s.dbCfg.Host,
			Port:           s.dbCfg.Port,
			Database:       s.dbCfg.Database,
			Username:       s.dbCfg.Username,
			HasPassword:    s.dbCfg.Password != "",
			Version:        s.mysql.version,
		}
	}
	return DBStatus{
		Mode:           "file",
		Configured:     false,
		FromConfig:     false,
		MandatorySetup: os.Getenv("NFR_STORAGE") != "file",
		Host:           "127.0.0.1",
		Port:           3306,
	}
}

// TestMySQL tests connectivity and table-creation permission on cfg without
// modifying any live tables or state.
func (s *Store) TestMySQL(ctx context.Context, cfg DBConfig) (string, error) {
	s.mu.Lock()
	cur := s.dbCfg
	hasCur := s.mysql != nil
	s.mu.Unlock()
	cfg = cfg.Normalized()
	if cfg.Password == "" && hasCur && cfg.Host == cur.Host && cfg.Port == cur.Port && cfg.Database == cur.Database && cfg.Username == cur.Username {
		cfg.Password = cur.Password
	}
	db, ver, err := openMySQLPool(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer db.Close()
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if _, err := db.ExecContext(tctx, "CREATE TEMPORARY TABLE _nfr_perm_probe (id INT NOT NULL PRIMARY KEY)"); err != nil {
		return "", fmt.Errorf("MySQL 连接成功，但缺少建表权限: %w", err)
	}
	_, _ = db.ExecContext(tctx, "DROP TEMPORARY TABLE IF EXISTS _nfr_perm_probe")
	return ver, nil
}

// SwitchToMySQL connects to cfg, initializes tables, migrates current state if
// the target database is empty (or safely adopts existing data if populated and
// secret.key matches), saves encrypted db.json, and hot-swaps the active backend.
func (s *Store) SwitchToMySQL(ctx context.Context, cfg DBConfig) (adoptedExisting bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbFromConfig {
		return false, errors.New("当前数据库由配置文件或环境变量托管，请修改 server.toml / NFR_DB_* 后重启")
	}
	cfg = cfg.Normalized()
	if cfg.Password == "" && s.mysql != nil && cfg.Host == s.dbCfg.Host && cfg.Port == s.dbCfg.Port && cfg.Database == s.dbCfg.Database && cfg.Username == s.dbCfg.Username {
		cfg.Password = s.dbCfg.Password
	}
	db, ver, err := openMySQLPool(ctx, cfg)
	if err != nil {
		return false, err
	}
	if err := ensureMySQLSchema(ctx, db); err != nil {
		_ = db.Close()
		return false, err
	}
	exists, keyHash, err := checkMySQLMeta(ctx, db)
	if err != nil {
		_ = db.Close()
		return false, err
	}

	var nextState State
	if exists {
		if keyHash != "" && keyHash != KeyFingerprint(s.key) {
			_ = db.Close()
			return false, errors.New("目标数据库已有数据，但本地加密密钥 (secret.key) 不匹配；请拷贝原 data_dir/secret.key 或配置 NFR_SECRET_KEY 后重试")
		}
		loaded, err := loadStateFromMySQL(ctx, db, s.key)
		if err != nil {
			_ = db.Close()
			return false, err
		}
		nextState = loaded
		adoptedExisting = true
	} else {
		nextState = cloneState(s.state)
		nextState.NormalizeLegacyUsers()
		nextState.SchemaVersion = SchemaVersion
		if err := saveStateToMySQL(ctx, db, s.key, nextState); err != nil {
			_ = db.Close()
			return false, err
		}
		adoptedExisting = false
	}

	if err := saveDBConfigFile(s.dir, s.key, cfg); err != nil {
		_ = db.Close()
		return false, fmt.Errorf("保存 db.json 失败: %w", err)
	}
	old := s.mysql
	s.mysql = &mysqlBackend{db: db, cfg: cfg, version: ver}
	s.dbCfg = cfg
	s.state = nextState
	if old != nil && old.db != nil {
		_ = old.db.Close()
	}
	return adoptedExisting, nil
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
	_, err := s.UpdateIf(func(st *State) (bool, error) {
		if err := fn(st); err != nil {
			return false, err
		}
		return true, nil
	})
	return err
}

// UpdateIf applies fn and persists the result only when fn reports changed=true
// and err==nil. When changed is false, Revision is not bumped and nothing is
// written to disk.
func (s *Store) UpdateIf(fn func(*State) (bool, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneState(s.state)
	changed, err := fn(&s.state)
	if err != nil {
		s.state = backup
		return false, err
	}
	if !changed {
		s.state = backup
		return false, nil
	}
	s.state.Revision = backup.Revision + 1
	if err := s.saveLocked(); err != nil {
		s.state = backup
		return false, err
	}
	return true, nil
}

// Snapshot returns a deep copy safe to hand to HTTP handlers.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func cloneState(in State) State {
	out := in
	if in.PanelSessions != nil {
		out.PanelSessions = make([]PanelSession, len(in.PanelSessions))
		copy(out.PanelSessions, in.PanelSessions)
	}
	if in.Users != nil {
		out.Users = make([]User, len(in.Users))
		for i, u := range in.Users {
			u.AvatarData = cloneBytes(u.AvatarData)
			out.Users[i] = u
		}
	}
	if in.Tunnels != nil {
		out.Tunnels = make([]Tunnel, len(in.Tunnels))
		for i, t := range in.Tunnels {
			t.Hosts = cloneStrings(t.Hosts)
			out.Tunnels[i] = t
		}
	}
	if in.Nodes != nil {
		out.Nodes = make([]Node, len(in.Nodes))
		for i, n := range in.Nodes {
			n.LastSeen = cloneTimePtr(n.LastSeen)
			out.Nodes[i] = n
		}
	}
	if in.Joins != nil {
		out.Joins = make([]auth.JoinCode, len(in.Joins))
		for i, j := range in.Joins {
			j.UsedAt = cloneTimePtr(j.UsedAt)
			out.Joins[i] = j
		}
	}
	if in.Leases != nil {
		out.Leases = make([]Lease, len(in.Leases))
		copy(out.Leases, in.Leases)
	}
	if in.Remotes != nil {
		out.Remotes = make([]Remote, len(in.Remotes))
		for i, r := range in.Remotes {
			r.LastSeen = cloneTimePtr(r.LastSeen)
			out.Remotes[i] = r
		}
	}
	if in.APIKeys != nil {
		out.APIKeys = make([]auth.APIKey, len(in.APIKeys))
		for i, k := range in.APIKeys {
			k.LastUsed = cloneTimePtr(k.LastUsed)
			k.AllowFrom = cloneStrings(k.AllowFrom)
			out.APIKeys[i] = k
		}
	}
	return out
}

func (s *Store) saveLocked() error {
	if s.mysql != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return saveStateToMySQL(ctx, s.mysql.db, s.key, s.state)
	}
	return s.saveFileLocked()
}

func (s *Store) saveFileLocked() error {
	disk := cloneState(s.state)
	if disk.Settings.AuthToken != "" {
		enc, err := encryptSecret(s.key, disk.Settings.AuthToken)
		if err != nil {
			return err
		}
		disk.Settings.AuthToken = enc
	}
	for i := range disk.Remotes {
		if disk.Remotes[i].APIKey != "" {
			enc, err := encryptSecret(s.key, disk.Remotes[i].APIKey)
			if err != nil {
				return err
			}
			disk.Remotes[i].APIKey = enc
		}
	}
	for i := range disk.Users {
		if disk.Users[i].AuthToken != "" {
			enc, err := encryptSecret(s.key, disk.Users[i].AuthToken)
			if err != nil {
				return err
			}
			disk.Users[i].AuthToken = enc
		}
	}
	body, err := json.MarshalIndent(disk, "", "  ")
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

// AdminUser returns the sole admin account, if initialized.
func (st *State) AdminUser() (*User, bool) {
	for i := range st.Users {
		if st.Users[i].Role == RoleAdmin {
			return &st.Users[i], true
		}
	}
	return nil, false
}

// UserByID finds a user by ID.
func (st *State) UserByID(id string) (*User, bool) {
	for i := range st.Users {
		if st.Users[i].ID == id {
			return &st.Users[i], true
		}
	}
	return nil, false
}

// UserByUsername finds a user by case-insensitive username.
func (st *State) UserByUsername(username string) (*User, bool) {
	want := strings.TrimSpace(username)
	if want == "" {
		return nil, false
	}
	for i := range st.Users {
		if strings.EqualFold(st.Users[i].Username, want) {
			return &st.Users[i], true
		}
	}
	return nil, false
}

// UserByAuthToken finds a user matching the given shared auth_token in constant time.
func (st *State) UserByAuthToken(token string) (*User, bool) {
	want := strings.TrimSpace(token)
	if want == "" {
		return nil, false
	}
	for i := range st.Users {
		if st.Users[i].AuthToken != "" && auth.ConstantTimeEqual(st.Users[i].AuthToken, want) {
			return &st.Users[i], true
		}
	}
	return nil, false
}

// NormalizeLegacyUsers upgrades a single-admin v1 state by creating the default
// "admin" user from PanelPassword / Settings.AuthToken and backfilling empty
// OwnerID fields on existing tunnels, nodes, sessions, join codes, and API keys.
func (st *State) NormalizeLegacyUsers() bool {
	changed := false
	now := time.Now().UTC()
	admin, hasAdmin := st.AdminUser()
	if !hasAdmin && (st.PanelPassword != "" || len(st.Tunnels) > 0 || len(st.Nodes) > 0 || len(st.PanelSessions) > 0) {
		tok := strings.TrimSpace(st.Settings.AuthToken)
		if tok == "" {
			tok = auth.NewSecret("nfr_t_", 18)
			st.Settings.AuthToken = tok
		}
		u := User{
			ID:           NewID(),
			Username:     "admin",
			Role:         RoleAdmin,
			PasswordHash: st.PanelPassword,
			AuthToken:    tok,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		st.Users = append(st.Users, u)
		admin = &st.Users[len(st.Users)-1]
		hasAdmin = true
		changed = true
	}
	if !hasAdmin {
		return changed
	}
	if admin.AuthToken == "" {
		tok := strings.TrimSpace(st.Settings.AuthToken)
		if tok == "" {
			tok = auth.NewSecret("nfr_t_", 18)
		}
		admin.AuthToken = tok
		st.Settings.AuthToken = tok
		changed = true
	}
	if admin.PasswordHash == "" && st.PanelPassword != "" {
		admin.PasswordHash = st.PanelPassword
		changed = true
	}
	for i := range st.Tunnels {
		if st.Tunnels[i].OwnerID == "" {
			st.Tunnels[i].OwnerID = admin.ID
			changed = true
		}
	}
	for i := range st.Nodes {
		if st.Nodes[i].OwnerID == "" {
			st.Nodes[i].OwnerID = admin.ID
			changed = true
		}
	}
	for i := range st.PanelSessions {
		if st.PanelSessions[i].UserID == "" {
			st.PanelSessions[i].UserID = admin.ID
			changed = true
		}
	}
	for i := range st.Joins {
		if st.Joins[i].OwnerID == "" {
			st.Joins[i].OwnerID = admin.ID
			changed = true
		}
	}
	for i := range st.APIKeys {
		if st.APIKeys[i].OwnerID == "" {
			st.APIKeys[i].OwnerID = admin.ID
			changed = true
		}
	}
	return changed
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

// NodeByName finds a node by case-insensitive display name.
func (st *State) NodeByName(name string) (*Node, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return nil, false
	}
	for i := range st.Nodes {
		if strings.EqualFold(st.Nodes[i].Name, want) {
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

// PruneLeases drops expired reservations and reports whether any were removed.
func (st *State) PruneLeases(now time.Time) bool {
	if len(st.Leases) == 0 {
		return false
	}
	kept := make([]Lease, 0, len(st.Leases))
	for _, l := range st.Leases {
		if now.Before(l.ExpiresAt) {
			kept = append(kept, l)
		}
	}
	removed := len(kept) != len(st.Leases)
	st.Leases = kept
	return removed
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

// PruneJoins removes expired or stale redeemed codes and reports whether any
// were removed.
func (st *State) PruneJoins(now time.Time) bool {
	if len(st.Joins) == 0 {
		return false
	}
	kept := make([]auth.JoinCode, 0, len(st.Joins))
	for _, j := range st.Joins {
		if j.UsedAt == nil && now.After(j.ExpiresAt) {
			continue
		}
		if j.UsedAt != nil && now.Sub(*j.UsedAt) > time.Hour {
			continue
		}
		kept = append(kept, j)
	}
	removed := len(kept) != len(st.Joins)
	st.Joins = kept
	return removed
}

// FindJoinCodeByTunnel finds the first unused, unexpired join code bound to a
// tunnel.
func FindJoinCodeByTunnel(st *State, tunnelID string) (*auth.JoinCode, bool) {
	now := time.Now()
	for i := range st.Joins {
		j := &st.Joins[i]
		if j.TunnelID == tunnelID && j.UsedAt == nil && j.Valid(now) == nil {
			return j, true
		}
	}
	return nil, false
}

// DeleteJoinsByTunnel removes every join code bound to a tunnel.
func DeleteJoinsByTunnel(st *State, tunnelID string) {
	if len(st.Joins) == 0 {
		return
	}
	kept := make([]auth.JoinCode, 0, len(st.Joins))
	for _, j := range st.Joins {
		if j.TunnelID == tunnelID {
			continue
		}
		kept = append(kept, j)
	}
	st.Joins = kept
}
