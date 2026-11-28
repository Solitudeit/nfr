package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDBConfigEncryptionAndValidation(t *testing.T) {
	dir := t.TempDir()
	key, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := (DBConfig{Host: "", Database: "db", Username: "u"}).Validate(); err == nil {
		t.Fatal("expected error for missing host")
	}
	if err := (DBConfig{Host: "127.0.0.1", Database: "", Username: "u"}).Validate(); err == nil {
		t.Fatal("expected error for missing database")
	}

	cfg := DBConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		Database: "testdb",
		Username: "tester",
		Password: "super-secret-db-password",
	}
	if err := saveDBConfigFile(dir, key, cfg); err != nil {
		t.Fatalf("saveDBConfigFile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, dbConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-db-password") {
		t.Fatal("plaintext MySQL password leaked into db.json")
	}
	got, ok, err := loadDBConfigFile(dir, key)
	if err != nil || !ok {
		t.Fatalf("loadDBConfigFile: err=%v ok=%v", err, ok)
	}
	if got != cfg {
		t.Fatalf("round-trip DBConfig mismatch: got %+v want %+v", got, cfg)
	}
	if !strings.Contains(cfg.DSN(), "charset=utf8mb4") || !strings.Contains(cfg.DSN(), "parseTime=true") {
		t.Fatalf("unexpected DSN: %s", cfg.DSN())
	}
}

func TestDBStatusFileModeAndMandatorySetupFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NFR_STORAGE", "")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds := st.DBStatus()
	if ds.Mode != "file" || ds.Configured || !ds.MandatorySetup {
		t.Fatalf("expected file mode with mandatory_setup=true, got %+v", ds)
	}

	t.Setenv("NFR_STORAGE", "file")
	ds2 := st.DBStatus()
	if ds2.MandatorySetup {
		t.Fatalf("expected mandatory_setup=false when NFR_STORAGE=file, got %+v", ds2)
	}

	// TestMySQL against an unreachable port returns a clean error without modifying state.
	if _, err := st.TestMySQL(context.Background(), DBConfig{
		Host:     "127.0.0.1",
		Port:     1,
		Database: "nope",
		Username: "u",
	}); err == nil {
		t.Fatal("expected TestMySQL on closed port to fail")
	}
}
