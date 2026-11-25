// Package cli holds the command line surface shared by nfrs (the public
// control node) and nfrc (the client that lives inside the private network).
// Both binaries are thin wrappers: everything reusable lives here so the two
// entry points never drift apart.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/hashicorp/yamux"

	"github.com/solitudeit/nfr/internal/transport"
	"github.com/solitudeit/nfr/internal/version"
)

// Program is the basename of the running binary, used in messages.
var Program = "nfrs"

// fatal prints a prefixed error and exits.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", Program, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// printVersion writes "<program> <build identity>".
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "%s %s\n", Program, version.String())
}

func defaultServerConfig() string {
	for _, p := range []string{"/etc/nfr/server.toml", "/etc/nfr/server.yaml", "/etc/nfr/server.yml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/etc/nfr/server.toml"
}

func defaultAgentConfig() string {
	var dir string
	if os.Geteuid() == 0 {
		dir = "/etc/nfr"
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		dir = filepath.Join(home, ".config", "nfr")
	} else if u, err := user.Current(); err == nil && u.HomeDir != "" {
		dir = filepath.Join(u.HomeDir, ".config", "nfr")
	}
	if dir != "" {
		for _, name := range []string{"client.toml", "client.yaml", "client.yml", "agent.toml", "agent.yaml", "agent.yml"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return filepath.Join(dir, "client.toml")
	}
	return "client.toml"
}

func defaultNodeName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "node"
}

func samePin(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	}
	return norm(a) == norm(b)
}

func yamuxClientConfig() *yamux.Config {
	return transport.YamuxConfig()
}

// isCommand reports whether arg names a subcommand of this binary.
func isCommand(arg string, names ...string) bool {
	for _, n := range names {
		if arg == n {
			return true
		}
	}
	return false
}
