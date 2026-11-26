# 部署指南

NFR 服务端（`nfrs`）与客户端（`nfrc`）均原生支持 **TOML**（`.toml`）与 **YAML**（`.yaml` / `.yml`）两种格式配置文件，默认前台运行，开箱适配 `systemd`、Docker / Docker Compose 与多平台二进制直接运行。

---

## 1. 端口规划与防火墙

| 端口 | 协议 | 用途 | 配置项 |
| --- | --- | --- | --- |
| `8443` | TCP | 控制通道 + Web 控制台（`/nfr/`）+ REST API | `control_listen`（必填） |
| `80` | TCP | HTTP 域名分流 / ACME HTTP-01（可选） | `http_listen`，可设 `"off"` |
| `443` | TCP | HTTPS SNI 分流 / ACME TLS-ALPN-01（可选） | `https_listen`，可设 `"off"` |
| `20000-29999` | TCP / UDP | 隧道动态端口池 | `port_range` |

防火墙放行示例：

```bash
# ufw
sudo ufw allow 8443/tcp
sudo ufw allow 80,443/tcp
sudo ufw allow 20000:29999/tcp
sudo ufw allow 20000:29999/udp

# firewalld
sudo firewall-cmd --permanent --add-port=8443/tcp --add-port=80/tcp --add-port=443/tcp \
  --add-port=20000-29999/tcp --add-port=20000-29999/udp
sudo firewall-cmd --reload
```

云服务器请同步在安全组放行上述端口。

---

## 2. 公网服务端部署（`nfrs`）

### 方式 A：Linux 一键安装 + `systemd`

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo NFR_PASSWORD='change-me-now' NFR_AUTH_TOKEN='my-shared-token' sh -s -- --server --systemd
```

- 脚本会自动校验 `SHA256SUMS`，安装 `/usr/local/bin/nfrs`、生成 `/etc/nfr/server.toml`（可加 `--format yaml` 生成 `server.yaml`），安装并启动 `nfr-server.service`。
- `nfr-server.service` 默认执行 `/usr/local/bin/nfrs`，会自动探测 `/etc/nfr/server.toml`、`server.yaml` 或 `server.yml`。
- 未传 `NFR_PASSWORD` 时，首次启动会生成随机密码并打印到日志（同时暂存于 `/var/lib/nfr/initial-password.txt`，首次成功登录后自动删除）。
- 忘记密码时执行：
  ```bash
  sudo nfrs reset-password 'new-strong-password'
  sudo systemctl restart nfr-server
  ```

### 方式 B：手动初始化配置并启动（Linux / macOS / Windows）

```bash
# 生成带中文注释的 TOML 或 YAML 配置模板
nfrs init -c server.toml          # 或 nfrs init -c server.yaml

# 校验配置语法与端口范围
nfrs verify -c server.toml

# 前台运行（推荐交给 systemd / Docker / supervisor 托管）
nfrs -c server.toml

# 如需手动转后台运行
nfrs -d -c server.toml
nfrs stop -c server.toml
```

---

## 3. 内网客户端部署（`nfrc`）

### 方式 A：Linux 一键安装 + 自动生成配置文件 + `systemd`

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo sh -s -- --client \
      --server-addr 203.0.113.10:8443 \
      --token my-shared-token \
      --name nas-home \
      --systemd
```

如偏好 YAML 格式，追加 `--format yaml` 即可生成 `/etc/nfr/client.yaml`。

修改 `/etc/nfr/client.toml` 或 `/etc/nfr/client.yaml` 里的 `proxies` 后，无需重启服务，支持 **`SIGHUP` 热重载**：

```bash
sudo systemctl reload nfr-agent
```

### 方式 B：手动编写配置文件（Windows / macOS / Linux / Android Termux）

可以直接在 Web 面板的 **节点** 页面切换 TOML / YAML 并一键复制或下载，也可以用命令生成模板：

```bash
nfrc init -c client.toml --server 203.0.113.10:8443 --token my-shared-token --name office-pc
# 或 YAML 格式
nfrc init -c client.yaml --server 203.0.113.10:8443 --token my-shared-token --name office-pc

nfrc verify -c client.toml
nfrc -c client.toml
```

**TOML 配置示例（`client.toml`）**：

```toml
server_addr = "203.0.113.10:8443"
auth_token  = "my-shared-token"
name        = "nas-home"
# 必填（与 ca = true 二选一）：从面板复制或由 `nfrc join` 自动写入的服务端证书指纹
pin         = "change-me-to-server-cert-sha256-pin"
# 若服务端使用 ACME / 公共 CA 证书，请删除 pin 并改为：ca = true

# 可选：直接声明隧道；也可以不写，连上后在 Web 面板里可视化创建与管理
[[proxies]]
name        = "ssh"
type        = "tcp"
local       = "127.0.0.1:22"
remote_port = 20022

[[proxies]]
name  = "blog"
type  = "http"
local = "127.0.0.1:8080"
hosts = ["blog.example.com"]
```

**YAML 配置示例（`client.yaml`）**：

```yaml
server_addr: "203.0.113.10:8443"
auth_token: "my-shared-token"
name: "nas-home"
# 必填（与 ca: true 二选一）：服务端自签证书指纹（ACME / 公共 CA 请改设 ca: true）
pin: "change-me-to-server-cert-sha256-pin"

proxies:
  - name: ssh
    type: tcp
    local: "127.0.0.1:22"
    remote_port: 20022
  - name: blog
    type: http
    local: "127.0.0.1:8080"
    hosts:
      - blog.example.com
```

> 同名节点重连会自动复用服务端已有节点，不会重复创建。配置文件声明的 `proxies` 与 Web 面板手动创建的隧道互相隔离：增删配置文件里的 `proxies` 并热重载只会同步配置托管的隧道，不会误删面板里手动创建的隧道。

---

## 4. Docker / Docker Compose

同一镜像 `ghcr.io/solitudeit/nfr:latest` 同时包含 `nfrs` 与 `nfrc`（详见 [deploy/docker/compose.yml](../deploy/docker/compose.yml)）：

```bash
# 服务端
NFR_PASSWORD='change-me-now' NFR_AUTH_TOKEN='my-shared-token' \
  docker compose -f deploy/docker/compose.yml up -d nfrs

# 客户端（零文件环境变量启动；自签证书需传 NFR_PIN，ACME / 公共 CA 传 NFR_CA=true）
NFR_SERVER='203.0.113.10:8443' NFR_AUTH_TOKEN='my-shared-token' NFR_PIN='<server-sha256-pin>' NFR_NAME='homelab' \
  docker compose -f deploy/docker/compose.yml --profile client up -d nfrc
```

推荐 `--network host`：服务端可直接绑定 `port_range` 任意端口，客户端可直接访问宿主机 `127.0.0.1` 服务。

---

## 5. 域名、证书与 Nginx/Caddy 共存

### 面板协议：默认 HTTP，按需开启 HTTPS

Web 控制台默认在 `control_listen`（`:8443`）上使用 **HTTP** 直接提供访问（`http://<服务器IP>:8443/nfr/`），避免浏览器自签证书拦截；客户端隧道控制通道依然全程使用 TLS 1.3。

如果你希望强制面板走 HTTPS，可以在 `server.toml` / `server.yaml` 中自行开启（也可前置 Nginx / Caddy 反代）：

```toml
[panel]
https = true
```

或设置环境变量 `NFR_PANEL_HTTPS=true`。在面板 **设置** 页可直接导出当前生效的 `server.toml` / `server.yaml`，在 **节点** 页可导出任意节点的完整 `client.toml` / `client.yaml`（含该节点全部隧道）。

### 证书优先级：`tls_cert/tls_key` > `acme` > 自签（默认）

- **自动 HTTPS（ACME / Let's Encrypt）**：将 `example.com` 与 `*.example.com` 解析到服务端，放行 80 与 443，在配置中开启：
  ```toml
  domain = "example.com"
  [acme]
  enabled = true
  email   = "ops@example.com"
  ```
  客户端对应设置 `ca = true` 即可改用系统根证书校验。
- **自有证书文件**：在服务端配置中同时填写 `tls_cert` 与 `tls_key`。

### 与已有 Nginx / Caddy 共存

当 80 / 443 已被占用时，让 NFR 的 HTTP 入口只监听回环地址并关闭 `https_listen`：

```toml
control_listen = ":8443"
http_listen    = "127.0.0.1:8080"
https_listen   = "off"
```

执行 `nfrs proxy-config app.example.com` 即可打印完整含 WebSocket 支持的 Nginx 反向代理片段。

---

## 6. 升级与卸载

```bash
# 升级（保留配置与 state.json，已安装 systemd 时自动重启）
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo sh -s -- --server --systemd

# 卸载（移除二进制与 systemd unit，保留 /etc/nfr 与 /var/lib/nfr）
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo sh -s -- --uninstall
```
