package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/transport"
	"github.com/solitudeit/nfr/internal/version"
)

// clientUsage is the help text of nfrc.
const clientUsage = `%[1]s — NFR client (the side that lives inside your network)

  %[1]s join <server> <code>   join this machine and keep it connected
  %[1]s                        connect with the saved agent.toml
  %[1]s status                 show the local configuration
  %[1]s version                print the build version

Join flags:

  -c <path>    configuration file (default /etc/nfr/agent.toml, or
               ~/.config/nfr/agent.toml when not running as root)
  --name <n>   name shown in the console (default: this machine's hostname)
  --no-start   save the configuration but do not start the agent

Typical use, on the machine that runs the service you want to publish:

  nfrc join tunnel.example.com ABC123

It saves the token and keeps the outbound connection open. Tunnels are
created in the console on the server; this side only follows the config.

Documentation: https://github.com/Solitudeit/nfr
`

// RunClient is the entry point of the nfrc binary.
func RunClient(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "join":
			cmdJoin(args[1:])
			return
		case "agent":
			// Accepted for compatibility with the old single binary and with
			// systemd units written against it.
			cmdAgent(args[1:])
			return
		case "status":
			cmdClientStatus(args[1:])
			return
		case "version", "-v", "--version":
			fmt.Printf("%s %s\n", Program, version.String())
			return
		case "help", "-h", "--help":
			fmt.Printf(clientUsage, Program)
			return
		default:
			if !strings.HasPrefix(args[0], "-") {
				fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", Program, args[0])
				fmt.Printf(clientUsage, Program)
				os.Exit(2)
			}
		}
	}
	cmdAgent(args)
}

func cmdAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	cfgPath := fs.String("c", defaultAgentConfig(), "configuration file")
	_ = fs.Parse(args)

	cfg, existed, err := config.LoadAgent(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	if !existed || len(cfg.Servers) == 0 {
		fatal("no server configured\n\n  run: %s join <server> <code>", Program)
	}
	runAgents(cfg, log.New(cfg.LogLevel, cfg.LogFile))
}

type wellKnown struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Control string `json:"control"`
	Pin     string `json:"pin"`
	// Verify is "pin" for self-signed or operator supplied certificates and
	// "ca" when the server issues certificates through ACME.
	Verify string `json:"verify"`
}

func cmdJoin(args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	cfgPath := fs.String("c", defaultAgentConfig(), "configuration file")
	name := fs.String("name", "", "name shown in the console")
	noStart := fs.Bool("no-start", false, "save the configuration but do not start the agent")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("usage: %s join <server> <code>", Program)
	}
	serverArg := fs.Arg(0)
	code := strings.ToUpper(strings.TrimSpace(fs.Arg(1)))
	if *name == "" {
		*name = defaultNodeName()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := serverArg
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")

	wk, observedPin, err := discover(ctx, base)
	if err != nil {
		fatal("cannot reach %s: %v\n\nCheck that the address is right and port 443 (or the given port) is open.", base, err)
	}
	if wk.Pin != "" && observedPin != "" && !samePin(wk.Pin, observedPin) {
		fmt.Fprintf(os.Stderr, "warning: the advertised fingerprint %s does not match the certificate %s\n",
			transport.FormatPin(wk.Pin), transport.FormatPin(observedPin))
	}
	control := resolveControl(serverArg, wk.Control)
	caMode := wk.Verify == "ca"
	if caMode {
		observedPin = ""
	}

	welcome, err := joinOverTLS(ctx, control, observedPin, caMode, code, *name)
	if err != nil {
		fatal("join failed: %v", err)
	}

	cfg, _, err := config.LoadAgent(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	entry := config.AgentServer{
		Addr:   control,
		NodeID: welcome.NodeID,
		Token:  welcome.Token,
		Name:   *name,
		Pin:    observedPin,
		CA:     caMode,
	}
	replaced := false
	for i := range cfg.Servers {
		if cfg.Servers[i].Addr == control {
			cfg.Servers[i] = entry
			replaced = true
		}
	}
	if !replaced {
		cfg.Servers = append(cfg.Servers, entry)
	}
	if err := cfg.Save(*cfgPath); err != nil {
		fatal("save config: %v", err)
	}

	fmt.Printf(`
  Connected "%s" as node %s
  ─────────────────────────────
  Server   %s
  Config   %s
  Cert     %s
`, *name, welcome.NodeID, control, *cfgPath, certLabel(caMode, observedPin))

	if *noStart {
		fmt.Printf(`
  Start the client when you are ready:

    %s -c %s
`, Program, *cfgPath)
		return
	}

	lg := log.New(cfg.LogLevel, cfg.LogFile)
	fmt.Print("\n  Keeping it connected; press Ctrl-C to stop.\n\n")
	runAgents(cfg, lg)
}

// runAgents keeps one connection per configured server until the process is
// asked to stop.
func runAgents(cfg config.Agent, lg *slog.Logger) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, s := range cfg.Servers {
		a := agent.New(agent.Options{
			Addr:   s.Addr,
			Token:  s.Token,
			NodeID: s.NodeID,
			Pin:    s.Pin,
			CA:     s.CA,
			Logger: lg.With("server", s.Addr),
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Run(ctx)
		}()
	}
	lg.Info("client started", "servers", len(cfg.Servers))
	wg.Wait()
}

// discover fetches /.well-known/nfr and records the certificate fingerprint
// actually presented, so the caller can pin the real key.
func discover(ctx context.Context, base string) (wellKnown, string, error) {
	var wk wellKnown
	host, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"))
	if err != nil {
		host = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: transport.WebClientConfig(host, ""),
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/nfr", nil)
	if err != nil {
		return wk, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return wk, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wk, "", fmt.Errorf("server answered %s", resp.Status)
	}
	if resp.TLS == nil {
		return wk, "", errors.New("the endpoint is not using TLS")
	}
	pin := transport.PinOf(*resp.TLS)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return wk, pin, err
	}
	if err := json.Unmarshal(body, &wk); err != nil {
		return wk, pin, fmt.Errorf("unexpected response: %w", err)
	}
	return wk, pin, nil
}

func certLabel(caMode bool, pin string) string {
	if caMode {
		return "public CA (verified by the system trust store)"
	}
	return transport.FormatPin(pin)
}

// joinOverTLS opens the join stream against the advertised control address.
func joinOverTLS(ctx context.Context, addr, pin string, caMode bool, code, name string) (proto.Frame, error) {
	var out proto.Frame
	dialer := &net.Dialer{Timeout: 12 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return out, err
	}
	host, _, _ := net.SplitHostPort(addr)
	tlsCfg := transport.ClientConfig(host, pin)
	if caMode {
		tlsCfg = transport.PublicClientConfig(host)
	}
	tc := tls.Client(raw, tlsCfg)
	hctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		raw.Close()
		return out, err
	}
	sess, err := yamux.Client(tc, yamuxClientConfig())
	if err != nil {
		tc.Close()
		return out, err
	}
	defer sess.Close()
	stream, err := sess.Open()
	if err != nil {
		return out, err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(15 * time.Second))
	if err := proto.WriteFrame(stream, &proto.Frame{
		Type:    proto.TypeJoin,
		Code:    code,
		Node:    name,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Version: version.String(),
	}); err != nil {
		return out, err
	}
	f, err := proto.ReadFrame(stream)
	if err != nil {
		return out, err
	}
	if f.Type == proto.TypeError {
		return out, errors.New(f.Message)
	}
	if f.Type != proto.TypeWelcome || f.Token == "" {
		return out, fmt.Errorf("unexpected reply %q", f.Type)
	}
	return *f, nil
}

// resolveControl combines what the operator typed with what the server
// advertises. The host always comes from the operator — they know how this
// machine reaches the server (LAN name, tunnel, public IP) and the advertised
// one may be a wildcard, a loopback address or a domain that does not resolve
// from here. The port comes from the advertisement when the operator omitted
// it, so `nfrc join nfr.example.com <code>` still lands on the right port.
func resolveControl(serverArg, control string) string {
	typed := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(serverArg, "https://"), "http://"), "/")
	host, port, err := net.SplitHostPort(typed)
	if err != nil {
		host, port = typed, ""
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		if _, p, err := net.SplitHostPort(control); err == nil {
			port = p
		} else if strings.HasPrefix(control, ":") {
			port = strings.TrimPrefix(control, ":")
		}
	}
	if port == "" {
		port = "8443"
	}
	return net.JoinHostPort(host, port)
}

// cmdClientStatus prints the local client configuration.
func cmdClientStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("c", defaultAgentConfig(), "configuration file")
	_ = fs.Parse(args)

	fmt.Println(Program, version.String())
	fmt.Println()
	if cfg, ok, err := config.LoadAgent(*cfgPath); err == nil && ok {
		fmt.Printf("config      %s\n", *cfgPath)
		if len(cfg.Servers) == 0 {
			fmt.Printf("  no servers yet, run: %s join <server> <code>\n", Program)
		}
		for _, s := range cfg.Servers {
			state := "no token"
			if s.Token != "" {
				state = "token ok"
			}
			fmt.Printf("  %-28s node %-10s %s\n", s.Addr, s.NodeID, state)
		}
	} else {
		fmt.Printf("config      %s (missing)\n", *cfgPath)
	}
}
