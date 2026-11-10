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
	return "/etc/nfr/server.toml"
}

func defaultAgentConfig() string {
	if os.Geteuid() == 0 {
		return "/etc/nfr/agent.toml"
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".config", "nfr", "agent.toml")
	}
	return "nfr-agent.toml"
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
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = 256
	return cfg
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
