package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/panel"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/version"
)

// ServerUsage is the help text of nfrs.
const serverUsage = `%[1]s — NFR control node

  %[1]s                     start the control node: it prints the console
                            address, enters the background and returns;
                            add --foreground to keep it attached
  %[1]s stop                stop the background control node
  %[1]s status              show the local configuration
  %[1]s proxy-config <dom>  print a reverse-proxy snippet for a busy 80/443
  %[1]s version             print the build version

Flags of the bare command:

  -c <path>      configuration file (default /etc/nfr/server.toml)
  --foreground   stay in the foreground (systemd, containers, debugging)

First run on the public server:

  %[1]s

It prints the console URL and a one-time password, then keeps running in the
background. Then, inside your network:

  nfrc join <server> <code>

Documentation: https://github.com/Solitudeit/nfr
`

// RunServer is the entry point of the nfrs binary.
func RunServer(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			cmdStop(args[1:])
			return
		case "status":
			cmdServerStatus(args[1:])
			return
		case "proxy-config":
			cmdProxyConfig(args[1:])
			return
		case "version", "-v", "--version":
			fmt.Printf("%s %s\n", Program, version.String())
			return
		case "help", "-h", "--help":
			fmt.Printf(serverUsage, Program)
			return
		case "server":
			// Accepted for compatibility with the old single binary and with
			// systemd units written against it.
			args = args[1:]
		default:
			if !strings.HasPrefix(args[0], "-") {
				fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", Program, args[0])
				fmt.Printf(serverUsage, Program)
				os.Exit(2)
			}
		}
	}
	cmdServer(args)
}

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

	cmd := exec.Command(exe, "-c", cfgPath, "--daemon-child")
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
		fmt.Fprintf(os.Stderr, "%s: 服务尚未就绪，稍后用 `%s status` 或查看日志 %s\n", Program, Program, logPath)
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
  停止          %s stop
`, pid, logPath, Program)
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
		fmt.Printf(`
  正在前台运行，Ctrl-C 停止；退出终端后仍需运行时用:  %s
`, Program)
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

// cmdStop terminates the background control node started by a bare nfrs.
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

// cmdServerStatus prints the local server configuration.
func cmdServerStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("c", defaultServerConfig(), "configuration file")
	_ = fs.Parse(args)

	fmt.Println(Program, version.String())
	fmt.Println()
	if cfg, ok, err := config.LoadServer(*cfgPath); err == nil && ok {
		fmt.Printf("config      %s\n", *cfgPath)
		fmt.Printf("control     %s\n", cfg.ControlListen)
		fmt.Printf("http        %s\n", cfg.HTTPListen)
		fmt.Printf("https       %s\n", cfg.HTTPSListen)
		fmt.Printf("data        %s\n", cfg.DataDir)
		if pid, err := readPID(cfg.DataDir); err == nil {
			fmt.Printf("pid         %d\n", pid)
		}
	} else {
		fmt.Printf("config      %s (missing)\n", *cfgPath)
	}
}

func readPID(dataDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "nfr.pid"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

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
