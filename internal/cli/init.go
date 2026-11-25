package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
)

func cmdServerInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("c", "server.toml", "output config path (.toml or .yaml)")
	force := fs.Bool("force", false, "overwrite existing file")
	domain := fs.String("domain", "", "optional public domain")
	publicHost := fs.String("public-host", "", "optional public IP or hostname")
	token := fs.String("token", "", "client auth_token (auto-generated if omitted)")
	_ = fs.Parse(args)

	if err := writeServerTemplate(*cfgPath, *domain, *publicHost, *token, *force); err != nil {
		fatal("init: %v", err)
	}
	fmt.Printf("  已生成服务端配置: %s\n  校验配置: %s verify -c %s\n  启动服务: %s -c %s\n",
		*cfgPath, Program, *cfgPath, Program, *cfgPath)
}

func cmdClientInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("c", "client.toml", "output config path (.toml or .yaml)")
	force := fs.Bool("force", false, "overwrite existing file")
	srvAddr := fs.String("server", "203.0.113.10:8443", "server control address")
	token := fs.String("token", "change-me-to-server-auth-token", "shared auth_token from server")
	name := fs.String("name", defaultNodeName(), "node display name")
	_ = fs.Parse(args)

	if err := writeClientTemplate(*cfgPath, *srvAddr, *token, *name, *force); err != nil {
		fatal("init: %v", err)
	}
	fmt.Printf("  已生成客户端配置: %s\n  校验配置: %s verify -c %s\n  启动连接: %s -c %s\n",
		*cfgPath, Program, *cfgPath, Program, *cfgPath)
}

func writeServerTemplate(path, domain, publicHost, token string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
	}
	tok := strings.TrimSpace(token)
	if tok == "" {
		tok = auth.NewSecret("nfr_t_", 18)
	}
	var body string
	if config.IsYAMLPath(path) {
		body = fmt.Sprintf(`# NFR 服务端配置文件（YAML 格式）
# 启动命令：nfrs -c %s

control_listen: ":8443"
http_listen: ":80"
https_listen: ":443"
data_dir: "/var/lib/nfr"
auth_token: %q
domain: %q
public_host: %q
port_range: [20000, 29999]
log_level: "info"

# 可选：首次启动预设面板管理员密码
# panel:
#   password: "change-me-now"

# 可选：自动申请 Let's Encrypt 证书（需要填写 domain 且 80 端口可达）
# acme:
#   enabled: true
#   email: "ops@example.com"
`, filepath.Base(path), tok, domain, publicHost)
	} else {
		body = fmt.Sprintf(`# NFR 服务端配置文件（TOML 格式）
# 启动命令：nfrs -c %s

control_listen = ":8443"
http_listen    = ":80"
https_listen   = ":443"
data_dir       = "/var/lib/nfr"
auth_token     = %q
domain         = %q
public_host    = %q
port_range     = [20000, 29999]
log_level      = "info"

# 可选：首次启动预设面板管理员密码
# [panel]
# password = "change-me-now"

# 可选：自动申请 Let's Encrypt 证书（需要填写 domain 且 80 端口可达）
# [acme]
# enabled = true
# email   = "ops@example.com"
`, filepath.Base(path), tok, domain, publicHost)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func writeClientTemplate(path, serverAddr, token, name string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
	}
	addr := config.NormalizeAddr(serverAddr)
	if addr == "" {
		addr = "203.0.113.10:8443"
	}
	var body string
	if config.IsYAMLPath(path) {
		body = fmt.Sprintf(`# NFR 客户端配置文件（YAML 格式）
# 启动命令：nfrc -c %s

server_addr: %q
auth_token: %q
name: %q
# pin: ""
# ca: false
log_level: "info"

# 可选：直接声明隧道（不写也可在 Web 面板中可视化创建）
# proxies:
#   - name: ssh
#     type: tcp
#     local: "127.0.0.1:22"
#     remote_port: 20022
#   - name: web
#     type: http
#     local: "127.0.0.1:8080"
#     hosts:
#       - app.example.com
`, filepath.Base(path), addr, token, name)
	} else {
		body = fmt.Sprintf(`# NFR 客户端配置文件（TOML 格式）
# 启动命令：nfrc -c %s

server_addr = %q
auth_token  = %q
name        = %q
# pin         = ""
# ca          = false
log_level   = "info"

# 可选：直接声明隧道（不写也可在 Web 面板中可视化创建）
# [[proxies]]
# name        = "ssh"
# type        = "tcp"
# local       = "127.0.0.1:22"
# remote_port = 20022
#
# [[proxies]]
# name  = "web"
# type  = "http"
# local = "127.0.0.1:8080"
# hosts = ["app.example.com"]
`, filepath.Base(path), addr, token, name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}
