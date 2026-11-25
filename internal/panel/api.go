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
)

func (p *Panel) buildAPIMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /nfr/api/auth/logout", p.apiAuthLogout)
	mux.HandleFunc("GET /nfr/api/dashboard", p.apiDashboard)
	mux.HandleFunc("GET /nfr/api/events", p.apiEvents)

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
	mux.HandleFunc("GET /nfr/api/downloads/{filename}", p.apiDownloadClient)

	mux.HandleFunc("POST /nfr/api/ports/allocate", p.apiAllocatePort)

	mux.HandleFunc("GET /nfr/api/nodes", p.apiListNodes)
	mux.HandleFunc("PATCH /nfr/api/nodes/{id}", p.apiRenameNode)
	mux.HandleFunc("DELETE /nfr/api/nodes/{id}", p.apiDeleteNode)
	mux.HandleFunc("POST /nfr/api/nodes/join-codes", p.apiCreateJoinCode)

	mux.HandleFunc("GET /nfr/api/configs", p.apiGetConfigs)

	mux.HandleFunc("GET /nfr/api/settings", p.apiGetSettings)
	mux.HandleFunc("PATCH /nfr/api/settings", p.apiPatchSettings)
	mux.HandleFunc("POST /nfr/api/settings/auth-token", p.apiSetAuthToken)
	mux.HandleFunc("POST /nfr/api/settings/api-toggle", p.apiToggleRESTAPI)
	mux.HandleFunc("POST /nfr/api/settings/password", p.apiChangePassword)

	mux.HandleFunc("GET /nfr/api/api-keys", p.apiListKeys)
	mux.HandleFunc("POST /nfr/api/api-keys", p.apiCreateKey)
	mux.HandleFunc("DELETE /nfr/api/api-keys/{id}", p.apiDeleteKey)

	return mux
}

func (p *Panel) apiDashboard(w http.ResponseWriter, r *http.Request) {
	info := p.srv.Info()
	tunnels := p.srv.Tunnels()
	if tunnels == nil {
		tunnels = []api.Tunnel{}
	}
	nodes := p.srv.Nodes()
	if nodes == nil {
		nodes = []api.Node{}
	}
	p.labelNodes(r.Context(), nodes)
	events := p.srv.RecentEvents()
	if events == nil {
		events = []api.Event{}
	}
	set := p.srv.Settings()
	eff := p.srv.EffectiveConfig()

	writeJSON(w, http.StatusOK, map[string]any{
		"info":    info,
		"tunnels": tunnels,
		"nodes":   nodes,
		"events":  events,
		"rates":   p.srv.RecentRates(),
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
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(ev api.Event) bool {
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

	for _, ev := range p.srv.RecentEvents() {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"tunnels": p.srv.Tunnels(),
	})
}

func (p *Panel) apiCreateTunnel(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelRequest
	if !readJSON(w, r, &req) {
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		tun, err := p.srv.CreateStandaloneTunnel(req)
		if err != nil {
			writeSrvErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"tunnel": tun})
		return
	}
	if nodeID == "new" {
		tun, jc, err := p.srv.CreateTunnelAwaiting(req, 30*time.Minute)
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
	tun, err := p.srv.CreateTunnel(req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tunnel": tun})
}

func (p *Panel) apiTunnelConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tun, ok := p.srv.Tunnel(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "隧道不存在"})
		return
	}
	writeJSON(w, http.StatusOK, p.buildTunnelConfigPayload(tun))
}

func (p *Panel) apiKickTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	kicked, err := p.srv.KickTunnel(id)
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
	id := r.PathValue("id")
	var req api.TunnelRequest
	if !readJSON(w, r, &req) {
		return
	}
	tun, err := p.srv.UpdateTunnel(id, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": tun})
}

func (p *Panel) apiDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := p.srv.DeleteTunnel(id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiToggleTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, ok := p.srv.Tunnel(id)
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
	if err := p.srv.SetTunnelEnabled(id, nextEnabled); err != nil {
		writeSrvErr(w, err)
		return
	}
	updated, _ := p.srv.Tunnel(id)
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": updated})
}

func (p *Panel) apiProbeTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := p.srv.ProbeTunnel(r.Context(), id)
	if err != nil && res.Status == "" {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (p *Panel) apiProbeAllTunnels(w http.ResponseWriter, r *http.Request) {
	results := p.srv.ProbeAllTunnels(r.Context())
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
	nodes := p.srv.Nodes()
	p.labelNodes(r.Context(), nodes)
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (p *Panel) apiRenameNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := p.srv.SetNodeName(id, req.Name); err != nil {
		writeSrvErr(w, err)
		return
	}
	nodes := p.srv.Nodes()
	p.labelNodes(r.Context(), nodes)
	for _, n := range nodes {
		if n.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"node": n})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := p.srv.RemoveNode(id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiCreateJoinCode(w http.ResponseWriter, r *http.Request) {
	var req api.JoinCodeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效"})
		return
	}
	resp, err := p.srv.CreateJoinCode(req)
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

func writeSrvErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	low := strings.ToLower(msg)
	status := http.StatusBadRequest
	switch {
	case strings.Contains(low, "not found"), strings.Contains(low, "unknown"):
		status = http.StatusNotFound
	case strings.Contains(low, "already"), strings.Contains(low, "in use"), strings.Contains(low, "conflict"):
		status = http.StatusConflict
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
