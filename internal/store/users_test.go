package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyStateMigrationCreatesAdminAndBackfillsOwner(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "schema_version": 1,
  "id": "inst1",
  "name": "NFR",
  "panel_password": "$2a$10$legacyhashvalue",
  "panel_sessions": [
    {"token_hash": "sess1", "csrf": "c1", "expires_at": "2099-01-01T00:00:00Z"}
  ],
  "settings": {
    "port_range": [20000, 29999],
    "auth_token": "nfr_t_legacytoken123"
  },
  "tunnels": [
    {"id": "tun1", "name": "ssh", "node_id": "node1", "type": "tcp", "local": "127.0.0.1:22", "remote_port": 20022, "enabled": true}
  ],
  "nodes": [
    {"id": "node1", "name": "home", "token_hash": "th1"}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open legacy state: %v", err)
	}
	snap := st.Snapshot()
	if snap.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", snap.SchemaVersion, SchemaVersion)
	}
	admin, ok := snap.AdminUser()
	if !ok {
		t.Fatal("expected migrated admin user")
	}
	if admin.Username != "admin" || admin.PasswordHash != "$2a$10$legacyhashvalue" || admin.AuthToken != "nfr_t_legacytoken123" {
		t.Fatalf("unexpected admin user: %+v", admin)
	}
	if snap.Tunnels[0].OwnerID != admin.ID || snap.Nodes[0].OwnerID != admin.ID || snap.PanelSessions[0].UserID != admin.ID {
		t.Fatalf("owner backfill failed: tun=%q node=%q sess=%q want=%q",
			snap.Tunnels[0].OwnerID, snap.Nodes[0].OwnerID, snap.PanelSessions[0].UserID, admin.ID)
	}

	// Verify User.AuthToken is encrypted on disk and decrypted on reload.
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "nfr_t_legacytoken123") {
		t.Fatal("plaintext user auth_token leaked to state.json")
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open state: %v", err)
	}
	snap2 := st2.Snapshot()
	if u, ok := snap2.UserByAuthToken("nfr_t_legacytoken123"); !ok || u.ID != admin.ID {
		t.Fatalf("UserByAuthToken failed after reload: ok=%v u=%+v", ok, u)
	}
}

func TestSecretKeyEnvOverrideAndFingerprint(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	t.Setenv("NFR_SECRET_KEY", hexKey)
	dir := t.TempDir()

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with NFR_SECRET_KEY: %v", err)
	}
	now := time.Now().UTC()
	if err := st.Update(func(s *State) error {
		s.Users = append(s.Users, User{
			ID:        "u1",
			Username:  "alice",
			Role:      RoleUser,
			AuthToken: "nfr_t_alice_secret",
			CreatedAt: now,
			UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if fp := KeyFingerprint(st.key); len(fp) != 64 {
		t.Fatalf("unexpected fingerprint length: %q", fp)
	}
	if _, err := os.Stat(filepath.Join(dir, secretKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("expected no secret.key file when NFR_SECRET_KEY is used")
	}
}
