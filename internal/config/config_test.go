package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServerMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.toml")
	cfg, exists, err := LoadServer(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if exists {
		t.Fatal("a missing file must report exists=false")
	}
	def := DefaultServer()
	if cfg.ControlListen != def.ControlListen || cfg.PortRange != def.PortRange {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestServerSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "server.toml")
	cfg := DefaultServer()
	cfg.ControlListen = "127.0.0.1:9443"
	cfg.HTTPSListen = "off"
	cfg.Domain = "example.com"
	cfg.PortRange = [2]int{30000, 30100}
	cfg.ACME.Enabled = true
	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	got, exists, err := LoadServer(path)
	if err != nil || !exists {
		t.Fatalf("load: %v exists=%v", err, exists)
	}
	if got.ControlListen != cfg.ControlListen || got.Domain != cfg.Domain || !got.ACME.Enabled {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestAgentSaveMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	a := Agent{Servers: []AgentServer{{Addr: "example.com:8443", Token: "t", Pin: "aa", CA: true}}}
	if err := a.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Servers) != 1 || !got.Servers[0].CA || got.Servers[0].Pin != "aa" {
		t.Fatalf("round trip mismatch: %+v", got.Servers)
	}
}
