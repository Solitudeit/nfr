# 部署

## 一台公网服务器最省事的做法

要求：Linux amd64/arm64、root、一个公网 IP、防火墙放行下面这些端口。

| 端口 | 用途 | 能否改 |
|---|---|---|
| 8443/tcp | 控制通道 + 面板 + REST API | `control_listen` |
| 80/tcp | HTTP 隧道入口 | `http_listen`，可设 `"off"` |
| 443/tcp | HTTPS 隧道入口 | `https_listen`，可设 `"off"` |
| 20000-29999/tcp | TCP 隧道的公网端口 | `port_range`（面板里也能改） |

```bash
curl -fsSL https://github.com/nfr-tunnel/nfr/releases/latest/download/install.sh | sudo sh
sudo nfr server -c /etc/nfr/server.toml        # 首次运行：生成配置、证书、密码
sudo cp deploy/systemd/nfr-server.service /etc/systemd/system/
sudo systemctl enable --now nfr-server
journalctl -u nfr-server -f
```

第一次的密码只会打印一次；忘了就在面板 Settings 里改，或者删掉
`state.json` 里的 `panel_password` 字段再重启（会重新生成）。

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
面板地址是 `https://IP:8443/_nfr/`，浏览器会警告一次。
`nfr join` 和纳管服务器会自动钉指纹，所以命令行侧不会报错。

**B. Let's Encrypt**
```toml
domain = "example.com"

[acme]
enabled = true
email   = "you@example.com"
```
重启后 NFR 会在需要时自动申请证书，存在 `<data_dir>/acme`。
这要求 443 能从公网访问（HTTP-01 走 80，TLS-ALPN-01 走 443）。
拿到公网证书后，`nfr join` 和纳管都会改用系统根证书校验，不再钉指纹。

**C. 自己的证书**
```toml
tls_cert = "/etc/letsencrypt/live/example.com/fullchain.pem"
tls_key  = "/etc/letsencrypt/live/example.com/privkey.pem"
```
优先级：显式证书文件 > ACME > 自签。

## Docker

```bash
docker run -d --name nfr --restart=always \
  -p 8443:8443 -p 80:80 -p 443:443 -p 20000-29999:20000-29999 \
  -v nfr-data:/var/lib/nfr \
  ghcr.io/nfr-tunnel/nfr:latest server -c /var/lib/nfr/server.toml
docker logs nfr          # 首次启动的密码在这里
```

注意：容器默认以 root 运行，否则绑不了 80/443。想降权就把
`http_listen` / `https_listen` 换成高位端口，再用端口映射或反代转发。

## 和已有的 nginx / caddy 共存

NFR 自己要占 80/443，和 nginx 直接抢端口。三种选择：

1. **推荐**：换一台独立机器/独立 IP 给 NFR；
2. 让 nginx 只监听回环，把 `http_listen = "127.0.0.1:8080"` 给 NFR，再用 iptables
   把 80 转发过去（443 同理，但证书要 NFR 自己处理）；
3. 只用 TCP 隧道：`http_listen = "off"`、`https_listen = "off"`，nginx 继续用 80/443。

## 内网机器

```bash
nfr join example.com ABC123                  # 加入码在面板 Nodes 页生成
sudo cp deploy/systemd/nfr-agent.service /etc/systemd/system/
sudo systemctl enable --now nfr-agent
```

agent 支持同时连多台服务器：多执行几次 `nfr join`（指向不同服务器）即可，
`agent.toml` 里会有多个 `[[server]]` 段。

## 升级

```bash
curl -fsSL https://github.com/nfr-tunnel/nfr/releases/latest/download/install.sh | sudo sh
sudo systemctl restart nfr-server nfr-agent
```

`state.json` 带 schema 版本，小版本升级直接覆盖二进制即可。
升级前备份：`cp /var/lib/nfr/state.json /var/lib/nfr/state.json.bak`。
