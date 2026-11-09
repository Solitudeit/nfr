# NFR

**自托管的内网穿透 / 反向隧道工具。一个二进制，自带面板。**

NFR (Not FRP) 是 frp、ngrok 的开源替代品：把内网的服务暴露到公网服务器上，
不需要在客户端写配置文件、不需要手动开端口、不需要再装一个第三方面板。

- 公网机：`nfr server` —— 跑控制通道、面板和 REST API，HTTP 可以用独立端口直连
- 内网机：`nfr join <server> <code>` —— 一条 6 位加入码完成接入，之后自动重连
- 管理：浏览器打开面板，所有隧道、节点、端口、证书都在里面创建
- 多机：A 机器的面板可以直接纳管 B、C 服务器，在一处管理全部隧道

```
                 公网                       内网
  访客 ──▶ :20612 ────┐              ┌── nfr agent ── localhost:8080
  访客 ──▶ :20613 ────┤              │
  浏览器 ─▶ :8443 ──▶ nfr server ────┤
  面板   ◀─ /nfr/    │   TLS+yamux  └── nfr agent ── localhost:22
                      └─ 端口池 20000-29999 ──▶ 访客直连
  域名访问（可选）：:80/:443 按 Host / SNI 分流
```

## 为什么不用 frp

| | frp | NFR |
|---|---|---|
| 客户端配置 | 手写 `frpc.toml`，每台机器拷一份 | 一条 `nfr join` 命令，配置由服务端下发 |
| 加入方式 | 永久 token 明文写在文件里 | 6 位一次性加入码，5 分钟有效，可吊销 |
| 管理入口 | 无面板，改 TOML 重启 | 内置面板，改完立即生效，不用重启 |
| 多机管理 | 一台一台改配置文件 | 面板纳管多台服务器，卡片式切换 |
| HTTP 访问 | 要绑域名或另配 vhost | 默认 `http://服务器IP:端口`，域名可选 |
| UDP | 支持 | 同样支持，且 TCP/UDP 端口独立可复用同一号码 |
| 端口分配 | 自己猜一个没被占的 | 「获取端口」按钮，服务端查占用后租给你（TCP/UDP 分开租） |
| 观测 | 另装 frps dashboard + Prometheus | 面板实时速率 + `/metrics` |

## 界面

面板是服务端渲染的，没有 npm、没有前端构建、没有 CDN。界面走"工具优先"路线：
浅色侧栏 + 1px 分隔线、内联统计条（不摆卡片）、紧凑表格为主角、8px 圆角、无阴影渐变，
层级靠字重和灰度表达；主色 `#0969da`，语义绿/红/橙只出现在状态上。所有颜色走 CSS
变量，深浅主题只换 token、不改组件结构；文字对比度按 WCAG 2.1 AA 校准。
右上角可切深色并记住选择，手机上是底部标签栏，数据每 4 秒自己刷新。

![概览](docs/screenshots/overview.png)

![深色主题](docs/screenshots/overview-dark.png)

| 新建隧道 | 接入机器 |
|---|---|
| ![新建隧道](docs/screenshots/tunnel-new.png) | ![节点](docs/screenshots/nodes.png) |

<details>
<summary>更多截图：隧道列表 / 服务器纳管 / 设置 / 手机</summary>

> 以下截图为功能示意，配色以概览页为准。

| 选择服务器 | 纳管另一台 |
|---|---|
| ![隧道](docs/screenshots/tunnels.png) | ![服务器](docs/screenshots/servers.png) |

| 设置与证书 | 手机 |
|---|---|
| ![设置](docs/screenshots/settings.png) | ![手机](docs/screenshots/overview-phone.png) |

| 隧道列表 | 登录 |
|---|---|
| ![隧道列表](docs/screenshots/tunnel-list.png) | ![登录](docs/screenshots/login.png) |

</details>

## 快速开始

三条命令，不用先写配置文件。

### 1. 公网服务器

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh | sudo sh
sudo nfr server
```

第一次启动会自动生成 `/etc/nfr/server.toml`、自签证书和管理员密码；启动信息打印完，
NFR 就转入后台运行，终端直接还给你：

```
  NFR 控制节点已就绪
  ────────────────────
  面板      https://203.0.113.10:8443/nfr/
  密码      k3m9xw2p
  证书指纹  3f:0e:...

  密码只显示这一次，登录后请在「设置」里修改。

  已在后台运行  PID 4231
  日志          /var/lib/nfr/nfr.log
  停止          nfr stop
```

要盯着日志前台跑（systemd / 调试），加 `--foreground`。

> 也可以用 Docker 起，同样一条命令：
>
> ```bash
> docker run -d --name nfr --restart=always -p 8443:8443 -p 80:80 -p 443:443 \
>   -v nfr-data:/var/lib/nfr ghcr.io/solitudeit/nfr
> docker logs nfr      # 面板地址与密码
> ```
>
> 要接域名 + 80/443，或让 Let's Encrypt 签证书，改 `/etc/nfr/server.toml` 后重启即可，
> 细节见 [docs/deploy.md](docs/deploy.md)。
>
> 80/443 已经被 nginx / Caddy 占着？把 `http_listen` 改到 `127.0.0.1:8080`、
> `https_listen = "off"`，再跑 `nfr proxy-config app.example.com` 生成反代片段贴进站点配置，
> 详见 [和已有的 nginx / Caddy 共存](docs/deploy.md#和已有的-nginx--caddy-共存)。

### 2. 内网机器（可选）

服务器自己就是一个节点：不接任何 agent 也能在「隧道」页直接给它建隧道，
本地地址填 `127.0.0.1` 即可。要映射局域网里的别的机器，再接 agent：
在面板「节点」页点「生成加入码」，把打印出来的那条命令粘到内网机器：

```bash
nfr join example.com ABC123
```

它记下证书指纹、换取长期令牌、写入 `/etc/nfr/agent.toml`，然后**直接保持连接**。
Ctrl-C 停止；交给 systemd 时先加 `--no-start`，再跑 `nfr agent` 作为服务。
断线会自己重连（1s 起，指数退避到 30s），服务器重启后隧道照样恢复。

### 3. 建隧道

面板「隧道」→ 选服务器 →「新建隧道」：

- **HTTP**：填内网地址，点「获取端口」拿一个公网端口（如 `20612`），访客直接访问
  `http://你的服务器IP:20612`；想按域名分流再填域名，走 80/443，不占额外端口
- **TCP**：填内网地址和一个公网端口，适合 SSH、数据库这类非 HTTP 协议
- **UDP**：同样一个公网端口，原样转发 UDP 数据报——DNS、WireGuard、游戏、
  Syslog 都用它。每个访客地址在服务端固定到一条独立会话，回包原路返回；
  空闲 90 秒自动回收。TCP 和 UDP 端口空间独立，`tcp:53` 和 `udp:53` 可以共存

保存后 agent 立刻收到新配置，正在传输的连接不会断。

**限速与配额**（表单里的「限速与配额」区块）：

- **带宽限速**：单位 KB/s，上下行各自限到这个速度，整条隧道所有连接共享。
  512 KB/s ≈ 4 Mbps。令牌桶在服务端整形，不限速时零开销
- **月流量配额**：单位 MB，按自然月统计、月初自动清零。超出后隧道自动停用
  （列表里标「配额停用」），下月自动恢复——把服务器带宽借给朋友用，
  不用天天盯着账
- 已用流量每 30 秒落盘一次，重启最多丢 30 秒的记账，不会清零

## 本地 30 秒试玩

不需要公网服务器，也不需要 root（用高位端口）：

```bash
cat > /tmp/nfr-demo.toml <<'TOML'
control_listen = "127.0.0.1:18443"
http_listen    = "127.0.0.1:18080"
https_listen   = "off"
data_dir       = "/tmp/nfr-demo"
TOML

go run ./cmd/nfr server -c /tmp/nfr-demo.toml &     # 记下打印出来的密码
# 打开 https://127.0.0.1:18443/nfr/ ，节点页生成加入码 ABC123，然后：
go run ./cmd/nfr join -c /tmp/nfr-agent.toml 127.0.0.1:18443 ABC123 &

python3 -m http.server 8080 &                       # 一个假的内网服务
# 面板里建 HTTP 隧道：内网地址 8080，公网端口填 20612（或点「获取端口」），然后：
curl http://127.0.0.1:20612/
```

## 自托管面板

同一个进程，`/nfr/`：

- **概览**：在线节点、启用隧道、进出流量、实时速率曲线
- **隧道**：第一屏是服务器卡片（每台一张，标明所在国家），点进去再新建 / 编辑 /
  启停 / 删除，改完立即推送
- **节点**：卡片列出每台机器，每张标明所在国家；面板所在的机器自带一张（恒在线、可改名、不可移除），接入的 agent 另有卡片；可生成加入码、看在线状态和最后心跳
- **服务器**：纳管其它 NFR 实例（填地址 + API 密钥即可）
- **设置**：实例名、域名、端口范围、证书来源与到期、面板密码，以及 API 密钥
  （给自动化用的 read / write / admin 密钥，明文只显示一次）

## 多服务器纳管

每台机器都是完整独立的实例；「纳管」只是浏览器里的一个 `?server=` 参数：
A 的面板通过 HTTPS 调用 B 的 REST API，把 B 的服务器卡片合并进同一个界面。
B 不用改配置，也不用信任 A 的数据库——B 的 state 永远在自己手里。

在 B 上：**设置 → 开放 REST API**，然后在同一页的「API 密钥」里建一个 key。
在 A 上：**服务器 → 添加服务器**，填 `https://B:8443` + key。
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
| PATCH | `/api/v1/nodes/{id}` | 节点改名 |
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
python3 scripts/e2e_udp.py        # UDP 隧道端到端
python3 scripts/e2e_quota.py      # 限速与配额端到端
python3 scripts/e2e_federation.py # 双实例纳管端到端
```

只依赖标准库 + `yamux`、`toml`、`x/crypto`，没有 CGO。

## 状态与版本

- 状态：`<data_dir>/state.json`（0600，原子写入），统计只在内存里
- 版本：`nfr version`，构建时 `-ldflags "-X .../version.Version=v0.1.0"`
- 兼容性：`state.json` 带 schema 版本，升级不会丢数据

## 路线图

- [x] 单端口多协议入口、面板、端口租约、加入码、多机纳管、Prometheus
- [x] UDP 隧道（每访客会话 + 空闲回收，TCP/UDP 端口空间独立）
- [x] 带宽限速（每隧道令牌桶，上下行各自封顶）与月流量配额（超额自动停用、跨月恢复）
- [ ] WebSocket / gRPC 长连接的更多测试用例
- [ ] QUIC 传输（代码里已留接口，默认不启用）
- [ ] STCP 类私密隧道（访客侧也需要 agent）
- [ ] 多用户面板（注册、按用户隔离隧道与配额）

## License

MIT
