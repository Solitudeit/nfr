// Package config reads and writes the small bootstrap files that tell a node
// where to listen and how to reach its peers. Everything that changes at
// runtime lives in the state store, not here.
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
)

// Server is the bootstrap configuration of a control node.
type Server struct {
	ControlListen string `toml:"control_listen"`
	HTTPListen    string `toml:"http_listen"`
	HTTPSListen   string `toml:"https_listen"`
	DataDir       string `toml:"data_dir"`
	Domain        string `toml:"domain"`
	PublicHost    string `toml:"public_host,omitempty"`
	// TLSCert and TLSKey point at a PEM certificate chain and key. When both
	// are set they replace the self-signed certificate generated on first run.
	TLSCert   string `toml:"tls_cert"`
	TLSKey    string `toml:"tls_key"`
	PortRange [2]int `toml:"port_range"`
	Panel     Panel  `toml:"panel"`
	ACME      ACME   `toml:"acme"`
	LogLevel  string `toml:"log_level"`
	LogFile   string `toml:"log_file"`
}

// Panel holds the built-in web console settings.
type Panel struct {
	// Password is a bcrypt hash. Empty means "generate on first run",
	// in which case the plaintext is printed once to the console.
	Password string `toml:"password"`
}

// ACME configures automatic certificate issuance for the HTTPS ingress.
type ACME struct {
	Enabled bool   `toml:"enabled"`
	Email   string `toml:"email"`
	// CA overrides the ACME directory URL, e.g. the Let's Encrypt staging URL.
	CA string `toml:"ca"`
	// Cache is where account and certificate material is kept. It defaults to
	// <data_dir>/acme.
	Cache string `toml:"cache"`
}

// Agent is the bootstrap configuration of an edge client.
type Agent struct {
	Servers  []AgentServer `toml:"server"`
	LogLevel string        `toml:"log_level"`
	LogFile  string        `toml:"log_file"`
}

// AgentServer is one control node the agent connects to.
type AgentServer struct {
	Addr   string `toml:"addr"`
	NodeID string `toml:"node_id"`
	Token  string `toml:"token"`
	Name   string `toml:"name"`
	// Pin is the lowercase hex sha256 fingerprint of the server certificate,
	// learned on first contact (trust on first use).
	Pin string `toml:"pin"`
	// CA verifies the server against the system trust store instead of a
	// pinned fingerprint. It is used when the peer serves a publicly trusted
	// certificate (ACME), where the fingerprint changes on renewal.
	CA bool `toml:"ca"`
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

// LoadServer reads path. The boolean reports whether the file existed.
func LoadServer(path string) (Server, bool, error) {
	cfg := DefaultServer()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, true, err
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

// Save writes the configuration, creating parent directories as needed.
func (c *Server) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".server-*.toml")
	if err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(c); err != nil {
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

// LoadAgent reads the agent configuration.
func LoadAgent(path string) (Agent, bool, error) {
	cfg := Agent{LogLevel: "info"}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, true, err
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, true, nil
}

// Save writes the agent configuration atomically.
func (a *Agent) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".client-*.toml")
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(a); err != nil {
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
// Precedence: defaults < TOML file < environment variables < CLI flags.
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
	if v := strings.TrimSpace(os.Getenv("NFR_PASSWORD")); v != "" {
		c.Panel.Password = v
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
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_LEVEL")); v != "" {
		a.LogLevel = v
	}
	if v := strings.TrimSpace(os.Getenv("NFR_LOG_FILE")); v != "" {
		a.LogFile = v
	}
}

// Validate checks an Agent configuration for completeness.
func (a Agent) Validate() error {
	if len(a.Servers) == 0 {
		return errors.New("no [[server]] configured; run `nfrc join <server> <code>`")
	}
	for i, s := range a.Servers {
		if _, _, err := net.SplitHostPort(s.Addr); err != nil {
			return fmt.Errorf("server[%d].addr %q: %w", i, s.Addr, err)
		}
		if strings.TrimSpace(s.NodeID) == "" {
			return fmt.Errorf("server[%d].node_id is required", i)
		}
		if strings.TrimSpace(s.Token) == "" {
			return fmt.Errorf("server[%d].token is required", i)
		}
	}
	return nil
}
