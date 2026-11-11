package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Under systemd the first-run banner goes to the journal only, and plenty of
// hosts have no journal at all (containers, journald disabled). Park the same
// banner — password included — inside the data directory so there is always
// one place an operator can read it from.
func TestKeepInitialPasswordParksTheBanner(t *testing.T) {
	dir := t.TempDir()
	banner := "\n  面板      https://203.0.113.10:8443/nfr/\n  密码      k3m9xw2p\n"

	if err := keepInitialPassword(dir, banner); err != nil {
		t.Fatalf("keep first-run password: %v", err)
	}

	path := filepath.Join(dir, "initial-password.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("parked file is missing: %v", err)
	}
	if string(raw) != banner {
		t.Fatalf("parked file lost the banner:\n%s", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("parked password is readable by others: mode %o", perm)
	}
}

// A stale copy of a secret is worse than no copy, so the panel deletes the
// file the moment the operator signs in successfully.

// `nfrs status` is what an operator runs when the banner is long gone, so it
// has to name the parked file when there is one.
func TestServerStatusPointsAtParkedPassword(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.toml")
	body := "control_listen = \":9443\"\ndata_dir = \"" + dir + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "initial-password.txt"), []byte("  密码 k3m9xw2p\n"), 0o600); err != nil {
		t.Fatalf("park password: %v", err)
	}

	out := captureStdout(t, func() { cmdServerStatus([]string{"-c", cfgPath}) })
	if !strings.Contains(out, "initial-password.txt") {
		t.Fatalf("status never mentions the parked password:\n%s", out)
	}
}

// Once the operator has signed in, the hint disappears with the file.
func TestServerStatusHidesTheHintAfterLogin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.toml")
	body := "control_listen = \":9443\"\ndata_dir = \"" + dir + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() { cmdServerStatus([]string{"-c", cfgPath}) })
	if strings.Contains(out, "initial-password.txt") {
		t.Fatalf("status promises a parked password that does not exist:\n%s", out)
	}
}
