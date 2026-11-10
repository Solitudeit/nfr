# 隧道优先重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 隧道成为面板唯一入口——建隧道时可选「新设备」，提交后给出绑定该隧道的 `nfrc join` 命令，设备一跑节点与隧道同时生效；表单分级、概览并入隧道页。

**Architecture:** 服务端在一次性加入码上扩展 `TunnelID` 绑定（auth/store 层），`handleJoin` 兑换时在同一 `st.Update` 内创建节点并绑定待接入隧道；面板表单加「新设备」目标与「高级」折叠，创建后重定向回隧道列表由待接入卡片展示命令。客户端与 proto 协议零改动。

**Tech Stack:** Go 1.26（标准库）、服务端渲染模板 + 原生 JS（embed）、Python e2e 脚本。

**Spec:** `docs/superpowers/specs/2026-11-10-tunnel-first-design.md`

## Global Constraints

- Go 1.26；`PATH` 需加 `/usr/local/go/bin`；构建 `go build ./...`，测试 `go test ./internal/...`。
- 无 npm、无前端构建、无 CDN；面板全部服务端渲染 + 原生 JS（`internal/panel/static/app.js`）。
- 客户端 `nfrc join <server> <code>` CLI 与 proto 帧不可改动。
- 加入码一次性、短 TTL；绑定码 TTL 1800s（`CreateJoinCode` 上限 3600 不变）。
- `buildTunnel`（internal/server/service.go:136）节点与名称校验不放宽；待接入创建走内部路径。
- 兑换只允许绑定 `NodeID==""` 的隧道，绝不重绑已接入隧道。
- 现有 e2e（`python3 scripts/e2e.py`，需先 `make build`）必须通过。

## Review Focus

- 待接入隧道在 `tunnelStatus` 必须先于 `NodeMissing` 判断，否则显示「需重新选择节点」→ Task 2 测试 `TestTunnelStatusAwaiting`。
- 兑换已删除隧道的绑定码：节点照常创建、绑定静默丢弃 → Task 3 测试 `TestJoinRedeemsDeletedTunnel`。
- 兑换不得重绑已接入隧道（`t.NodeID!=""` 时按普通 join）→ Task 3 测试 `TestJoinDoesNotRebind`。
- federation 远端表单不得出现「新设备」卡（`sel!="local"` 时隐藏）→ Task 5 验证步骤。
- 命令重新生成必须删除旧码（同一隧道同时至多一个有效绑定码）→ Task 4 测试 `TestRegenerateReplacesBoundCode`。

---

### Task 1: store/auth 层——JoinCode.TunnelID 与 Tunnel.Awaiting 查询

**Files:**
- Modify: `internal/auth/auth.go:99`（JoinCode struct）
- Modify: `internal/store/store.go`（Joins 操作辅助函数）

**Interfaces:**
- Produces: `auth.JoinCode.TunnelID string`（json `tunnel_id,omitempty`）；`store.FindJoinCodeByTunnel(tunnelID string, st *State) (*auth.JoinCode, bool)`（包级函数，返回未使用且未过期的绑定码）；`store.DeleteJoinsByTunnel(st *State, tunnelID string)`。

- [ ] **Step 1: 写失败测试** `internal/store/store_join_test.go`

```go
func TestJoinCodeByTunnel(t *testing.T) {
	st := &State{Joins: []auth.JoinCode{{Code: "ABC234", TunnelID: "t1", ExpiresAt: time.Now().Add(time.Minute)}}}
	if _, ok := FindJoinCodeByTunnel(st, "t1"); !ok { t.Fatal("want bound code found") }
	if _, ok := FindJoinCodeByTunnel(st, "t2"); ok { t.Fatal("want none for other tunnel") }
	DeleteJoinsByTunnel(st, "t1")
	if _, ok := FindJoinCodeByTunnel(st, "t1"); ok { t.Fatal("want deleted") }
}
```

- [ ] **Step 2: 运行确认失败** — `go test ./internal/store/ -run TestJoinCodeByTunnel` 期望编译错误 undefined。
- [ ] **Step 3: 实现** — auth.go JoinCode 增加 `TunnelID string \`json:"tunnel_id,omitempty"\``；store.go 按接口签名实现两个函数（遍历 `st.Joins`，`j.TunnelID==tunnelID && j.UsedAt==nil && j.Valid(now)==nil`）。
- [ ] **Step 4: 测试通过** — `go test ./internal/store/ -run TestJoinCodeByTunnel` PASS。
- [ ] **Step 5: Commit** `feat: join codes can bind to a tunnel`

### Task 2: 服务端——CreateTunnelAwaiting + 状态 awaiting

**Files:**
- Modify: `internal/server/service.go`（CreateTunnel 后新增方法）
- Modify: `internal/server/server.go:596`（tunnelStatus）
- Modify: `internal/api/types.go:26`（api.Tunnel 加 Awaiting 字段）
- Test: `internal/server/server_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `store.FindJoinCodeByTunnel` / `store.DeleteJoinsByTunnel`；现有 `buildTunnel`、`auth.NewJoinCode()`。
- Produces: `func (s *Server) CreateTunnelAwaiting(req api.TunnelRequest, ttl time.Duration) (api.Tunnel, api.JoinCodeResponse, error)`——在同一 `st.Update` 里：`buildTunnel` 前若 `req.NodeID=="new"`，生成自动名称（`fmt.Sprintf("tunnel-%d", port)` 与现有名冲突时追加 4 位随机）并置 `nodeID=""` 跳过节点校验（把节点校验参数化：给 `buildTunnel` 加 `allowAwaiting bool` 参数，`allowAwaiting==true && nodeID==""` 时跳过 NodeByID 检查，普通路径不变）；创建隧道后铸造 `auth.JoinCode{TunnelID: t.ID, NodeName: t.Name, TTL: ttl}`；`reconcile()` 后返回含 `Command: fmt.Sprintf("nfrc join %s %s", host, code)` 的响应。`api.Tunnel` 加 `Awaiting bool \`json:"awaiting,omitempty"\``；`tunnelStatus` 顶部加 `case t.Awaiting: return "等待接入", "warn"`（在 Enabled/NodeMissing 各 case 之前，但 `!t.Enabled` 仍优先）。

- [ ] **Step 1: 写失败测试**

```go
func TestCreateTunnelAwaiting(t *testing.T) {
	// 测试服已存在于该文件（沿用现有 helper）；创建 req{NodeID:"new", Type:"tcp", Local:"127.0.0.1:80"}
	tun, jc, err := srv.CreateTunnelAwaiting(req, 30*time.Minute)
	// 断言: err==nil; tun.NodeID==""; tun.Awaiting==true; tun.Status=="等待接入"; jc.Code 六位; strings.HasPrefix(jc.Command, "nfrc join ")
}
func TestAwaitingPortReachable(t *testing.T) {
	// 断言创建后拨 tun.RemotePort 端口可连接（reconcile 已监听），读出 502 语义可不做
}
```

- [ ] **Step 2: 运行确认失败** — 编译错误 undefined。
- [ ] **Step 3: 实现** — 按接口说明改 `buildTunnel` 签名（现有两个调用点传 `false`）与新增方法；`tunnelStatus` 加 awaiting case。
- [ ] **Step 4: 测试通过** — `go test ./internal/server/ -run 'TestCreateTunnelAwaiting|TestAwaitingPortReachable'` PASS，且 `go test ./internal/server/` 全绿（确认 buildTunnel 改签名未破坏现有测试）。
- [ ] **Step 5: Commit** `feat: awaiting tunnels bind a join code at creation`

### Task 3: handleJoin 兑换绑定

**Files:**
- Modify: `internal/server/control.go:152`（handleJoin）
- Test: `internal/server/server_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 `TunnelID` 字段；store 现有 `TunnelByID`。
- Produces: 兑换语义——同一 `st.Update` 内，`jc.Valid` 通过后：创建节点（现有逻辑）；若 `jc.TunnelID!=""` 且 `st.TunnelByID(jc.TunnelID)` 存在且 `NodeID==""`，置 `t.NodeID=nodeID; t.UpdatedAt=now` 并发布事件 `tunnel-bound`。隧道已删除或已绑定时静默跳过绑定。

- [ ] **Step 1: 写失败测试**

```go
func TestJoinBindsAwaitingTunnel(t *testing.T) {
	// CreateTunnelAwaiting → 用返回的 code 走 handleJoin（参照现有 join 测试的连接 helper）
	// 断言: 新节点存在; tunnel.NodeID==新节点ID; tunnel.Awaiting==false
}
func TestJoinRedeemsDeletedTunnel(t *testing.T) {
	// CreateTunnelAwaiting → DeleteTunnel → join code
	// 断言: join 成功、节点已创建、无 panic
}
func TestJoinDoesNotRebind(t *testing.T) {
	// 手工造 jc{TunnelID: 已有节点隧道} → join
	// 断言: 该隧道 NodeID 不变
}
```

- [ ] **Step 2: 运行确认失败** — `TestJoinBindsAwaitingTunnel` FAIL（NodeID 仍为空）。
- [ ] **Step 3: 实现** — handleJoin 的 Update 闭包内按接口说明插入绑定段。
- [ ] **Step 4: 测试通过** — 三个新测试 PASS，`go test ./internal/server/` 全绿。
- [ ] **Step 5: Commit** `feat: redeeming a bound join code activates its tunnel`

### Task 4: 吊销与重新生成

**Files:**
- Modify: `internal/server/service.go`（DeleteTunnel、新增 RegenerateTunnelCode）
- Test: `internal/server/server_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 `DeleteJoinsByTunnel`、`FindJoinCodeByTunnel`。
- Produces: `DeleteTunnel` 在删除循环内同时 `store.DeleteJoinsByTunnel(st, id)`；`func (s *Server) RegenerateTunnelCode(id string) (api.JoinCodeResponse, error)`——查隧道存在且 `NodeID==""`，同一 Update 内删旧绑定码、铸新码，返回响应（含 Command）。

- [ ] **Step 1: 写失败测试**

```go
func TestDeleteTunnelRevokesCode(t *testing.T) {
	// CreateTunnelAwaiting → DeleteTunnel → 旧 code join 必须失败("invalid join code")
}
func TestRegenerateReplacesBoundCode(t *testing.T) {
	// CreateTunnelAwaiting(code A) → RegenerateTunnelCode(code B) → A join 失败、B join 成功
}
```

- [ ] **Step 2: 运行确认失败**。
- [ ] **Step 3: 实现**。
- [ ] **Step 4: 测试通过** — 两测试 PASS，`go test ./internal/server/` 全绿。
- [ ] **Step 5: Commit** `feat: revoke bound codes on tunnel delete, regenerate on demand`

### Task 5: 面板——表单分级 + 新设备 + 隧道页命令展示

**Files:**
- Modify: `internal/panel/templates/tunnel_form.html`（必填区、新设备卡、高级折叠、删 combobox/stepper 按钮逻辑保留）
- Modify: `internal/panel/templates/tunnels.html`（待接入卡片内联命令区）
- Modify: `internal/panel/handlers.go:336`（tunnelSave 分流 CreateTunnelAwaiting；tunnels handler 填充待接入卡的 code/command 数据）
- Modify: `internal/panel/static/app.js`（倒计时、复制已有 data-copy 逻辑复用）

**Interfaces:**
- Consumes: Task 2 `CreateTunnelAwaiting`、`Awaiting` 字段；Task 4 `RegenerateTunnelCode`。
- Produces: 表单行为——`nodepick` 末尾追加 `<button type="button" class="np-card np-add" data-new-device>＋ 新设备</button>`（点选后 `node_id` 值为 `new`，与现有选卡 JS 同一 selectNode 路径）；名称、远程端口、域名、限速、配额、启用开关移入 `<details class="adv">`（名称 placeholder「留空自动生成」）；删除 `SUGGEST` map 与 combobox 函数（tunnel_form.html:234-288），地址/端口变普通输入；`grab()` 保留但仅在高级区展开且端口为空时自动调用，隐藏手动按钮。tunnelSave：`sel=="local" && id=="" && node_id=="new"` 时走 `CreateTunnelAwaiting(req, 30*time.Minute)`，成功后 302 回 `/nfr/tunnels?server=local#t-<id>`；远程服务器（`sel!="local"`）表单不渲染新设备卡。tunnels 页：`t.Awaiting` 的卡片渲染命令块（`class="join-code"` 保留供 e2e 抓取）+ `data-expires` 倒计时 + 重新生成按钮（POST `/nfr/tunnels/regen-code`，新路由调 RegenerateTunnelCode 后回同页）。

- [ ] **Step 1: 写失败测试** `internal/panel/panel_test.go` 追加：渲染 tunnel_form 含 `data-new-device` 与 `<details`；POST 建待接入隧道后 GET tunnels 含 `class="join-code"` 与 `等待接入`。
- [ ] **Step 2: 运行确认失败**。
- [ ] **Step 3: 实现** 按接口说明改三文件。
- [ ] **Step 4: 测试通过** — `go test ./internal/panel/` PASS。
- [ ] **Step 5: 浏览器冒烟（手动）** — `make build && ./bin/nfrs --foreground -c /tmp/nfr-smoke.toml`，登录面板走一遍新设备建隧道、复制命令、重新生成、删除吊销。
- [ ] **Step 6: Commit** `feat: tunnel-first panel form with new-device flow`

### Task 6: 概览合并 + 导航收敛

**Files:**
- Modify: `internal/panel/templates/tunnels.html`（顶部 stats 块，数据取自现有 handler 字段）
- Modify: `internal/panel/handlers.go`（tunnels handler 填充 stats；删除 overview 路由改 302；Nav 值映射）
- Modify: `internal/panel/templates/layout.html:66-72`（nav 4 项）
- Delete: `internal/panel/templates/overview.html`
- Modify: `internal/panel/handlers.go` overview handler → `http.Redirect("/nfr/tunnels?server=local", 303)`

**Interfaces:**
- Consumes: 现有 stats 渲染结构（overview.html:11-35 的四个 stat）。
- Produces: 导航 4 项：隧道/节点/服务器/设置；`/nfr/overview` 303 到 `/nfr/tunnels?server=local`；tunnels handler 在 `IsLocal` 分支填充 `OnlineNodes`（已有）与总流量（从 `p.srv.Metrics()` 聚合，若无现成聚合则遍历 Tunnels 的 In/Out 求和）。

- [ ] **Step 1: 写失败测试** — panel_test：GET `/nfr/overview` 断言 303；GET `/nfr/tunnels?server=local` 含 `data-stat="nodes-online"` 且不含 `nav` 中的「概览」链接。
- [ ] **Step 2: 运行确认失败**。
- [ ] **Step 3: 实现**。
- [ ] **Step 4: 测试通过** + `go test ./internal/...` 全绿。
- [ ] **Step 5: Commit** `feat: merge overview into tunnels page, nav drops to four`

### Task 7: e2e 更新与全量验证

**Files:**
- Modify: `scripts/e2e.py:200`（「概览」→「隧道」）
- Modify: `scripts/e2e.py`（新用例函数）
- Modify: `docs/`（README 界面段落与 architecture.md 若提及五页导航/概览需同步）

**Interfaces:**
- Consumes: 全部前序任务。

- [ ] **Step 1: e2e 断言改「隧道」**，运行 `make build && python3 scripts/e2e.py` 通过。
- [ ] **Step 2: 新用例**——登录后 POST `/nfr/tunnels/toggle` 之外新建：POST `/nfr/tunnels` 表单（node_id=new）→ 从 `/nfr/tunnels?server=local` 抓 `class="join-code">([0-9A-Z]+)<` 与命令 → `nfrc join` → 断言隧道列表该卡片不再 `awaiting` → 对公网端口跑现有 TCP 流量 helper 断言通。
- [ ] **Step 3: 运行 e2e 全绿** + `go vet ./...` + `gofmt -l .` 空。
- [ ] **Step 4: 更新文档**（README 流程图与「为什么不用 frp」表格中「加入方式」行同步新流程；architecture.md 数据流加一句绑定码）。
- [ ] **Step 5: Commit** `test: e2e covers tunnel-first join flow`

## Self-Review 结论

- 覆盖检查：spec 目标 1→Task 2/3/5；2→Task 6（单服务器 302 已存在，验证并入 Task 5 冒烟）；3→Task 5；4→Task 6；「客户端零改动」→全计划无 cli/proto 改动。spec「非目标」均已排除。
- 类型一致：`CreateTunnelAwaiting(req api.TunnelRequest, ttl time.Duration) (api.Tunnel, api.JoinCodeResponse, error)` 与 Task 5 调用一致；`FindJoinCodeByTunnel(st *State, tunnelID string)` Task 3/4 使用同一签名。
- Review Focus 五条各有归属测试。
- 比例：7 任务与 spec 篇幅相当。
