# NFR

**自托管的内网穿透 / 反向隧道工具。一个二进制，自带面板。**

NFR (Not FRP) 是 frp、ngrok 的开源替代品：把内网的服务暴露到公网服务器上，
不需要在客户端写配置文件、不需要手动开端口、不需要再装一个第三方面板。

- 公网机：`nfr server` —— 接管 80/443，跑控制通道、面板和 REST API
- 内网机：`nfr join <server> <code>` —— 一条 6 位加入码完成接入，之后自动重连
- 管理：浏览器打开面板，所有隧道、节点、端口、证书都在里面创建
- 多机：A 机器的面板可以直接纳管 B、C 服务器，在一处管理全部隧道

```
                 公网                       内网
  访客 ──▶ :80/:443 ──┐              ┌── nfr agent ── localhost:8080
  浏览器 ─▶ :8443 ──▶ nfr server ────┤
  仪表盘 ◀─ /_nfr/    │   TLS+yamux  └── nfr agent ── localhost:22
                       └─ 端口池 20000-29999 ──▶ 访客直连
```

## 为什么不用 frp

| | frp | NFR |
|---|---|---|
| 客户端配置 | 手写 `frpc.toml`，每台机器拷一份 | 一条 `nfr join` 命令，配置由服务端下发 |
| 加入方式 | 永久 token 明文写在文件里 | 6 位一次性加入码，5 分钟有效，可吊销 |
| 管理入口 | 无面板，改 TOML 重启 | 内置面板，改完立即生效，不用重启 |
| 多机管理 | 一台一台改配置文件 | 面板纳管多台服务器，卡片式切换 |
| 端口分配 | 自己猜一个没被占的 | 「获取端口」按钮，服务端查占用后租给你 |
| 观测 | 另装 frps dashboard + Prometheus | 面板实时速率 + `/metrics` |

## 快速开始

### 1. 公网服务器

```bash
curl -fsSL https://github.com/nfr-tunnel/nfr/releases/latest/download/install.sh | sh
sudo nfr server -c /etc/nfr/server.toml
```

首次启动会自动生成 `/etc/nfr/server.toml`、自签证书和管理员密码，并把面板地址
和密码打印一次：

```
  NFR control node is ready
  ─────────────────────────
  Console   https://203.0.113.10:8443/_nfr/
  Password  k3m9xw2p
  Cert      3f:0e:...
```

> 域名 + 80/443 的正式部署见 [docs/deploy.md](docs/deploy.md)，
> 想用 Let's Encrypt 就在配置里打开 `[acme] enabled = true`。

### 2. 内网机器

在面板的 **Nodes → Create join code** 生成一个一次性加入码，然后在目标机器上：

```bash
nfr join example.com ABC123
```

它只做三件事：信任证书（记录指纹）、换取长期令牌、写入 `/etc/nfr/agent.toml`。
之后：

```bash
sudo nfr agent -c /etc/nfr/agent.toml      # 或交给 systemd
```

agent 断线会自己重连（1s 起，指数退避到 30s），服务器重启后隧道照样恢复。

### 3. 建隧道

面板里 **Create tunnel**：选服务器 → 选节点 → 选协议：

- **HTTP**：填内网地址和域名（`blog.example.com`），80/443 按 Host/SNI 分流，不占额外端口
- **TCP**：填内网地址，点「获取端口」拿一个公网端口（如 `20612`），访客直接连 `example.com:20612`

保存后 agent 立刻收到新配置，正在传输的连接不会断。

## 自托管面板

同一个进程，`/_nfr/`：

- **Overview**：全部隧道、实时速率、连接数、错误数
- **Tunnels**：按服务器卡片切换，新建 / 编辑 / 启停 / 删除，即时生效
- **Nodes**：在线节点、最后心跳、加入码
- **Servers**：纳管其它 NFR 实例（填地址 + API 密钥即可）
- **Settings**：域名、端口池、证书、管理员密码
- **API keys**：给自动化用的只读 / 读写 / 管理密钥

面板是服务端渲染的 Go template，没有 npm、没有前端构建、没有外部 CDN。

## 多服务器纳管

每台机器都是完整独立的实例；「纳管」只是浏览器里的一个 `?server=` 参数：
A 的面板通过 HTTPS 调用 B 的 REST API，把 B 的服务器卡片合并进同一个界面。
B 不用改配置，也不用信任 A 的数据库——B 的 state 永远在自己手里。

在 B 上：**Settings → 开放 API**，再到 **API keys** 建一个 key。
在 A 上：**Servers → Add server**，填 `https://B:8443` + key。
自签证书第一次会显示指纹，确认后钉住；之后指纹变了会直接拒连并告警。

## 安全模型

- 传输：TCP + TLS 1.3 + [yamux](https://github.com/hashicorp/yamux)，ALPN 区分隧道流量
  和普通 HTTPS；隧道流量也可以走 QUIC（预留，未启用）
- 身份：加入码一次性 + 5 分钟过期 + 同 IP 错 5 次锁 15 分钟；长期令牌只存 SHA-256，
  校验用 HMAC 挑战应答，令牌本身不过网、不落盘明文
- 证书：自签默认 TOFU 钉指纹；配了域名可以走 ACME，对端改用系统根证书校验
- 面板：bcrypt 密码、HttpOnly + SameSite=Strict 会话、CSRF token、登录限流
- API：密钥只存哈希，默认关闭，可按来源 IP/CIDR 限制，scope 分 read/write/admin

## 配置

服务端 `/etc/nfr/server.toml`（首次启动自动生成）：

```toml
control_listen = ":8443"     # 控制通道 + 面板 + API
http_listen    = ":80"       # HTTP 隧道入口
https_listen   = ":443"      # HTTPS 隧道入口（可设成 "off"）
data_dir       = "/var/lib/nfr"
domain         = "example.com"
port_range     = [20000, 29999]
log_level      = "info"

[acme]
enabled = false
email   = "you@example.com"
# ca = "https://acme-staging-v02.api.letsencrypt.org/directory"
```

客户端 `/etc/nfr/agent.toml`（`nfr join` 自动写入，一般不用手改）：

```toml
log_level = "info"

[[server]]
addr    = "example.com:8443"
node_id = "k3m9xw2p"
token   = "nfr_n_..."
pin     = "3f0e..."
```

隧道本身不在配置文件里——它们在面板和 `state.json` 里，这正是和 frp 最大的区别。

## REST API

面板用的就是这一套 API，`Authorization: Bearer nfr_...`：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/info` | 实例信息、在线节点与隧道 |
| GET | `/api/v1/certs` | 证书来源、指纹、到期时间 |
| GET/POST | `/api/v1/tunnels` | 列表 / 创建 |
| GET/PATCH/DELETE | `/api/v1/tunnels/{id}` | 详情 / 修改 / 删除 |
| POST | `/api/v1/ports/allocate` | 申请一个公网端口（10 分钟租约） |
| GET/DELETE | `/api/v1/nodes[/{id}]` | 节点 |
| POST | `/api/v1/nodes/join-codes` | 生成一次性加入码 |
| GET/POST/DELETE | `/api/v1/servers[/{id}]` | 纳管实例 |
| GET/POST/DELETE | `/api/v1/api-keys[/{id}]` | API 密钥 |
| GET/PATCH | `/api/v1/settings` | 域名、端口池、API 开关 |
| GET | `/api/v1/events` | SSE 事件流 |

```bash
curl -H "Authorization: Bearer $NFR_KEY" https://example.com:8443/api/v1/tunnels
```

访问对面实例时加 `?server=<id>`，例如
`GET /api/v1/tunnels?server=k3m9xw2p` 会代理到被纳管的 B。

## 观测

`GET /metrics`（Prometheus 文本，需要 read 权限的 key）：

```
nfr_up 1
nfr_nodes_online 2
nfr_tunnel_bytes_total{id="k3m9",name="blog",direction="out"} 1.2e+06
nfr_tunnel_active{id="k3m9",name="blog"} 3
```

## 构建

```bash
make build            # 本机
make cross            # linux/amd64, linux/arm64, darwin/arm64, windows/amd64
make test             # go test ./...
python3 scripts/e2e.py            # 单实例端到端
python3 scripts/e2e_federation.py # 双实例纳管端到端
```

只依赖标准库 + `yamux`、`toml`、`x/crypto`，没有 CGO。

## 状态与版本

- 状态：`<data_dir>/state.json`（0600，原子写入），统计只在内存里
- 版本：`nfr version`，构建时 `-ldflags "-X .../version.Version=v0.1.0"`
- 兼容性：`state.json` 带 schema 版本，升级不会丢数据

## 路线图

- [x] 单端口多协议入口、面板、端口租约、加入码、多机纳管、Prometheus
- [ ] WebSocket / gRPC 长连接的更多测试用例
- [ ] QUIC 传输（代码里已留接口，默认不启用）
- [ ] UDP 隧道
- [ ] 流量配额与限速

## License

MIT
