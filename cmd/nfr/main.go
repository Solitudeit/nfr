// Command nfr is the server, the agent and the join helper in one binary.
package main

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
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/panel"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
	"github.com/solitudeit/nfr/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		cmdServer(os.Args[2:])
	case "agent":
		cmdAgent(os.Args[2:])
	case "join":
		cmdJoin(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("nfr", version.String())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "nfr: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`nfr — self-hosted reverse tunnels

  nfr server                 run a control node (config is created for you)
  nfr join <server> <code>   join this machine and keep it connected
  nfr agent                  connect with the saved agent.toml
  nfr status                 show the local configuration
  nfr version                print the build version

First run on the public server:

  nfr server

It prints the console URL and a one-time password. Then, inside your network:

  nfr join <server> <code>

Documentation: https://github.com/Solitudeit/nfr
`)
}

// ---------------------------------------------------------------------------
// server

func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	cfgPath := fs.String("c", defaultServerConfig(), "configuration file")
	_ = fs.Parse(args)

	cfg, existed, err := config.LoadServer(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	if !existed {
		if err := cfg.Save(*cfgPath); err != nil {
			fatal("write default config: %v", err)
		}
	}
	lg := log.New(cfg.LogLevel, cfg.LogFile)
	if !existed {
		lg.Info("wrote default configuration", "path", *cfgPath)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fatal("create data directory: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		fatal("open state: %v", err)
	}
	srv, generated, err := server.New(server.Options{Config: cfg, Store: st, Logger: lg})
	if err != nil {
		fatal("start node: %v", err)
	}
	srv.SetPanel(panel.New(srv, lg))

	if generated != "" {
		info := srv.Info()
		fmt.Printf(`
  NFR control node is ready
  ─────────────────────────
  Console   %s
  Password  %s
  Cert      %s

  The password is shown once. Change it in Settings after signing in.
  Next: sign in, open 节点, click 生成加入码, then run the printed command
  inside your network.
`, info.PanelURL, generated, info.Pinned)
	}
	lg.Info("starting", "control", cfg.ControlListen, "http", cfg.HTTPListen, "https", cfg.HTTPSListen, "data", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fatal("run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// agent

func cmdAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	cfgPath := fs.String("c", defaultAgentConfig(), "configuration file")
	_ = fs.Parse(args)

	cfg, existed, err := config.LoadAgent(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	if !existed || len(cfg.Servers) == 0 {
		fatal("no server configured\n\n  run: nfr join <server> <code>")
	}
	runAgents(cfg, log.New(cfg.LogLevel, cfg.LogFile))
}

// ---------------------------------------------------------------------------
// join

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
		fatal("usage: nfr join <server> <code>")
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
  Start the agent when you are ready:

    nfr agent -c %s
`, *cfgPath)
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
	lg.Info("agent started", "servers", len(cfg.Servers))
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

// joinOverTLS opens the join stream against the advertised control address.
func certLabel(caMode bool, pin string) string {
	if caMode {
		return "public CA (verified by the system trust store)"
	}
	return transport.FormatPin(pin)
}

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

// resolveControl turns the advertised control address into something dialable
// from this machine.
func resolveControl(serverArg, control string) string {
	host := serverArg
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(control, ":") {
		return host + control
	}
	if h, p, err := net.SplitHostPort(control); err == nil {
		if h == "" || h == "0.0.0.0" || h == "::" {
			return net.JoinHostPort(host, p)
		}
		return control
	}
	return net.JoinHostPort(host, control)
}

// ---------------------------------------------------------------------------
// status

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("c", defaultAgentConfig(), "agent configuration file")
	_ = fs.Parse(args)

	fmt.Println("nfr", version.String())
	fmt.Println()

	if cfg, ok, err := config.LoadAgent(*cfgPath); err == nil && ok {
		fmt.Printf("agent config  %s\n", *cfgPath)
		if len(cfg.Servers) == 0 {
			fmt.Println("  no servers yet, run: nfr join <server> <code>")
		}
		for _, s := range cfg.Servers {
			state := "no token"
			if s.Token != "" {
				state = "token ok"
			}
			fmt.Printf("  %-28s node %-10s %s\n", s.Addr, s.NodeID, state)
		}
	} else {
		fmt.Printf("agent config  %s (missing)\n", *cfgPath)
	}

	sp := defaultServerConfig()
	if cfg, ok, err := config.LoadServer(sp); err == nil && ok {
		fmt.Printf("server config %s\n", sp)
		fmt.Printf("  control %s  http %s  https %s\n", cfg.ControlListen, cfg.HTTPListen, cfg.HTTPSListen)
		fmt.Printf("  data    %s\n", cfg.DataDir)
	}
}

// ---------------------------------------------------------------------------
// helpers

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

func fatal(format string, args ...any) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "nfr:", format)
	} else {
		fmt.Fprintf(os.Stderr, "nfr: "+format+"\n", args...)
	}
	os.Exit(1)
}
