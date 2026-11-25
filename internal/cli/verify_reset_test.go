package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

func TestServerVerifyAndResetPassword(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.toml")
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "127.0.0.1:18443"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	out := captureStdout(t, func() {
		cmdServerVerify([]string{"-c", cfgPath})
	})
	if !strings.Contains(out, "syntax is ok") {
		t.Fatalf("unexpected verify output: %q", out)
	}

	// Park an initial password file; reset-password must replace the hash and clear it.
	parked := server.InitialPasswordPath(dir)
	if err := os.WriteFile(parked, []byte("old"), 0o600); err != nil {
		t.Fatalf("write parked: %v", err)
	}
	out = captureStdout(t, func() {
		cmdResetPassword([]string{"-c", cfgPath, "new-strong-pass"})
	})
	if !strings.Contains(out, "已更新") {
		t.Fatalf("unexpected reset-password output: %q", out)
	}
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Fatalf("reset-password did not remove parked initial-password.txt: %v", err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(st.Snapshot().PanelPassword), []byte("new-strong-pass")); err != nil {
		t.Fatalf("reset password hash does not verify: %v", err)
	}
}

func TestPlaintextPasswordSeedsFirstRun(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.Panel.Password = "docker-first-run-pw"

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, generated, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if generated != "" {
		t.Fatalf("expected no random password when Panel.Password is supplied, got %q", generated)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(st.Snapshot().PanelPassword), []byte("docker-first-run-pw")); err != nil {
		t.Fatalf("seeded password hash mismatch: %v", err)
	}
}
