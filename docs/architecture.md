# 架构

## 进程与角色

两个二进制，一个在内网、一个在公网，共用 `internal/` 里的同一套逻辑：

| 程序 | 部署位置 | 启动 | 职责 |
|---|---|---|---|
| `nfrs` | 公网服务器 | `nfrs` | 公网入口（80/443 + 端口池）、控制通道、面板、REST API、`state.json` |
| `nfrc` | 内网机器 | `nfrc` | 主动外连控制通道、按服务端下发的隧道列表拨内网服务 |

`nfrc join <server> <code>` 是一次性动作：拿到加入码 → 换长期令牌 → 写 `agent.toml`。
加入码也可以在创建隧道时绑定到那条待接入的隧道，兑换时设备接入、隧道自动激活。
不加 `--no-start` 时它接着就把连接挂上，等于 `nfrs` 侧再也不需要碰第二条命令。

命令行的分发逻辑集中在 `internal/cli`：`cmd/nfrs` 与 `cmd/nfrc` 只负责设置
`cli.Program` 并交给 `cli.RunServer` / `cli.RunClient`，所以两边的帮助、版本、
配置路径和错误文案不会各写一套。

## 连接建立

```
nfrc ──TCP──▶ nfrs:8443
   TLS 1.3 握手（ALPN nfr/1）
   yamux 会话
   ├─ stream 0：控制流
   │    hello   {node_id, os, arch, version}
   │    challenge {nonce}
   │    auth    {mac = HMAC-SHA256(sha256(token), nonce)}
   │    welcome {node_id, revision}
   │    config  {tunnels…}        ← 之后每次变更都会重发
   │    dial_failed {tunnel, msg} ← 拨不通内网地址时上报，面板据此显示「目标不可达」
   │    ping/pong 每 25s，90s 无帧判定掉线
   └─ stream N：一个公网连接一条流
        2 字节长度 + JSON {tunnel, remote}
        nfrc 拨 local → 双向 copy → 半关闭
```

令牌本身从不上网：server 存 `sha256(token)`，agent 用它算 HMAC 证明持有令牌。
加入码换令牌那一步走的是同一条 TLS 连接上的 `join` 流。

## 数据面

- **HTTP 隧道**有两种入口：分配了公网端口的，server 在那个端口上跑一个
  HTTP 服务，所有请求都送给这条隧道（访客用 `http://服务器IP:端口`）；
  没分配端口的，server 在 80/443 上按 `Host`（TLS 下优先看 SNI）匹配域名。
  两种都重写请求头后写进一条 yamux 流；agent 转发给内网服务，响应原路返回。
  WebSocket/Upgrade 走 `proxyUpgrade`：接管连接后双向裸拷。
- **TCP 隧道**：server 在端口池里 listen，来一个连接开一条流，agent 拨
  `local` 后双向拷贝。
- **半关闭**：HTTP/1.0 和 ssh 依赖 FIN 语义。yamux 的 `Close()` 是半关闭
  （发完数据再 FIN），`*net.TCPConn` 用 `CloseWrite()`，两端各自处理。

每个连接都会计入 metrics（字节、连接数、错误数），只存内存：
5 分钟秒级点 + 24 小时分钟级点。

## 隧道状态怎么来的

服务端不猜。一个状态由四份事实合成：

- `enabled` / `quota_paused`：来自 `state.json`，操作员或配额自动关的。
- 节点在不在：控制通道有没有活跃的 yamux 会话（`s.agents`），不看 `last_seen`。
- `node_missing`：隧道指向的 node_id 已经不在节点表里（换了机器、删了节点）。
- `target_down`：agent 上一条 `dial_failed` 帧，拨号成功一次或隧道被改动就清掉。

合起来只输出一个词（就绪 / 活跃 / 已停用 / 配额停用 / 节点离线 / 需重新选择节点 /
目标不可达），渲染和 4 秒轮询用的是同一个字段，所以页面不会自相矛盾。

内置的「服务端本机」默认不是隧道目标：内网地址属于加入的机器，悄悄在服务器上
拨它会让一条隧道看起来能用却打到别处。需要时可以设置里显式打开。

## 状态

`<data_dir>/state.json`，0600，写入走「临时文件 → fsync → rename → 目录 fsync」。
内容是 tunnels / nodes / joins / leases / remotes / api_keys / settings / 面板密码哈希。
没有数据库：单进程持锁读写，`Update` 失败会整体回滚，不会出现内存和文件不一致。

端口租约也在里面：面板点「获取端口」→ `POST /api/v1/ports/allocate` →
服务端确认「state 里没被占 + OS 层能 bind」→ 10 分钟租约；保存隧道时校验租约，
冲突返回 409 并给出新建议。

## 多实例纳管（联邦）

没有中心节点。每台 server 都是完整独立的，A 通过 HTTPS 调用 B 的
`/api/v1/*` 来读写在 B 上的隧道，界面用 `?server=<id>` 区分。

- 认证：B 上生成的 API key（scope read/write/admin），A 加密存进自己的 state
- 信任：首次连接 TOFU 钉证书指纹；B 用 ACME 时改为系统根证书校验
- 断连：B 不可达时卡片变灰，不影响 A 自己的隧道

所以「多服务器管理」不是把数据集中到一处，而是把多个控制台拼成一个视图。

## 包结构

```
cmd/nfrs             公网服务器的入口（裸命令 = 后台启动）
cmd/nfrc             内网机器的入口（裸命令 = 按 agent.toml 连接）
internal/cli         两个二进制共用的命令行：参数、横幅、后台化、join
internal/config      引导配置（TOML）
internal/store       state.json + 查询
internal/auth        join code、token、HMAC、API key
internal/proto       帧与流首部
internal/transport   TLS 配置、自签证书、指纹
internal/server      控制通道、入口、端口池、API、metrics
internal/agent       外连、配置应用、流量转发
internal/federation 调用其它实例的客户端
internal/panel       服务端渲染面板（go:embed）
internal/metrics     内存统计
```
