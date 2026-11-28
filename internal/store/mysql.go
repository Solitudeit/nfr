package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/solitudeit/nfr/internal/auth"
)

type mysqlBackend struct {
	db      *sql.DB
	cfg     DBConfig
	version string
}

var mysqlDDL = []string{
	`CREATE TABLE IF NOT EXISTS nfr_meta (
		id TINYINT NOT NULL PRIMARY KEY,
		schema_version INT NOT NULL,
		instance_id VARCHAR(64) NOT NULL,
		name VARCHAR(128) NOT NULL,
		revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
		key_hash VARCHAR(64) NOT NULL DEFAULT '',
		panel_password VARCHAR(255) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL,
		settings_json LONGTEXT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_users (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		username VARCHAR(64) NOT NULL,
		role VARCHAR(16) NOT NULL,
		password_hash VARCHAR(255) NOT NULL DEFAULT '',
		auth_token_enc TEXT NOT NULL,
		disabled TINYINT(1) NOT NULL DEFAULT 0,
		max_tunnels INT NOT NULL DEFAULT 0,
		max_bandwidth_kb INT NOT NULL DEFAULT 0,
		monthly_quota_mb BIGINT NOT NULL DEFAULT 0,
		used_bytes BIGINT NOT NULL DEFAULT 0,
		usage_month VARCHAR(16) NOT NULL DEFAULT '',
		avatar_mime VARCHAR(64) NOT NULL DEFAULT '',
		avatar_data MEDIUMBLOB,
		avatar_ver BIGINT NOT NULL DEFAULT 0,
		created_at DATETIME(6) NOT NULL,
		updated_at DATETIME(6) NOT NULL,
		UNIQUE KEY uk_nfr_users_username (username)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_sessions (
		token_hash VARCHAR(128) NOT NULL PRIMARY KEY,
		user_id VARCHAR(64) NOT NULL,
		csrf VARCHAR(128) NOT NULL,
		expires_at DATETIME(6) NOT NULL,
		KEY idx_nfr_sessions_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_tunnels (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		owner_id VARCHAR(64) NOT NULL,
		name VARCHAR(128) NOT NULL,
		node_id VARCHAR(64) NOT NULL DEFAULT '',
		type VARCHAR(16) NOT NULL,
		local_addr VARCHAR(255) NOT NULL,
		remote_port INT NOT NULL DEFAULT 0,
		hosts_json TEXT,
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		bandwidth_kb INT NOT NULL DEFAULT 0,
		quota_mb BIGINT NOT NULL DEFAULT 0,
		used_bytes BIGINT NOT NULL DEFAULT 0,
		usage_month VARCHAR(16) NOT NULL DEFAULT '',
		quota_paused TINYINT(1) NOT NULL DEFAULT 0,
		quota_reason VARCHAR(32) NOT NULL DEFAULT '',
		managed_by_config TINYINT(1) NOT NULL DEFAULT 0,
		created_at DATETIME(6) NOT NULL,
		updated_at DATETIME(6) NOT NULL,
		KEY idx_nfr_tunnels_owner (owner_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_nodes (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		owner_id VARCHAR(64) NOT NULL,
		name VARCHAR(128) NOT NULL,
		token_hash VARCHAR(128) NOT NULL,
		os VARCHAR(32) NOT NULL DEFAULT '',
		arch VARCHAR(32) NOT NULL DEFAULT '',
		version VARCHAR(64) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL,
		last_seen DATETIME(6) NULL,
		last_remote VARCHAR(128) NOT NULL DEFAULT '',
		KEY idx_nfr_nodes_owner (owner_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_join_codes (
		code VARCHAR(32) NOT NULL PRIMARY KEY,
		owner_id VARCHAR(64) NOT NULL,
		node_name VARCHAR(128) NOT NULL,
		tunnel_id VARCHAR(64) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL,
		expires_at DATETIME(6) NOT NULL,
		used_at DATETIME(6) NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_leases (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		port INT NOT NULL,
		network VARCHAR(16) NOT NULL DEFAULT 'tcp',
		tunnel_id VARCHAR(64) NOT NULL DEFAULT '',
		expires_at DATETIME(6) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_remotes (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		name VARCHAR(128) NOT NULL,
		url VARCHAR(512) NOT NULL,
		api_key_enc TEXT NOT NULL,
		pin VARCHAR(128) NOT NULL DEFAULT '',
		ca TINYINT(1) NOT NULL DEFAULT 0,
		version VARCHAR(64) NOT NULL DEFAULT '',
		nodes INT NOT NULL DEFAULT 0,
		tunnels INT NOT NULL DEFAULT 0,
		created_at DATETIME(6) NOT NULL,
		last_seen DATETIME(6) NULL,
		last_error TEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	`CREATE TABLE IF NOT EXISTS nfr_api_keys (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		owner_id VARCHAR(64) NOT NULL,
		label VARCHAR(128) NOT NULL,
		hash VARCHAR(128) NOT NULL,
		scope VARCHAR(32) NOT NULL,
		allow_from_json TEXT,
		created_at DATETIME(6) NOT NULL,
		last_used DATETIME(6) NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

func openMySQLPool(ctx context.Context, cfg DBConfig) (*sql.DB, string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, "", fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	var ver string
	_ = db.QueryRowContext(pingCtx, "SELECT VERSION()").Scan(&ver)
	return db, strings.TrimSpace(ver), nil
}

func openMySQLWithRetry(ctx context.Context, cfg DBConfig, attempts int) (*sql.DB, string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		db, ver, err := openMySQLPool(ctx, cfg)
		if err == nil {
			return db, ver, nil
		}
		lastErr = err
		if i+1 < attempts {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil, "", lastErr
}

func ensureMySQLSchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range mysqlDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("初始化 MySQL 表结构失败: %w", err)
		}
	}
	return nil
}

func checkMySQLMeta(ctx context.Context, db *sql.DB) (exists bool, keyHash string, err error) {
	err = db.QueryRowContext(ctx, "SELECT key_hash FROM nfr_meta WHERE id = 1").Scan(&keyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, strings.TrimSpace(keyHash), nil
}

func utcOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil || t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

func fromNullTime(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}

func saveStateToMySQL(ctx context.Context, db *sql.DB, key []byte, in State) error {
	disk := cloneState(in)
	if disk.Settings.AuthToken != "" {
		enc, err := encryptSecret(key, disk.Settings.AuthToken)
		if err != nil {
			return err
		}
		disk.Settings.AuthToken = enc
	}
	settingsJSON, err := json.Marshal(disk.Settings)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nfr_meta (id, schema_version, instance_id, name, revision, key_hash, panel_password, created_at, settings_json)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			schema_version = VALUES(schema_version),
			instance_id = VALUES(instance_id),
			name = VALUES(name),
			revision = VALUES(revision),
			key_hash = VALUES(key_hash),
			panel_password = VALUES(panel_password),
			created_at = VALUES(created_at),
			settings_json = VALUES(settings_json)`,
		disk.SchemaVersion, disk.ID, disk.Name, disk.Revision, KeyFingerprint(key),
		disk.PanelPassword, utcOrNow(disk.CreatedAt), string(settingsJSON),
	); err != nil {
		return fmt.Errorf("write nfr_meta: %w", err)
	}

	for _, tbl := range []string{
		"nfr_users", "nfr_sessions", "nfr_tunnels", "nfr_nodes",
		"nfr_join_codes", "nfr_leases", "nfr_remotes", "nfr_api_keys",
	} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
			return fmt.Errorf("clear %s: %w", tbl, err)
		}
	}

	for _, u := range disk.Users {
		encTok, err := encryptSecret(key, u.AuthToken)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_users (
				id, username, role, password_hash, auth_token_enc, disabled,
				max_tunnels, max_bandwidth_kb, monthly_quota_mb, used_bytes, usage_month,
				avatar_mime, avatar_data, avatar_ver, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.Username, u.Role, u.PasswordHash, encTok, u.Disabled,
			u.MaxTunnels, u.MaxBandwidthKB, u.MonthlyQuotaMB, u.UsedBytes, u.UsageMonth,
			u.AvatarMime, u.AvatarData, u.AvatarVer, utcOrNow(u.CreatedAt), utcOrNow(u.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert user %s: %w", u.Username, err)
		}
	}

	for _, ps := range disk.PanelSessions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_sessions (token_hash, user_id, csrf, expires_at)
			VALUES (?, ?, ?, ?)`,
			ps.TokenHash, ps.UserID, ps.CSRF, utcOrNow(ps.ExpiresAt),
		); err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
	}

	for _, t := range disk.Tunnels {
		hostsRaw, _ := json.Marshal(t.Hosts)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_tunnels (
				id, owner_id, name, node_id, type, local_addr, remote_port, hosts_json,
				enabled, bandwidth_kb, quota_mb, used_bytes, usage_month, quota_paused,
				quota_reason, managed_by_config, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.OwnerID, t.Name, t.NodeID, t.Type, t.Local, t.RemotePort, string(hostsRaw),
			t.Enabled, t.BandwidthKB, t.QuotaMB, t.UsedBytes, t.UsageMonth, t.QuotaPaused,
			t.QuotaReason, t.ManagedByConfig, utcOrNow(t.CreatedAt), utcOrNow(t.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert tunnel %s: %w", t.ID, err)
		}
	}

	for _, n := range disk.Nodes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_nodes (
				id, owner_id, name, token_hash, os, arch, version, created_at, last_seen, last_remote
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			n.ID, n.OwnerID, n.Name, n.TokenHash, n.OS, n.Arch, n.Version,
			utcOrNow(n.CreatedAt), nullTime(n.LastSeen), n.LastRemote,
		); err != nil {
			return fmt.Errorf("insert node %s: %w", n.ID, err)
		}
	}

	for _, j := range disk.Joins {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_join_codes (code, owner_id, node_name, tunnel_id, created_at, expires_at, used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			j.Code, j.OwnerID, j.NodeName, j.TunnelID,
			utcOrNow(j.CreatedAt), utcOrNow(j.ExpiresAt), nullTime(j.UsedAt),
		); err != nil {
			return fmt.Errorf("insert join_code: %w", err)
		}
	}

	for _, l := range disk.Leases {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_leases (id, port, network, tunnel_id, expires_at)
			VALUES (?, ?, ?, ?, ?)`,
			l.ID, l.Port, l.Network, l.TunnelID, utcOrNow(l.ExpiresAt),
		); err != nil {
			return fmt.Errorf("insert lease: %w", err)
		}
	}

	for _, r := range disk.Remotes {
		encKey, err := encryptSecret(key, r.APIKey)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_remotes (
				id, name, url, api_key_enc, pin, ca, version, nodes, tunnels, created_at, last_seen, last_error
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.URL, encKey, r.Pin, r.CA, r.Version, r.Nodes, r.Tunnels,
			utcOrNow(r.CreatedAt), nullTime(r.LastSeen), r.LastError,
		); err != nil {
			return fmt.Errorf("insert remote %s: %w", r.ID, err)
		}
	}

	for _, k := range disk.APIKeys {
		allowRaw, _ := json.Marshal(k.AllowFrom)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO nfr_api_keys (id, owner_id, label, hash, scope, allow_from_json, created_at, last_used)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			k.ID, k.OwnerID, k.Label, k.Hash, k.Scope, string(allowRaw),
			utcOrNow(k.CreatedAt), nullTime(k.LastUsed),
		); err != nil {
			return fmt.Errorf("insert api_key %s: %w", k.ID, err)
		}
	}

	return tx.Commit()
}

func loadStateFromMySQL(ctx context.Context, db *sql.DB, key []byte) (State, error) {
	var st State
	var keyHash, settingsJSON string
	if err := db.QueryRowContext(ctx, `
		SELECT schema_version, instance_id, name, revision, key_hash, panel_password, created_at, settings_json
		FROM nfr_meta WHERE id = 1`,
	).Scan(&st.SchemaVersion, &st.ID, &st.Name, &st.Revision, &keyHash, &st.PanelPassword, &st.CreatedAt, &settingsJSON); err != nil {
		return State{}, fmt.Errorf("read nfr_meta: %w", err)
	}
	if keyHash != "" && keyHash != KeyFingerprint(key) {
		return State{}, errors.New("目标数据库已有数据，但本地加密密钥 (secret.key) 不匹配；请拷贝原 data_dir/secret.key 或配置 NFR_SECRET_KEY 后重试")
	}
	if err := json.Unmarshal([]byte(settingsJSON), &st.Settings); err != nil {
		return State{}, fmt.Errorf("parse nfr_meta.settings_json: %w", err)
	}
	if st.Settings.PortRange[1] == 0 {
		st.Settings.PortRange = [2]int{20000, 29999}
	}
	if st.Settings.AuthToken != "" {
		plain, err := decryptSecret(key, st.Settings.AuthToken)
		if err != nil {
			return State{}, fmt.Errorf("decrypt settings.auth_token: %w", err)
		}
		st.Settings.AuthToken = plain
	}

	// Users
	rows, err := db.QueryContext(ctx, `
		SELECT id, username, role, password_hash, auth_token_enc, disabled,
			max_tunnels, max_bandwidth_kb, monthly_quota_mb, used_bytes, usage_month,
			avatar_mime, avatar_data, avatar_ver, created_at, updated_at
		FROM nfr_users ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_users: %w", err)
	}
	for rows.Next() {
		var u User
		var encTok string
		if err := rows.Scan(
			&u.ID, &u.Username, &u.Role, &u.PasswordHash, &encTok, &u.Disabled,
			&u.MaxTunnels, &u.MaxBandwidthKB, &u.MonthlyQuotaMB, &u.UsedBytes, &u.UsageMonth,
			&u.AvatarMime, &u.AvatarData, &u.AvatarVer, &u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			rows.Close()
			return State{}, err
		}
		plain, err := decryptSecret(key, encTok)
		if err != nil {
			rows.Close()
			return State{}, fmt.Errorf("decrypt user %s auth_token: %w", u.ID, err)
		}
		u.AuthToken = plain
		st.Users = append(st.Users, u)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_users: %w", err)
	}
	rows.Close()

	// Sessions
	rows, err = db.QueryContext(ctx, `SELECT token_hash, user_id, csrf, expires_at FROM nfr_sessions`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_sessions: %w", err)
	}
	for rows.Next() {
		var ps PanelSession
		if err := rows.Scan(&ps.TokenHash, &ps.UserID, &ps.CSRF, &ps.ExpiresAt); err != nil {
			rows.Close()
			return State{}, err
		}
		st.PanelSessions = append(st.PanelSessions, ps)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_sessions: %w", err)
	}
	rows.Close()

	// Tunnels
	rows, err = db.QueryContext(ctx, `
		SELECT id, owner_id, name, node_id, type, local_addr, remote_port, hosts_json,
			enabled, bandwidth_kb, quota_mb, used_bytes, usage_month, quota_paused,
			quota_reason, managed_by_config, created_at, updated_at
		FROM nfr_tunnels ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_tunnels: %w", err)
	}
	for rows.Next() {
		var t Tunnel
		var hostsJSON sql.NullString
		if err := rows.Scan(
			&t.ID, &t.OwnerID, &t.Name, &t.NodeID, &t.Type, &t.Local, &t.RemotePort, &hostsJSON,
			&t.Enabled, &t.BandwidthKB, &t.QuotaMB, &t.UsedBytes, &t.UsageMonth, &t.QuotaPaused,
			&t.QuotaReason, &t.ManagedByConfig, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			rows.Close()
			return State{}, err
		}
		if hostsJSON.Valid && hostsJSON.String != "" && hostsJSON.String != "null" {
			_ = json.Unmarshal([]byte(hostsJSON.String), &t.Hosts)
		}
		st.Tunnels = append(st.Tunnels, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_tunnels: %w", err)
	}
	rows.Close()

	// Nodes
	rows, err = db.QueryContext(ctx, `
		SELECT id, owner_id, name, token_hash, os, arch, version, created_at, last_seen, last_remote
		FROM nfr_nodes ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_nodes: %w", err)
	}
	for rows.Next() {
		var n Node
		var ls sql.NullTime
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.Name, &n.TokenHash, &n.OS, &n.Arch, &n.Version,
			&n.CreatedAt, &ls, &n.LastRemote,
		); err != nil {
			rows.Close()
			return State{}, err
		}
		n.LastSeen = fromNullTime(ls)
		st.Nodes = append(st.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_nodes: %w", err)
	}
	rows.Close()

	// Join codes
	rows, err = db.QueryContext(ctx, `SELECT code, owner_id, node_name, tunnel_id, created_at, expires_at, used_at FROM nfr_join_codes`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_join_codes: %w", err)
	}
	for rows.Next() {
		var j auth.JoinCode
		var ua sql.NullTime
		if err := rows.Scan(&j.Code, &j.OwnerID, &j.NodeName, &j.TunnelID, &j.CreatedAt, &j.ExpiresAt, &ua); err != nil {
			rows.Close()
			return State{}, err
		}
		j.UsedAt = fromNullTime(ua)
		st.Joins = append(st.Joins, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_join_codes: %w", err)
	}
	rows.Close()

	// Leases
	rows, err = db.QueryContext(ctx, `SELECT id, port, network, tunnel_id, expires_at FROM nfr_leases`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_leases: %w", err)
	}
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.ID, &l.Port, &l.Network, &l.TunnelID, &l.ExpiresAt); err != nil {
			rows.Close()
			return State{}, err
		}
		st.Leases = append(st.Leases, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_leases: %w", err)
	}
	rows.Close()

	// Remotes
	rows, err = db.QueryContext(ctx, `
		SELECT id, name, url, api_key_enc, pin, ca, version, nodes, tunnels, created_at, last_seen, last_error
		FROM nfr_remotes`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_remotes: %w", err)
	}
	for rows.Next() {
		var r Remote
		var encKey string
		var ls sql.NullTime
		var le sql.NullString
		if err := rows.Scan(
			&r.ID, &r.Name, &r.URL, &encKey, &r.Pin, &r.CA, &r.Version, &r.Nodes, &r.Tunnels,
			&r.CreatedAt, &ls, &le,
		); err != nil {
			rows.Close()
			return State{}, err
		}
		plain, err := decryptSecret(key, encKey)
		if err != nil {
			rows.Close()
			return State{}, fmt.Errorf("decrypt remote %s api_key: %w", r.ID, err)
		}
		r.APIKey = plain
		r.LastSeen = fromNullTime(ls)
		if le.Valid {
			r.LastError = le.String
		}
		st.Remotes = append(st.Remotes, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_remotes: %w", err)
	}
	rows.Close()

	// API Keys
	rows, err = db.QueryContext(ctx, `SELECT id, owner_id, label, hash, scope, allow_from_json, created_at, last_used FROM nfr_api_keys`)
	if err != nil {
		return State{}, fmt.Errorf("query nfr_api_keys: %w", err)
	}
	for rows.Next() {
		var k auth.APIKey
		var af sql.NullString
		var lu sql.NullTime
		if err := rows.Scan(&k.ID, &k.OwnerID, &k.Label, &k.Hash, &k.Scope, &af, &k.CreatedAt, &lu); err != nil {
			rows.Close()
			return State{}, err
		}
		if af.Valid && af.String != "" && af.String != "null" {
			_ = json.Unmarshal([]byte(af.String), &k.AllowFrom)
		}
		k.LastUsed = fromNullTime(lu)
		st.APIKeys = append(st.APIKeys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return State{}, fmt.Errorf("iterate nfr_api_keys: %w", err)
	}
	rows.Close()

	st.NormalizeLegacyUsers()
	return st, nil
}
