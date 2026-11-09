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
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
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
	case "stop":
		cmdStop(os.Args[2:])
	case "agent":
		cmdAgent(os.Args[2:])
	case "join":
		cmdJoin(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "proxy-config":
		cmdProxyConfig(os.Args[2:])
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

  nfr server                 start a control node: it prints the console
                             address, enters the background and returns;
                             add --foreground to keep it attached
  nfr stop                   stop the background control node
  nfr join <server> <code>   join this machine and keep it connected
  nfr agent                  connect with the saved agent.toml
  nfr status                 show the local configuration
  nfr proxy-config <domain>  print a reverse-proxy snippet for a busy 80/443
  nfr version                print the build version

First run on the public server:

  nfr server

It prints the console URL and a one-time password, then keeps running in the
background. Then, inside your network:

  nfr join <server> <code>

Documentation: https://github.com/Solitudeit/nfr
`)
}

// ---------------------------------------------------------------------------
// server

func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	cfgPath := fs.String("c", defaultServerConfig(), "configuration file")
	foreground := fs.Bool("foreground", false, "stay in the foreground (systemd, containers, debugging)")
	child := fs.Bool("daemon-child", false, "internal use: run as the detached background process")
	_ = fs.Parse(args)

	if *child {
		runServer(*cfgPath, true)
		return
	}

	cfg, _, err := config.LoadServer(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	if *foreground {
		runServer(*cfgPath, false)
		return
	}
	if runtime.GOOS == "windows" {
		fmt.Println("  后台运行暂不支持 Windows，本次以前台运行（Ctrl-C 停止）。")
		fmt.Println()
		runServer(*cfgPath, false)
		return
	}
	daemonize(*cfgPath, cfg)
}

// daemonize spawns a detached copy of this process and stays just long enough
// to print the first-run banner (console address + one-time password) before
// returning the terminal to the user.
func daemonize(cfgPath string, cfg config.Server) {
	exe, err := os.Executable()
	if err != nil {
		fatal("locate executable: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fatal("create data directory: %v", err)
	}
	logPath := cfg.LogFile
	if logPath == "" {
		logPath = filepath.Join(cfg.DataDir, "nfr.log")
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fatal("open log file: %v", err)
	}

	firstRun := filepath.Join(cfg.DataDir, "first-run.txt")
	_ = os.Remove(firstRun)

	cmd := exec.Command(exe, "server", "-c", cfgPath, "--daemon-child")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = daemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		fatal("start background process: %v", err)
	}
	lf.Close()
	pid := cmd.Process.Pid

	fmt.Print(`
  正在启动 NFR 控制节点…
`)

	ready, shown := false, false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !shown {
			if txt, rerr := os.ReadFile(firstRun); rerr == nil {
				fmt.Print(string(txt))
				_ = os.Remove(firstRun)
				shown = true
			}
		}
		if alive(cmd.Process) == nil && waitControl(cfg.ControlListen, 250*time.Millisecond) {
			ready = true
			break
		}
		if alive(cmd.Process) != nil {
			fatal("启动失败，请查看日志: %s", logPath)
		}
	}
	// The child writes the banner right before it starts listening; give it a
	// short grace period so the one-time password is never lost.
	for i := 0; ready && !shown && i < 10; i++ {
		time.Sleep(200 * time.Millisecond)
		if txt, rerr := os.ReadFile(firstRun); rerr == nil {
			fmt.Print(string(txt))
			_ = os.Remove(firstRun)
			shown = true
		}
	}
	if !ready {
		fmt.Fprintf(os.Stderr, "nfr: 服务尚未就绪，稍后用 `nfr status` 或查看日志 %s\n", logPath)
		os.Exit(1)
	}
	if !shown {
		fmt.Printf(`
  NFR 控制节点已在后台运行
  面板      %s
`, server.PanelBaseURL(cfg))
	}
	fmt.Printf(`
  已在后台运行  PID %d
  日志          %s
  停止          nfr stop
`, pid, logPath)
}

// alive reports whether the background process is still with us.
func alive(p *os.Process) error {
	return p.Signal(syscall.Signal(0))
}

// waitControl dials the control port until it answers or the budget runs out.
func waitControl(addr string, budget time.Duration) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "8443"
	}
	deadline := time.Now().Add(budget)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 400*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		if budget <= 0 || time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// runServer is the actual control node, either as a foreground process
// (systemd, containers, debugging) or as the detached background child.
func runServer(cfgPath string, child bool) {
	cfg, existed, err := config.LoadServer(cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	if !existed {
		if err := cfg.Save(cfgPath); err != nil {
			fatal("write default config: %v", err)
		}
	}
	lg := log.New(cfg.LogLevel, cfg.LogFile)
	if !existed {
		lg.Info("wrote default configuration", "path", cfgPath)
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
		banner := serverBanner(cfg, generated, srv.Info().Pinned)
		if child {
			// Hand the first-run banner to the parent process, which prints
			// it and returns the terminal.
			_ = os.WriteFile(filepath.Join(cfg.DataDir, "first-run.txt"), []byte(banner), 0o600)
		} else {
			fmt.Print(banner)
		}
	}
	if child {
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "nfr.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		defer func() { _ = os.Remove(filepath.Join(cfg.DataDir, "nfr.pid")) }()
	}
	if !child {
		fmt.Print(`
  正在前台运行，Ctrl-C 停止；退出终端后仍需运行时用:  nfr server
`)
	}
	lg.Info("starting", "control", cfg.ControlListen, "http", cfg.HTTPListen, "https", cfg.HTTPSListen, "data", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fatal("run: %v", err)
	}
}

// serverBanner renders the first-run message: where to sign in, the one-time
// password and what to do next. Nothing else belongs here.
func serverBanner(cfg config.Server, generated, pin string) string {
	cands := server.ConsoleCandidates(cfg)
	var b strings.Builder
	b.WriteString("\n  NFR 控制节点已就绪\n")
	b.WriteString("  ────────────────────\n")
	b.WriteString("  面板      " + cands[0] + "\n")
	for _, c := range cands[1:] {
		if len(cands) <= 3 {
			b.WriteString("            " + c + "\n")
		}
	}
	b.WriteString("  密码      " + generated + "\n")
	b.WriteString("  证书指纹  " + pin + "\n")
	b.WriteString(`
  密码只显示这一次，登录后请在「设置」里修改。
  打不开时，把地址里的 IP 换成服务商控制台分配的公网 IP。
  下一步：登录面板 → 节点 → 生成加入码，在内网机器上运行打印出的命令。
`)
	return b.String()
}

// cmdStop terminates the background control node started by `nfr server`.
func cmdStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	cfgPath := fs.String("c", defaultServerConfig(), "configuration file")
	_ = fs.Parse(args)

	if runtime.GOOS == "windows" {
		fatal("Windows 上请在服务管理器或任务管理器中停止 NFR")
	}
	cfg, _, err := config.LoadServer(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	pidPath := filepath.Join(cfg.DataDir, "nfr.pid")
	b, err := os.ReadFile(pidPath)
	if err != nil {
		fmt.Print(`  没有正在运行的后台实例。
  systemd 部署请用: systemctl stop nfr-server
`)
		return
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		fatal("pid 文件损坏: %s", pidPath)
	}
	// Guard against a recycled PID: only kill processes that look like NFR.
	if data, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); rerr == nil {
		if !strings.Contains(string(data), "nfr") {
			_ = os.Remove(pidPath)
			fmt.Printf("  PID %d 已不是 NFR 进程，已清理 pid 文件。\n", pid)
			return
		}
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		fmt.Printf("  进程 %d 不存在，已清理 pid 文件。\n", pid)
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	for i := 0; i < 40; i++ {
		if err := p.Signal(syscall.Signal(0)); err != nil {
			_ = os.Remove(pidPath)
			fmt.Printf("  已停止（PID %d）\n", pid)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("  进程 %d 仍未退出，可执行: kill %d\n", pid, pid)
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

// cmdProxyConfig prints a reverse-proxy server block for the case where
// 80/443 are already owned by nginx or Caddy, so NFR sits behind it.
func cmdProxyConfig(args []string) {
	fs := flag.NewFlagSet("proxy-config", flag.ExitOnError)
	upstream := fs.String("upstream", "127.0.0.1:8080", "where nfr http_listen points")
	httpsPort := fs.String("https-port", "443", "listen port for the TLS server block")
	_ = fs.Parse(args)

	domain := "app.example.com"
	if fs.NArg() > 0 {
		domain = fs.Arg(0)
	}

	fmt.Printf(`# NFR behind an existing nginx / Caddy
#
# 1. keep your web server on 80/443, and in server.toml use:
#      http_listen  = %q
#      https_listen = "off"
#    WebSocket needs this once in the nginx http{} block:
#      map $http_upgrade $connection_upgrade { default upgrade; '' close; }
#
# 2. paste the block below into the site for %s (or into
#    /etc/nginx/conf.d/nfr.conf).  SSL is terminated by nginx.
# 3. in the NFR console, create an HTTP tunnel whose domain is %s.

server {
    listen 80;
    listen [::]:80;
    server_name %s;

    location / {
        proxy_pass http://%s;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        client_max_body_size 0;
    }
}

# TLS: once nginx holds the certificate for %s, add
#   listen %s ssl http2;
# plus the usual ssl_certificate / ssl_certificate_key lines.
# Wildcard alternative: server_name *.example.com; then every subdomain
# reaches NFR and the console decides which tunnel answers.
`, *upstream, domain, domain, domain, *upstream, domain, *httpsPort)
}

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
