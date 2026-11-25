package cli

import (
	"path/filepath"
	"testing"

	"github.com/solitudeit/nfr/internal/config"
)

func TestInitTemplatesTOMLAndYAML(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"server.toml", "server.yaml"} {
		p := filepath.Join(dir, name)
		if err := writeServerTemplate(p, "example.com", "203.0.113.10", "tok-123456", false); err != nil {
			t.Fatalf("writeServerTemplate(%s): %v", name, err)
		}
		// Must refuse overwrite without --force.
		if err := writeServerTemplate(p, "", "", "", false); err == nil {
			t.Fatalf("writeServerTemplate(%s) overwrote without force", name)
		}
		cfg, ok, err := config.LoadServer(p)
		if err != nil || !ok {
			t.Fatalf("LoadServer(%s): %v ok=%v", name, err, ok)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", name, err)
		}
		if cfg.AuthToken != "tok-123456" || cfg.Domain != "example.com" || cfg.PublicHost != "203.0.113.10" {
			t.Fatalf("unexpected server template %s: %+v", name, cfg)
		}
	}

	for _, name := range []string{"client.toml", "client.yaml"} {
		p := filepath.Join(dir, name)
		if err := writeClientTemplate(p, "203.0.113.10", "tok-123456", "nas", false); err != nil {
			t.Fatalf("writeClientTemplate(%s): %v", name, err)
		}
		if err := writeClientTemplate(p, "203.0.113.10", "tok-123456", "nas", false); err == nil {
			t.Fatalf("writeClientTemplate(%s) overwrote without force", name)
		}
		cfg, ok, err := config.LoadAgent(p)
		if err != nil || !ok {
			t.Fatalf("LoadAgent(%s): %v ok=%v", name, err, ok)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", name, err)
		}
		srvs := cfg.EffectiveServers()
		if len(srvs) != 1 || srvs[0].Addr != "203.0.113.10:8443" || srvs[0].AuthToken != "tok-123456" || srvs[0].Name != "nas" {
			t.Fatalf("unexpected client template %s: %+v", name, srvs)
		}
	}
}
