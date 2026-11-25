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

func TestServerSaveLoadTOMLAndYAML(t *testing.T) {
	for _, name := range []string{"server.toml", "server.yaml", "server.yml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", name)
			cfg := DefaultServer()
			cfg.ControlListen = "127.0.0.1:9443"
			cfg.HTTPSListen = "off"
			cfg.Domain = "example.com"
			cfg.AuthToken = "shared-secret-123"
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
			if got.ControlListen != cfg.ControlListen || got.Domain != cfg.Domain || got.AuthToken != cfg.AuthToken || !got.ACME.Enabled || got.PortRange != cfg.PortRange {
				t.Fatalf("round trip mismatch: %+v", got)
			}
		})
	}
}

func TestAgentSaveLoadTOMLAndYAML(t *testing.T) {
	t.Run("legacy servers list", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agent.toml")
		a := Agent{Servers: []AgentServer{{Addr: "example.com:8443", NodeID: "n1", Token: "t", Pin: "aa", CA: true}}}
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
		if err := got.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})

	t.Run("flat toml with proxies", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client.toml")
		raw := `
server_addr = "203.0.113.10"
auth_token = "tok-xyz"
name = "nas"

[[proxies]]
name = "ssh"
type = "tcp"
local_ip = "127.0.0.1"
local_port = 22
remote_port = 20022

[[proxies]]
name = "blog"
type = "http"
local = "127.0.0.1:8080"
hosts = ["blog.example.com"]
`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok, err := LoadAgent(path)
		if err != nil || !ok {
			t.Fatalf("load: %v ok=%v", err, ok)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		srvs := got.EffectiveServers()
		if len(srvs) != 1 || srvs[0].Addr != "203.0.113.10:8443" || srvs[0].AuthToken != "tok-xyz" || len(srvs[0].Proxies) != 2 {
			t.Fatalf("unexpected effective servers: %+v", srvs)
		}
		if srvs[0].Proxies[0].EffectiveLocal() != "127.0.0.1:22" || srvs[0].Proxies[1].EffectiveLocal() != "127.0.0.1:8080" {
			t.Fatalf("unexpected proxies: %+v", srvs[0].Proxies)
		}
	})

	t.Run("flat yaml with proxies", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client.yaml")
		raw := `
server_addr: "203.0.113.10:8443"
auth_token: "tok-yaml"
name: "office-pc"
proxies:
  - name: rdp
    type: tcp
    local: "127.0.0.1:3389"
    remote_port: 23389
  - name: dns
    type: udp
    local_port: 53
    remote_port: 20053
`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok, err := LoadAgent(path)
		if err != nil || !ok {
			t.Fatalf("load: %v ok=%v", err, ok)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		srvs := got.EffectiveServers()
		if len(srvs) != 1 || srvs[0].Name != "office-pc" || srvs[0].Proxies[1].EffectiveLocal() != "127.0.0.1:53" {
			t.Fatalf("unexpected yaml servers: %+v", srvs)
		}
	})
}

func TestServerApplyEnvAndValidate(t *testing.T) {
	t.Setenv("NFR_CONTROL_LISTEN", "127.0.0.1:9443")
	t.Setenv("NFR_HTTP_LISTEN", "off")
	t.Setenv("NFR_HTTPS_LISTEN", "off")
	t.Setenv("NFR_DOMAIN", "env.example.com")
	t.Setenv("NFR_PUBLIC_HOST", "203.0.113.20")
	t.Setenv("NFR_AUTH_TOKEN", "env-auth-token")
	t.Setenv("NFR_PORT_RANGE", "25000-26000")
	t.Setenv("NFR_PASSWORD", "env-secret-pw")
	t.Setenv("NFR_ACME_ENABLED", "true")
	t.Setenv("NFR_ACME_EMAIL", "ops@example.com")

	cfg := DefaultServer()
	cfg.DataDir = t.TempDir()
	cfg.ApplyEnv()

	if cfg.ControlListen != "127.0.0.1:9443" || cfg.HTTPListen != "off" || cfg.Domain != "env.example.com" || cfg.PublicHost != "203.0.113.20" || cfg.AuthToken != "env-auth-token" {
		t.Fatalf("ApplyEnv missed fields: %+v", cfg)
	}
	if cfg.PortRange != [2]int{25000, 26000} || cfg.Panel.Password != "env-secret-pw" || !cfg.ACME.Enabled || cfg.ACME.Email != "ops@example.com" {
		t.Fatalf("ApplyEnv missed fields: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid env config failed validation: %v", err)
	}

	bad := DefaultServer()
	bad.ControlListen = "not-a-host-port"
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid control_listen passed Validate")
	}

	badACME := DefaultServer()
	badACME.ACME.Enabled = true
	badACME.Domain = ""
	if err := badACME.Validate(); err == nil {
		t.Fatal("ACME without domain passed Validate")
	}
}
