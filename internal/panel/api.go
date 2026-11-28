package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/store"
)

func (p *Panel) buildAPIMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /nfr/api/auth/logout", p.apiAuthLogout)
	mux.HandleFunc("GET /nfr/api/dashboard", p.apiDashboard)
	mux.HandleFunc("GET /nfr/api/events", p.apiEvents)

	// User profile & avatars (authenticated users)
	mux.HandleFunc("GET /nfr/api/profile", p.apiGetProfile)
	mux.HandleFunc("PATCH /nfr/api/profile", p.apiPatchProfile)
	mux.HandleFunc("POST /nfr/api/profile/auth-token", p.apiRotateProfileAuthToken)
	mux.HandleFunc("POST /nfr/api/profile/avatar", p.apiUploadAvatar)
	mux.HandleFunc("DELETE /nfr/api/profile/avatar", p.apiDeleteAvatar)
	mux.HandleFunc("GET /nfr/api/avatars/{id}", p.apiGetAvatar)

	mux.HandleFunc("GET /nfr/api/tunnels", p.apiListTunnels)
	mux.HandleFunc("POST /nfr/api/tunnels", p.apiCreateTunnel)
	mux.HandleFunc("POST /nfr/api/tunnels/probe-all", p.apiProbeAllTunnels)
	mux.HandleFunc("GET /nfr/api/tunnels/{id}/config", p.apiTunnelConfig)
	mux.HandleFunc("PATCH /nfr/api/tunnels/{id}", p.apiUpdateTunnel)
	mux.HandleFunc("DELETE /nfr/api/tunnels/{id}", p.apiDeleteTunnel)
	mux.HandleFunc("POST /nfr/api/tunnels/{id}/toggle", p.apiToggleTunnel)
	mux.HandleFunc("POST /nfr/api/tunnels/{id}/kick", p.apiKickTunnel)
	mux.HandleFunc("POST /nfr/api/tunnels/{id}/probe", p.apiProbeTunnel)

	mux.HandleFunc("GET /nfr/api/downloads", p.apiListDownloads)

	// Admin-only self-upgrade endpoints
	mux.HandleFunc("GET /nfr/api/update", p.requireAdmin(p.apiGetUpdate))
	mux.HandleFunc("POST /nfr/api/update/check", p.requireAdmin(p.apiCheckUpdate))
	mux.HandleFunc("POST /nfr/api/update/mirrors", p.requireAdmin(p.apiProbeUpdateMirrors))
	mux.HandleFunc("PATCH /nfr/api/update/settings", p.requireAdmin(p.apiPatchUpdateSettings))
	mux.HandleFunc("POST /nfr/api/update/apply", p.requireAdmin(p.apiApplyUpdate))
	mux.HandleFunc("GET /nfr/api/update/progress", p.requireAdmin(p.apiUpgradeProgress))

	mux.HandleFunc("POST /nfr/api/ports/allocate", p.apiAllocatePort)

	mux.HandleFunc("GET /nfr/api/nodes", p.apiListNodes)
	mux.HandleFunc("PATCH /nfr/api/nodes/{id}", p.apiRenameNode)
	mux.HandleFunc("DELETE /nfr/api/nodes/{id}", p.apiDeleteNode)
	mux.HandleFunc("POST /nfr/api/nodes/join-codes", p.apiCreateJoinCode)

	// Admin-only configs, settings, API keys, users, and database setup
	mux.HandleFunc("GET /nfr/api/configs", p.requireAdmin(p.apiGetConfigs))

	mux.HandleFunc("GET /nfr/api/settings", p.requireAdmin(p.apiGetSettings))
	mux.HandleFunc("PATCH /nfr/api/settings", p.requireAdmin(p.apiPatchSettings))
	mux.HandleFunc("POST /nfr/api/settings/auth-token", p.requireAdmin(p.apiSetAuthToken))
	mux.HandleFunc("POST /nfr/api/settings/api-toggle", p.requireAdmin(p.apiToggleRESTAPI))
	mux.HandleFunc("POST /nfr/api/settings/password", p.requireAdmin(p.apiChangePassword))

	mux.HandleFunc("GET /nfr/api/api-keys", p.requireAdmin(p.apiListKeys))
	mux.HandleFunc("POST /nfr/api/api-keys", p.requireAdmin(p.apiCreateKey))
	mux.HandleFunc("DELETE /nfr/api/api-keys/{id}", p.requireAdmin(p.apiDeleteKey))

	mux.HandleFunc("GET /nfr/api/users", p.requireAdmin(p.apiListUsers))
	mux.HandleFunc("POST /nfr/api/users", p.requireAdmin(p.apiCreateUser))
	mux.HandleFunc("GET /nfr/api/users/{id}", p.requireAdmin(p.apiGetUserDetail))
	mux.HandleFunc("PATCH /nfr/api/users/{id}", p.requireAdmin(p.apiUpdateUser))
	mux.HandleFunc("DELETE /nfr/api/users/{id}", p.requireAdmin(p.apiDeleteUser))
	mux.HandleFunc("POST /nfr/api/users/{id}/reset-password", p.requireAdmin(p.apiResetUserPassword))
	mux.HandleFunc("POST /nfr/api/users/{id}/tunnels/{tid}/toggle", p.requireAdmin(p.apiAdminToggleUserTunnel))
	mux.HandleFunc("POST /nfr/api/users/{id}/tunnels/{tid}/kick", p.requireAdmin(p.apiAdminKickUserTunnel))
	mux.HandleFunc("DELETE /nfr/api/users/{id}/tunnels/{tid}", p.requireAdmin(p.apiAdminDeleteUserTunnel))

	mux.HandleFunc("GET /nfr/api/db", p.requireAdmin(p.apiGetDB))
	mux.HandleFunc("POST /nfr/api/db/test", p.requireAdmin(p.apiTestDB))
	mux.HandleFunc("POST /nfr/api/db/setup", p.requireAdmin(p.apiSetupDB))

	return mux
}

func (p *Panel) apiDashboard(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	uid := sess.userID
	isAdmin := sess.role == store.RoleAdmin

	info := p.srv.Info()
	tunnels := p.srv.TunnelsForOwner(uid)
	if tunnels == nil {
		tunnels = []api.Tunnel{}
	}
	nodes := p.srv.NodesForOwner(uid)
	if nodes == nil {
		nodes = []api.Node{}
	}
	onlineNodes := 0
	for _, n := range nodes {
		if n.Online {
			onlineNodes++
		}
	}
	info.Nodes = onlineNodes
	info.Tunnels = len(tunnels)

	events := p.srv.RecentEventsForOwner(uid, isAdmin)
	if events == nil {
		events = []api.Event{}
	}
	set := p.srv.Settings()
	eff := p.srv.EffectiveConfig()
	user, _ := p.srv.UserProfile(uid)

	writeJSON(w, http.StatusOK, map[string]any{
		"info":    info,
		"user":    user,
		"tunnels": tunnels,
		"nodes":   nodes,
		"events":  events,
		"rates":   p.srv.RecentRatesForOwner(uid),
		"settings": map[string]any{
			"domain":      set.Domain,
			"public_host": set.PublicHost,
			"port_range":  set.PortRange,
			"panel_https": eff.Panel.HTTPS,
			"join_addr":   p.srv.JoinAddr(),
		},
	})
}

func (p *Panel) apiEvents(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	uid := sess.userID
	isAdmin := sess.role == store.RoleAdmin

	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	visible := func(ev api.Event) bool {
		return ev.OwnerID == uid || (isAdmin && ev.OwnerID == "")
	}
	send := func(ev api.Event) bool {
		if !visible(ev) {
			return true
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	ch, unsub := p.srv.SubscribeEvents()
	defer unsub()

	for _, ev := range p.srv.RecentEventsForOwner(uid, isAdmin) {
		if !send(ev) {
			return
		}
	}
	_, _ = fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			if !send(ev) {
				return
			}
		}
	}
}

func (p *Panel) apiListTunnels(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	writeJSON(w, http.StatusOK, map[string]any{
		"tunnels": p.srv.TunnelsForOwner(uid),
	})
}

func (p *Panel) apiCreateTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	var req api.TunnelRequest
	if !readJSON(w, r, &req) {
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		tun, err := p.srv.CreateStandaloneTunnelForOwner(uid, req)
		if err != nil {
			writeSrvErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"tunnel": tun})
		return
	}
	if nodeID == "new" {
		tun, jc, err := p.srv.CreateTunnelAwaitingForOwner(uid, req, 30*time.Minute)
		if err != nil {
			writeSrvErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"tunnel":       tun,
			"join_code":    jc.Code,
			"join_command": jc.Command,
			"join":         jc,
		})
		return
	}
	tun, err := p.srv.CreateTunnelForOwner(uid, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tunnel": tun})
}

func (p *Panel) apiTunnelConfig(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	tun, ok := p.srv.TunnelForOwner(uid, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "隧道不存在"})
		return
	}
	writeJSON(w, http.StatusOK, p.buildTunnelConfigPayload(tun))
}

func (p *Panel) apiKickTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	kicked, err := p.srv.KickTunnelForOwner(uid, id)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	msg := "已发送强制下线指令"
	if !kicked {
		msg = "该隧道当前未在线"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"kicked":  kicked,
		"message": msg,
	})
}

func (p *Panel) apiUpdateTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	var req api.TunnelRequest
	if !readJSON(w, r, &req) {
		return
	}
	tun, err := p.srv.UpdateTunnelForOwner(uid, id, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": tun})
}

func (p *Panel) apiDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	if err := p.srv.DeleteTunnelForOwner(uid, id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiToggleTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	cur, ok := p.srv.TunnelForOwner(uid, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tunnel not found"})
		return
	}
	nextEnabled := !cur.Enabled
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效"})
		return
	}
	if body.Enabled != nil {
		nextEnabled = *body.Enabled
	}
	if err := p.srv.SetTunnelEnabledForOwner(uid, id, nextEnabled); err != nil {
		writeSrvErr(w, err)
		return
	}
	updated, _ := p.srv.TunnelForOwner(uid, id)
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": updated})
}

func (p *Panel) apiProbeTunnel(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	res, err := p.srv.ProbeTunnelForOwner(r.Context(), uid, id)
	if err != nil && res.Status == "" {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (p *Panel) apiProbeAllTunnels(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	results := p.srv.ProbeAllTunnelsForOwner(r.Context(), uid)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (p *Panel) apiAllocatePort(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		TunnelID string `json:"tunnel_id"`
		Network  string `json:"network"`
		Type     string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效"})
		return
	}
	netKind := strings.ToLower(strings.TrimSpace(req.Network))
	if netKind == "" {
		netKind = strings.ToLower(strings.TrimSpace(req.Type))
	}
	if netKind != "udp" {
		netKind = "tcp"
	}
	resp, err := p.srv.AllocatePort(req.TunnelID, netKind)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) apiListNodes(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	nodes := p.srv.NodesForOwner(uid)
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (p *Panel) apiRenameNode(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := p.srv.SetNodeNameForOwner(uid, id, req.Name); err != nil {
		writeSrvErr(w, err)
		return
	}
	nodes := p.srv.NodesForOwner(uid)
	for _, n := range nodes {
		if n.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"node": n})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	id := r.PathValue("id")
	if err := p.srv.RemoveNodeForOwner(uid, id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiCreateJoinCode(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	var req api.JoinCodeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效"})
		return
	}
	resp, err := p.srv.CreateJoinCodeForOwner(uid, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (p *Panel) apiGetConfigs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.buildConfigsPayload())
}

func (p *Panel) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	set := p.srv.Settings()
	info := p.srv.Info()
	cert := p.srv.CertSummary()
	eff := p.srv.EffectiveConfig()
	expiry := ""
	if cert.NotAfter != nil {
		expiry = cert.NotAfter.Local().Format("2006-01-02")
	}
	keys := p.srv.APIKeys()
	if keys == nil {
		keys = []api.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":                   info.Name,
		"domain":                 set.Domain,
		"public_host":            set.PublicHost,
		"port_range":             set.PortRange,
		"email":                  set.HTTPSEmail,
		"api_enabled":            set.APIEnabled,
		"auth_token":             p.srv.AuthToken(),
		"auth_token_from_config": p.srv.AuthTokenFromConfig(),
		"panel_https":            eff.Panel.HTTPS,
		"panel_url":              info.PanelURL,
		"control_addr":           info.ControlAddr,
		"pin":                    info.Pinned,
		"cert_mode":              cert.Mode,
		"cert_source":            certSourceLabel(cert.Source),
		"cert_expiry":            expiry,
		"keys":                   keys,
	})
}

func (p *Panel) apiPatchSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Domain     string `json:"domain"`
		PublicHost string `json:"public_host"`
		PortRange  [2]int `json:"port_range"`
		Email      string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	cur := p.srv.Settings()
	if err := p.srv.UpdateSettings(req.Domain, req.PublicHost, cur.APIEnabled, req.PortRange, req.Email); err != nil {
		writeSrvErr(w, err)
		return
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		if err := p.srv.SetInstanceName(name); err != nil {
			writeSrvErr(w, err)
			return
		}
	}
	p.apiGetSettings(w, r)
}

func (p *Panel) apiSetAuthToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthToken string `json:"auth_token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.AuthToken)
	if tok == "" {
		tok = auth.NewSecret("nfr_t_", 18)
	}
	if err := p.srv.SetAuthToken(tok); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"auth_token": p.srv.AuthToken(),
	})
}

func (p *Panel) apiToggleRESTAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := p.srv.ToggleAPI(req.Enabled); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"api_enabled": p.srv.Settings().APIEnabled,
	})
}

func (p *Panel) apiChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.NewPassword) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码长度不能少于 8 位"})
		return
	}
	if !checkPassword(p.srv.PanelPasswordHash(), req.OldPassword) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码不正确"})
		return
	}
	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := p.srv.SetPanelPasswordHash(hash); err != nil {
		writeSrvErr(w, err)
		return
	}
	p.revokeOtherSessions(r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": p.srv.APIKeys()})
}

func (p *Panel) apiCreateKey(w http.ResponseWriter, r *http.Request) {
	var req api.APIKeyRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Scope == "" {
		req.Scope = "read"
	}
	resp, err := p.srv.CreateAPIKey(req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (p *Panel) apiDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := p.srv.DeleteAPIKey(id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiGetUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.srv.UpdateStatus())
}

func (p *Panel) apiCheckUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		Source  string `json:"source"`
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效"})
		return
	}
	status, err := p.srv.CheckForUpdates(r.Context(), req.Source, req.Channel)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "检查更新失败: " + err.Error(),
			"status": status,
		})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (p *Panel) apiProbeUpdateMirrors(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	writeJSON(w, http.StatusOK, p.srv.ProbeUpdateMirrors(r.Context(), req.Force))
}

func (p *Panel) apiPatchUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoCheck *bool  `json:"auto_check"`
		Channel   string `json:"channel"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	cur := p.srv.UpdateStatus()
	autoCheck := cur.AutoCheck
	if req.AutoCheck != nil {
		autoCheck = *req.AutoCheck
	}
	channel := cur.Channel
	if strings.TrimSpace(req.Channel) != "" {
		channel = req.Channel
	}
	if err := p.srv.SetUpdateSettings(autoCheck, channel); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p.srv.UpdateStatus())
}

func (p *Panel) apiApplyUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source        string `json:"source"`
		MirrorPrefix  string `json:"mirror_prefix"`
		TargetVersion string `json:"target_version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	prog, err := p.srv.StartSelfUpgrade(req.Source, req.MirrorPrefix, req.TargetVersion)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, prog)
}

func (p *Panel) apiUpgradeProgress(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.srv.UpgradeProgress())
}

func writeSrvErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	low := strings.ToLower(msg)
	status := http.StatusBadRequest
	switch {
	case strings.Contains(low, "not found"), strings.Contains(low, "unknown"), strings.Contains(msg, "不存在"):
		status = http.StatusNotFound
	case strings.Contains(low, "already"), strings.Contains(low, "in use"), strings.Contains(low, "conflict"), strings.Contains(msg, "已存在"), strings.Contains(msg, "已占用"):
		status = http.StatusConflict
	case strings.Contains(msg, "停用"), strings.Contains(low, "forbidden"):
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func certSourceLabel(source string) string {
	switch source {
	case "self-signed":
		return "自签，本机自动生成"
	case "acme":
		return "ACME（Let's Encrypt 或指定 CA）"
	case "file":
		return "自有证书文件"
	case "":
		return "未知"
	}
	return source
}
