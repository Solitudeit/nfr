# 架构说明

## 1. 双进程模型

NFR 由两个纯 Go 静态二进制组成（零 CGO、零外部数据库、零前端构建工具），共用 `internal/` 下的核心包：

| 二进制 | 运行位置 | 职责 |
| --- | --- | --- |
| `nfrs` | 公网服务器 | 终止公网流量（TCP / UDP 端口池 + HTTP/HTTPS 域名入口）、控制通道、内置 Web 控制台（`/nfr/`）、REST API、Prometheus `/metrics`、状态持久化 |
| `nfrc` | 内网机器 | 按 `client.toml` / `client.yaml` 主动建立 TLS 1.3 + yamux 长连接、上报声明式 `proxies`、接收下发路由并转发至本地服务 |

两者的命令行入口统一收拢在 `internal/cli/`，均默认**前台运行**（适配 systemd 与容器），服务端另支持 `-d` / `--daemon` 显式后台化；客户端支持 Unix `SIGHUP`（`systemctl reload nfr-agent`）无中断热重载配置。

---

## 2. 控制通道与认证协议

```
nfrc ──TCP──▶ nfrs:8443
   TLS 1.3 握手（ALPN: nfr/1）
   yamux 多路复用会话（2 MiB 窗口 + TCP KeepAlive 30s + 25s 双向心跳）
   ├─ stream 0：控制流（4 字节长度前缀 JSON 帧）
   │    hello        {node, node_id, os, arch, ver, proxies, sync_proxies}
   │    challenge    {nonce (32B)}
   │    auth         {mac = HMAC-SHA256(sha256(secret), nonce)}
   │    welcome      {node_id, rev}
   │    config       {tunnels…}        ← 每次配置变更全量下发
   │    dial_failed  {code, msg}       ← 内网拨号失败实时上报
   │    ping / pong                    ← 25s 主动保活，90s 读超时断链重连
   └─ stream N：数据流（每个访客连接/UDP 会话一条 yamux 流）
        2 字节长度前缀 + JSON {tunnel, remote}
        双向转发（支持半关闭 CloseWrite 与令牌桶限速）
```

认证采用 **挑战-响应 HMAC-SHA256**，密钥明文从不在线路上传输：
- **配置文件共享密钥模式（主推）**：客户端配置 `server_addr` + `auth_token` + `name` 直连；服务端校验通过后按 `name` 幂等复用或创建节点，并同步客户端声明的 `proxies`（标记 `managed_by_config = true`，与面板手动创建的隧道互不干扰）。
- **一次性加入码模式（兼容保留）**：通过 `join` 流兑换单节点长期令牌。

---

## 3. 数据面实现

- **TCP 隧道**：服务端在 `port_range` 监听公网 TCP 端口，每个入站连接通过 `openStream` 在目标节点会话上打开一条 yamux stream，双向零额外封装拷贝并支持 TCP 半关闭（`CloseWrite`）。
- **HTTP / HTTPS 隧道**：支持独立公网端口访问或 80 / 443 按 `Host` / TLS SNI 分流。每条隧道复用独立的 `http.Transport` 连接池与标准 `httputil.ReverseProxy`，自动补齐 `X-Forwarded-*`，原生支持 Chunked 流式传输与 WebSocket（`101 Switching Protocols`），通过 `httpStreamConn` 在连接层统一完成字节计数、限速与目标恢复检测。
- **UDP 隧道**：公网单 UDP Socket 按「隧道 + 访客地址」映射到独立的 yamux stream，数据报以 2 字节大端长度定界。每个会话配备独立的有界发送队列（64 包）+ 专用写协程（5 秒写超时）+ `sync.Pool` 缓冲池，避免单条拥塞流阻塞同端口其他访客；每隧道设活跃会话上限（默认 2048，超限时淘汰最久未活跃会话）以抵御伪造源端口洪泛。

---

## 4. 状态持久化与安全

- **引导配置**：`internal/config` 原生支持 `.toml` 与 `.yaml` / `.yml` 自动识别，优先级为 `默认值 < 配置文件 < NFR_* 环境变量 < CLI 参数`。
- **运行时状态**：持久化在 `<data_dir>/state.json`（权限 `0600`），写入采用「临时文件 → `fsync` → 原子 `rename` → 目录 `fsync`」，失败时通过结构化深拷贝 `cloneState` 完整回滚。
- **敏感凭据加密**：共享 `auth_token` 与多机纳管的远端 `api_key` 使用 `<data_dir>/secret.key`（`0600`）以 **AES-256-GCM** 加密落盘；管理员密码使用 bcrypt 存储；API 密钥仅存 SHA-256 哈希。
- **实时活动事件**：内存环形缓冲区保留最近 100 条运行事件（节点上下线、隧道变更、目标不可达、配额触发等），同时服务于 REST API `/api/v1/events` 与 Web 面板实时活动抽屉（`/nfr/api/events`）。

---

## 5. 目录结构

```
cmd/nfrs              服务端入口
cmd/nfrc              客户端入口
configs/              TOML 与 YAML 服务端/客户端配置模板
deploy/               systemd unit 与 Docker Compose 模板
internal/cli          统一 CLI：启动、init、verify、reset-password、SIGHUP 重载、后台化
internal/config       TOML / YAML 双格式配置解析、校验与环境变量覆盖
internal/store        state.json 原子存储、AES-256-GCM 凭据加密与查询
internal/auth         HMAC-SHA256、bcrypt、API Key、Join Code
internal/proto        控制帧与数据流首部编解码
internal/transport    TLS 1.3、自签证书/TOFU、公共 CA、yamux 参数
internal/server       控制通道、HTTP/TCP/UDP 数据面、限速、配额、REST API、Prometheus
internal/agent        客户端外连、配置热应用、TCP/UDP 本地转发
internal/federation   多服务器纳管 HTTP 客户端
internal/panel        go:embed 服务端渲染 Web 控制台（零外部前端依赖）
internal/metrics      内存流量计数与瞬时速率采样
scripts/              install.sh、多平台打包 package.py、4 套 E2E 与截图脚本
```
