// Package config reads and writes the bootstrap files (TOML or YAML) that tell
// a server where to listen and a client how to connect. Everything that changes
// dynamically at runtime lives in the state store.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// Server is the bootstrap configuration of a control node.
type Server struct {
	ControlListen string `toml:"control_listen" yaml:"control_listen"`
	HTTPListen    string `toml:"http_listen" yaml:"http_listen"`
	HTTPSListen   string `toml:"https_listen" yaml:"https_listen"`
	DataDir       string `toml:"data_dir" yaml:"data_dir"`
	Domain        string `toml:"domain" yaml:"domain"`
	PublicHost    string `toml:"public_host,omitempty" yaml:"public_host,omitempty"`
	// AuthToken is the shared secret clients present in their config file
	// (client.toml / client.yaml) to authenticate without a one-time join code.
	AuthToken string `toml:"auth_token,omitempty" yaml:"auth_token,omitempty"`
	// TLSCert and TLSKey point at a PEM certificate chain and key. When both
	// are set they replace the self-signed certificate generated on first run.
	TLSCert   string `toml:"tls_cert" yaml:"tls_cert"`
	TLSKey    string `toml:"tls_key" yaml:"tls_key"`
	PortRange [2]int `toml:"port_range" yaml:"port_range"`
	Panel     Panel  `toml:"panel" yaml:"panel"`
	ACME      ACME   `toml:"acme" yaml:"acme"`
	LogLevel  string `toml:"log_level" yaml:"log_level"`
	LogFile   string `toml:"log_file" yaml:"log_file"`
}

// Panel holds the built-in web console settings.
type Panel struct {
	// Password is a bcrypt hash (or plaintext on first run, which is hashed
	// into state.json). Empty generates a random password on first run.
	Password string `toml:"password,omitempty" yaml:"password,omitempty"`
	// HTTPS enforces HTTPS for the web console on control_listen. By default
	// (false) the console is served over plain HTTP so browsers do not hit a
	// self-signed certificate warning; agents still connect over TLS 1.3.
	HTTPS bool `toml:"https,omitempty" yaml:"https,omitempty"`
}

// ACME configures automatic certificate issuance for the HTTPS ingress.
type ACME struct {
	Enabled bool   `toml:"enabled" yaml:"enabled"`
	Email   string `toml:"email" yaml:"email"`
	// CA overrides the ACME directory URL, e.g. the Let's Encrypt staging URL.
	CA string `toml:"ca" yaml:"ca"`
	// Cache is where account and certificate material is kept. It defaults to
	// <data_dir>/acme.
	Cache string `toml:"cache" yaml:"cache"`
}

// ProxyConfig declares one tunnel inside a client configuration file (TOML or YAML).
type ProxyConfig struct {
	Name        string   `toml:"name" yaml:"name"`
	Type        string   `toml:"type" yaml:"type"` // tcp | udp | http
	Local       string   `toml:"local,omitempty" yaml:"local,omitempty"`
	LocalIP     string   `toml:"local_ip,omitempty" yaml:"local_ip,omitempty"`
	LocalPort   int      `toml:"local_port,omitempty,omitzero" yaml:"local_port,omitempty"`
	RemotePort  int      `toml:"remote_port,omitempty,omitzero" yaml:"remote_port,omitempty"`
	Hosts       []string `toml:"hosts,omitempty" yaml:"hosts,omitempty"`
	BandwidthKB int      `toml:"bandwidth_kb,omitempty,omitzero" yaml:"bandwidth_kb,omitempty"`
	QuotaMB     int64    `toml:"quota_mb,omitempty,omitzero" yaml:"quota_mb,omitempty"`
}

// EffectiveLocal resolves either "local" or "local_ip" + "local_port" into host:port.
func (p ProxyConfig) EffectiveLocal() string {
	if s := strings.TrimSpace(p.Local); s != "" {
		return s
	}
	if p.LocalPort > 0 {
		ip := strings.TrimSpace(p.LocalIP)
		if ip == "" {
			ip = "127.0.0.1"
		}
		return net.JoinHostPort(ip, strconv.Itoa(p.LocalPort))
	}
	return ""
}

// Agent is the bootstrap configuration of an edge client.
// Supports both flat single-server config (server_addr + auth_token +
// optional proxies) and multi-server [[server]] / servers lists, in TOML or YAML.
type Agent struct {
	ServerAddr string        `toml:"server_addr,omitempty" yaml:"server_addr,omitempty"`
	AuthToken  string        `toml:"auth_token,omitempty" yaml:"auth_token,omitempty"`
	Name       string        `toml:"name,omitempty" yaml:"name,omitempty"`
	Pin        string        `toml:"pin,omitempty" yaml:"pin,omitempty"`
	CA         bool          `toml:"ca,omitempty" yaml:"ca,omitempty"`
	Proxies    []ProxyConfig `toml:"proxies,omitempty" yaml:"proxies,omitempty"`

	Servers  []AgentServer `toml:"server,omitempty" yaml:"servers,omitempty"`
	LogLevel string        `toml:"log_level,omitempty" yaml:"log_level,omitempty"`
	LogFile  string        `toml:"log_file,omitempty" yaml:"log_file,omitempty"`
}

// AgentServer is one control node the agent connects to.
type AgentServer struct {
	Addr      string        `toml:"addr" yaml:"addr"`
	AuthToken string        `toml:"auth_token,omitempty" yaml:"auth_token,omitempty"`
	NodeID    string        `toml:"node_id,omitempty" yaml:"node_id,omitempty"`
	Token     string        `toml:"token,omitempty" yaml:"token,omitempty"`
	Name      string        `toml:"name,omitempty" yaml:"name,omitempty"`
	Proxies   []ProxyConfig `toml:"proxies,omitempty" yaml:"proxies,omitempty"`
	// Pin is the lowercase hex sha256 fingerprint of the server certificate,
	// learned on first contact (trust on first use).
	Pin string `toml:"pin,omitempty" yaml:"pin,omitempty"`
	// CA verifies the server against the system trust store instead of a
	// pinned fingerprint. It is used when the peer serves a publicly trusted
	// certificate (ACME), where the fingerprint changes on renewal.
	CA bool `toml:"ca,omitempty" yaml:"ca,omitempty"`
}

// IsYAMLPath reports whether path should be decoded/encoded as YAML.
func IsYAMLPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	return ext == ".yaml" || ext == ".yml"
}

// NormalizeAddr adds the default control port (:8443) when omitted.
func NormalizeAddr(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(s, "8443")
}

// DefaultServer returns a usable configuration for a fresh installation.
func DefaultServer() Server {
	return Server{
		ControlListen: ":8443",
		HTTPListen:    ":80",
		HTTPSListen:   ":443",
		DataDir:       "/var/lib/nfr",
		PortRange:     [2]int{20000, 29999},
		LogLevel:      "info",
	}
}

// LoadServer reads path (TOML or YAML by extension). The boolean reports
// whether the file existed.
func LoadServer(path string) (Server, bool, error) {
	cfg := DefaultServer()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, true, err
	}
	if IsYAMLPath(path) {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, true, err
		}
	} else {
		if _, err := toml.Decode(string(raw), &cfg); err != nil {
			return cfg, true, err
		}
	}
	cfg.applyDefaults()
	return cfg, true, nil
}

func (c *Server) applyDefaults() {
	def := DefaultServer()
	if c.ControlListen == "" {
		c.ControlListen = def.ControlListen
	}
	if c.HTTPListen == "" {
		c.HTTPListen = def.HTTPListen
	}
	if c.HTTPSListen == "" {
		c.HTTPSListen = def.HTTPSListen
	}
	if c.DataDir == "" {
		c.DataDir = def.DataDir
	}
	if c.PortRange[0] == 0 || c.PortRange[1] == 0 {
		c.PortRange = def.PortRange
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

// Save writes the configuration (TOML or YAML by extension), creating parent
// directories as needed.
func (c *Server) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".server-*.tmp")
	if err != nil {
		return err
	}
	if IsYAMLPath(path) {
		enc := yaml.NewEncoder(f)
		enc.SetIndent(2)
		err = enc.Encode(c)
		_ = enc.Close()
	} else {
		err = toml.NewEncoder(f).Encode(c)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// LoadAgent reads the agent configuration (TOML or YAML by extension).
func LoadAgent(path string) (Agent, bool, error) {
	cfg := Agent{LogLevel: "info"}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, true, err
	}
	if IsYAMLPath(path) {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, true, err
		}
	} else {
		if _, err := toml.Decode(string(raw), &cfg); err != nil {
			return cfg, true, err
		}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, true, nil
}

// Save writes the agent configuration atomically (TOML or YAML by extension).
func (a *Agent) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".client-*.tmp")
	if err != nil {
		return err
	}
	if IsYAMLPath(path) {
		enc := yaml.NewEncoder(f)
		enc.SetIndent(2)
		err = enc.Encode(a)
		_ = enc.Close()
	} else {
		err = toml.NewEncoder(f).Encode(a)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// EffectiveServers returns all configured server targets, combining the flat
// single-server fields (server_addr / auth_token / proxies) with any [[server]]
// / servers list entries.
func (a Agent) EffectiveServers() []AgentServer {
	var out []AgentServer
	if addr := NormalizeAddr(a.ServerAddr); addr != "" {
		out = append(out, AgentServer{
			Addr:      addr,
			AuthToken: strings.TrimSpace(a.AuthToken),
			Name:      strings.TrimSpace(a.Name),
			Pin:       strings.TrimSpace(a.Pin),
			CA:        a.CA,
			Proxies:   a.Proxies,
		})
	}
	for _, s := range a.Servers {
		cp := s
		cp.Addr = NormalizeAddr(cp.Addr)
		cp.AuthToken = strings.TrimSpace(cp.AuthToken)
		cp.NodeID = strings.TrimSpace(cp.NodeID)
		cp.Token = strings.TrimSpace(cp.Token)
		cp.Name = strings.TrimSpace(cp.Name)
		cp.Pin = strings.TrimSpace(cp.Pin)
		if cp.AuthToken == "" && cp.Token == "" && a.AuthToken != "" {
			cp.AuthToken = strings.TrimSpace(a.AuthToken)
		}
		if cp.Name == "" && a.Name != "" {
			cp.Name = strings.TrimSpace(a.Name)
		}
		out = append(out, cp)
	}
	return out
}

// HashPassword returns a bcrypt hash suitable for config storage.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// IsBcryptHash reports whether s looks like an encoded bcrypt hash.
func IsBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

// ApplyEnv overrides Server fields with NFR_* environment variables when set.
// Precedence: defaults < config file < environment variables < CLI flags.
func (c *Server) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv("NFR_CONTROL_LISTEN")); v != "" {
		c.ControlListen = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_HTTP_LISTEN")); v != "" {
		c.HTTPListen = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_HTTPS_LISTEN")); v != "" {
		c.HTTPSListen = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_DATA_DIR")); v != "" {
		c.DataDir = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_DOMAIN")); v != "" {
		c.Domain = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_PUBLIC_HOST")); v != "" {
		c.PublicHost = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_AUTH_TOKEN")); v != "" {
		c.AuthToken = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_PASSWORD")); v != "" {
		c.Panel.Password = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_PANEL_HTTPS")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.Panel.HTTPS = true
		case "0", "false", "no", "off":
			c.Panel.HTTPS = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("NFR_TLS_CERT")); v != "" {
		c.TLSCert = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_TLS_KEY")); v != "" {
		c.TLSKey = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_ACME_ENABLED")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.ACME.Enabled = true
		case "0", "false", "no", "off":
			c.ACME.Enabled = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("NFR_ACME_EMAIL")); v != "" {
		c.ACME.Email = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_ACME_CA")); v != "" {
		c.ACME.CA = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_PORT_RANGE")); v != "" {
		if lo, hi, ok := parsePortRange(v); ok {
			c.PortRange = [2]int{lo, hi}
		}
	}
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_LEVEL")); v != "" {
		c.LogLevel = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_FILE")); v != "" {
		c.LogFile = v
	}
}

func parsePortRange(raw string) (int, int, bool) {
	sep := "-"
	if strings.Contains(raw, ",") {
		sep = ","
	}
	parts := strings.SplitN(raw, sep, 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lo, hi, true
}

func isOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "none", "false":
		return true
	}
	return false
}

// Validate checks a Server configuration for common mistakes before starting.
func (c Server) Validate() error {
	if isOff(c.ControlListen) {
		return errors.New("control_listen is required")
	}
	if _, _, err := net.SplitHostPort(c.ControlListen); err != nil {
		return fmt.Errorf("invalid control_listen %q: %w", c.ControlListen, err)
	}
	if !isOff(c.HTTPListen) {
		if _, _, err := net.SplitHostPort(c.HTTPListen); err != nil {
			return fmt.Errorf("invalid http_listen %q: %w", c.HTTPListen, err)
		}
	}
	if !isOff(c.HTTPSListen) {
		if _, _, err := net.SplitHostPort(c.HTTPSListen); err != nil {
			return fmt.Errorf("invalid https_listen %q: %w", c.HTTPSListen, err)
		}
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data_dir is required")
	}
	if c.PortRange[0] <= 0 || c.PortRange[1] < c.PortRange[0] || c.PortRange[1] > 65535 {
		return fmt.Errorf("invalid port_range [%d, %d]: must be within 1..65535", c.PortRange[0], c.PortRange[1])
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
	}
	if c.TLSCert != "" {
		if _, err := os.Stat(c.TLSCert); err != nil {
			return fmt.Errorf("tls_cert: %w", err)
		}
		if _, err := os.Stat(c.TLSKey); err != nil {
			return fmt.Errorf("tls_key: %w", err)
		}
	}
	if c.ACME.Enabled && strings.TrimSpace(c.Domain) == "" {
		return errors.New("acme.enabled requires domain to be set")
	}
	return nil
}

// ApplyEnv overrides Agent fields with NFR_* environment variables when set.
func (a *Agent) ApplyEnv() {
	if v := strings.TrimSpace(os.Getenv("NFR_SERVER")); v != "" && len(a.Servers) == 0 && a.ServerAddr == "" {
		a.ServerAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_AUTH_TOKEN")); v != "" {
		a.AuthToken = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_NAME")); v != "" && a.Name == "" {
		a.Name = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_PIN")); v != "" && a.Pin == "" {
		a.Pin = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_CA")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			a.CA = true
		case "0", "false", "no", "off":
			a.CA = false
		}
	}
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_LEVEL")); v != "" {
		a.LogLevel = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_FILE")); v != "" {
		a.LogFile = v
	}
}

// Validate checks an Agent configuration for completeness.
func (a Agent) Validate() error {
	servers := a.EffectiveServers()
	if len(servers) == 0 {
		return errors.New("no server configured; set server_addr + auth_token in client.toml / client.yaml")
	}
	for i, s := range servers {
		if _, _, err := net.SplitHostPort(s.Addr); err != nil {
			return fmt.Errorf("server[%d].addr %q: %w", i, s.Addr, err)
		}
		hasAuthToken := s.AuthToken != ""
		hasNodeCred := s.NodeID != "" && s.Token != ""
		if !hasAuthToken && !hasNodeCred {
			return fmt.Errorf("server[%d]: auth_token (or node_id + token) is required", i)
		}
		if !s.CA && strings.TrimSpace(s.Pin) == "" {
			return fmt.Errorf("server[%d]: pin is required (set server certificate SHA-256 fingerprint or enable ca = true)", i)
		}
		for j, p := range s.Proxies {
			if strings.TrimSpace(p.Name) == "" {
				return fmt.Errorf("server[%d].proxies[%d].name is required", i, j)
			}
			typ := strings.ToLower(strings.TrimSpace(p.Type))
			if typ == "" {
				typ = "tcp"
			}
			if typ != "tcp" && typ != "udp" && typ != "http" {
				return fmt.Errorf("server[%d].proxies[%d].type %q must be tcp, udp or http", i, j, p.Type)
			}
			local := p.EffectiveLocal()
			if _, _, err := net.SplitHostPort(local); err != nil {
				return fmt.Errorf("server[%d].proxies[%d].local %q: %w", i, j, local, err)
			}
			if p.RemotePort < 0 || p.RemotePort > 65535 {
				return fmt.Errorf("server[%d].proxies[%d].remote_port %d out of range", i, j, p.RemotePort)
			}
		}
	}
	return nil
}
