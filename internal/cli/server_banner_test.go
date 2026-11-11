package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solitudeit/nfr/internal/config"
)

// Every start has to say where the console lives: on a headless box the
// operator reads systemctl status or the journal, and "which URL was it again"
// is the single most common question after an upgrade restarted the process.
func TestServerBannerAlwaysShowsConsoleAddress(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.Domain = "panel.example.com"

	out := serverBanner(cfg, "", "ab:cd:ef")
	if !strings.Contains(out, "面板") {
		t.Fatalf("banner lost the console label:\n%s", out)
	}
	if !strings.Contains(out, "https://panel.example.com:8443/nfr/") {
		t.Fatalf("banner does not show the console URL:\n%s", out)
	}
}

// The password is one-time by design. Repeating it on every start turns a
// secret into a permanent journal entry, so the second start must not carry it.
func TestServerBannerOmitsPasswordAfterFirstRun(t *testing.T) {
	out := serverBanner(config.DefaultServer(), "", "ab:cd:ef")
	if strings.Contains(out, "密码") {
		t.Fatalf("banner repeats the password on a later start:\n%s", out)
	}
}

// The first start still has to hand over the one-time password and say that it
// will not come back.
func TestServerBannerShowsPasswordOnFirstRun(t *testing.T) {
	out := serverBanner(config.DefaultServer(), "k3m9xw2p", "ab:cd:ef")
	if !strings.Contains(out, "k3m9xw2p") {
		t.Fatalf("first-run banner is missing the password:\n%s", out)
	}
	if !strings.Contains(out, "只显示这一次") {
		t.Fatalf("first-run banner does not warn that the password is one-time:\n%s", out)
	}
	if !strings.Contains(out, "ab:cd:ef") {
		t.Fatalf("banner is missing the certificate pin:\n%s", out)
	}
}

// `nfrs status` is the other place an operator looks after the banner scrolled
// away, so it has to name the console too.
func TestServerStatusPrintsConsoleURL(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "server.toml")
	body := "control_listen = \":9443\"\ndomain = \"panel.example.com\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() { cmdServerStatus([]string{"-c", cfgPath}) })
	if !strings.Contains(out, "https://panel.example.com:9443/nfr/") {
		t.Fatalf("status does not print the console URL:\n%s", out)
	}
}
