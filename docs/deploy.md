# 部署

## 一台公网服务器最省事的做法

要求：Linux amd64/arm64、root、一个公网 IP、防火墙放行下面这些端口。

| 端口 | 用途 | 能否改 |
|---|---|---|
| 8443/tcp | 控制通道 + 面板 + REST API | `control_listen` |
| 80/tcp | 域名模式的 HTTP 入口（可选） | `http_listen`，可设 `"off"` |
| 443/tcp | 域名模式的 HTTPS 入口（可选） | `https_listen`，可设 `"off"` |
| 20000-29999/tcp | HTTP 与 TCP 隧道的公网端口 | `port_range`（面板里也能改） |

从一台干净的机器到能用的面板，一条命令：

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo sh -s -- --systemd
```

它依次做三件事：装 `nfrs`/`nfrc` 到 `/usr/local/bin`；下载
`nfr-server.service` 到 `/etc/systemd/system` 并 `daemon-reload`；`enable --now`
后把 `systemctl status` 打出来。首次启动会自己写 `/etc/nfr/server.toml`、
生成自签证书和数据目录，并把面板地址、一次性密码、证书指纹打进日志
（`sudo journalctl -u nfr-server -n 15` 可再看）。unit 用
`StateDirectory=nfr` / `ConfigurationDirectory=nfr` 让 systemd 先建好
`/var/lib/nfr` 与 `/etc/nfr` 并保持可写，所以全新机器上首次启动就能写出配置。
别把它们改成 `ReadWritePaths=`——目录不存在时那个选项会让 unit 以
`status=226/NAMESPACE` 直接失败。

不想用 systemd，就只装二进制、手动前台起一次（打印完转后台，`nfrs stop` 停）：

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh | sudo sh
sudo mkdir -p /etc/nfr && sudo nfrs -c /etc/nfr/server.toml
```

**两种方式别同时跑**：都监听 `control_listen`（默认 8443），后起的会
`bind: address already in use`。从手动换到 systemd 时先 `sudo nfrs stop`。

面板地址在**每次启动时都会打印**（`journalctl -u nfr-server -n 15` 里能看到，
`nfrs status` 也会列出来）；只有一次性密码是首次启动独有。忘了密码就在面板
「设置」里改，或者删掉 `state.json` 里的 `panel_password` 字段再重启
（会重新生成一个并打进日志）。

建好的隧道默认按 **公网端口** 访问：面板里给隧道分配一个 `20000-29999` 的端口，
访客直接打开 `http://服务器IP:端口`，不需要域名、证书或 nginx。只有想「一条 80/443
挂多个站点」时才需要看后面的「域名与证书」。

### 防火墙

```bash
# ufw
ufw allow 8443/tcp && ufw allow 80,443/tcp && ufw allow 20000:29999/tcp
# 或者 firewalld
firewall-cmd --permanent --add-port=8443/tcp --add-port=80/tcp --add-port=443/tcp \
  --add-port=20000-29999/tcp && firewall-cmd --reload
```

云厂商的安全组要单独放行，尤其是端口段。

## 域名与证书

把 `example.com` 和 `*.example.com` 都解析到这台机器（A / AAAA 都行），
然后二选一：

**A. 自签证书（默认，零配置）**
面板地址是 `https://IP:8443/nfr/`，浏览器会警告一次。
`nfrc join` 和纳管服务器会自动钉指纹，所以命令行侧不会报错。

**B. Let's Encrypt**
```toml
domain = "example.com"

[acme]
enabled = true
email   = "you@example.com"
```
重启后 NFR 会在需要时自动申请证书，存在 `<data_dir>/acme`。
这要求 443 能从公网访问（HTTP-01 走 80，TLS-ALPN-01 走 443）。
拿到公网证书后，`nfrc join` 和纳管都会改用系统根证书校验，不再钉指纹。

**C. 自己的证书**
```toml
tls_cert = "/etc/letsencrypt/live/example.com/fullchain.pem"
tls_key  = "/etc/letsencrypt/live/example.com/privkey.pem"
```
优先级：显式证书文件 > ACME > 自签。

## Docker

```bash
docker run -d --name nfr --restart=always \
  -p 8443:8443 -p 80:80 -p 443:443 \
  -v nfr-data:/var/lib/nfr \
  ghcr.io/solitudeit/nfr
docker logs nfr          # 首次启动的面板地址与密码在这里
```

注意：容器默认以 root 运行，否则绑不了 80/443。想降权就把
`http_listen` / `https_listen` 换成高位端口，再用端口映射或反代转发。

## 和已有的 nginx / Caddy 共存

只有「域名模式」才需要 80/443；端口模式的 HTTP 隧道和 TCP 隧道都不经过 nginx，
直接跳过这一节。nginx 已经占着 80/443 时，改成 **nginx 在前、NFR 在后**：

**1. `server.toml` 让出入口**

```toml
control_listen = ":8443"
http_listen    = "127.0.0.1:8080"
https_listen   = "off"
```

`control_listen` 仍然直接对公网提供面板与 agent 接入；HTTP 入口只监听回环，由 nginx 转发进来。

**2. 生成反代配置**

```bash
nfrs proxy-config app.example.com       # 换成你的域名
```

把打印出来的 `server { … }` 粘到该站点的配置里，或者写到 `/etc/nginx/conf.d/nfr.conf`。片段里已经带好 `Host`、`X-Forwarded-*` 和 WebSocket 的升级头；
其中 `map $http_upgrade $connection_upgrade` 需要放在 nginx 的 `http {}` 里，否则 `nginx -t` 会报未知变量。

**3. 重载并建隧道**

```bash
sudo nginx -t && sudo systemctl reload nginx
```

回到面板建 HTTP 隧道，域名填 `app.example.com`、内网地址填内网机器的 `127.0.0.1:端口`。
TLS 由 nginx 的证书终止，NFR 会从 `X-Forwarded-Proto` 知道访客其实是 HTTPS，再转给后端。

**多域名**：建一个泛解析站点 `server_name *.example.com;` 指向同一个 8080，之后每条 HTTP
隧道填各自域名即可；也可以一个域名一个站点，反代目标都写 `http://127.0.0.1:8080`。

**TCP 隧道**不受这一节影响：它们直接监听 `port_range` 里的端口，不经过 nginx。

## 内网机器

隧道由内网那台机器发起（`nfrc` 拨回来），服务器从不代替它拨「内网地址」——
这是「内网地址永远是客户端视角」这条铁律的落地方式。

```bash
nfrc join example.com ABC123                 # 加入码在面板「节点」页生成
```

地址只写主机名、不写端口时，端口取面板给出的那个（默认 8443）。
如果你是从本机直接连、而服务器只监听内网/回环，就把完整地址和端口写全，例如
`nfrc join 10.0.0.5:8443 ABC123`；`join` 用你输入的主机地址，不用服务器通告的地址。

面板生成的命令取地址的顺序是：设置里的**公网域名** → 设置里的**对外地址** →
探测到的本机网卡地址。三者都没有（例如云主机在内网网卡后面）时，面板会在命令旁
给出警告——先去「设置」填对外地址，再把命令粘到别的机器上。

`join` 默认写完配置就一直连着，适合 `tmux`/`screen` 里直接跑；交给 systemd 时：

```bash
nfrc join --no-start example.com ABC123
sudo cp deploy/systemd/nfr-agent.service /etc/systemd/system/
sudo systemctl enable --now nfr-agent
```

加入码只用来换一次令牌，换完就作废。**之后重启客户端不要再 `join`**（每 `join`
一次都会在服务端新增一个节点），而是用已保存的配置启动：

```bash
nfrc -c /etc/nfr/agent.toml          # 前台（非 root 用户：~/.config/nfr/agent.toml）
sudo systemctl restart nfr-agent     # 已装成服务时
```

这条命令在 `join` 的输出、`--no-start` 的提示和面板「等待接入」卡片上都会给出。
`nfr-agent.service` 的 `ExecStart` 就是它（附带 `-c /etc/nfr/agent.toml`），
所以装成服务之后重启机器会自动恢复。

agent 支持同时连多台服务器：多执行几次 `nfrc join`（指向不同服务器）即可，
`agent.toml` 里会有多个 `[[server]]` 段。

## 升级

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh \
  | sudo sh -s -- --systemd
```

已经装过并正在运行时，`install.sh` 会替你 `systemctl restart`——`enable --now`
对运行中的 unit 不做事，不重启的话面板还是旧进程、旧界面。它不会重打密码，
密码沿用 `state.json` 里那份。

手动装二进制的话，重启要自己做：

```bash
curl -fsSL https://github.com/Solitudeit/nfr/releases/latest/download/install.sh | sudo sh
sudo systemctl restart nfr-server      # 内网机器上：sudo systemctl restart nfr-agent
```

旧的 systemd unit 也要换（`ExecStart` 从 `nfr server` 改成 `nfrs`）。v0.9.1 起
unit 文件直接作为 release 资产发布，按上面的「四条命令」重下一遍即可：

```bash
sudo curl -fsSL -o /etc/systemd/system/nfr-server.service \
  https://github.com/Solitudeit/nfr/releases/latest/download/nfr-server.service
sudo systemctl daemon-reload && sudo systemctl restart nfr-server
```

从 0.8.x 的单二进制 `nfr` 升级上来时，旧命令要换名：

| 旧 | 新 |
| --- | --- |
| `nfr server --foreground -c …` | `nfrs --foreground -c …` |
| `nfr agent -c …` | `nfrc -c …` |
| `nfr join <addr> <code>` | `nfrc join <addr> <code>` |
| `nfr stop` / `nfr status` | `nfrs stop` / `nfrs status` |

`install.sh` 会同时装 `nfrs` 与 `nfrc`；只要一个时用
`NFR_ONLY=server`（或 `client`）。旧的 `nfr` 二进制可以删掉。

`state.json` 带 schema 版本，小版本升级直接覆盖二进制即可。
升级前备份：`cp /var/lib/nfr/state.json /var/lib/nfr/state.json.bak`。
