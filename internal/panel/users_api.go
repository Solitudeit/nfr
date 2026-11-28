package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/solitudeit/nfr/internal/api"
)

const maxAvatarBytes = 128 << 10 // 128 KiB hard ceiling for uploaded avatars

// detectAvatarMime validates image bytes strictly by magic header (PNG, JPEG,
// or WebP only). SVG, HTML, scripts, and disguised payloads are always rejected.
func detectAvatarMime(data []byte) (string, bool) {
	if len(data) < 12 {
		return "", false
	}
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "image/png", true
	}
	// JPEG: FF D8 FF
	if data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg", true
	}
	// WebP: "RIFF" + 4 bytes + "WEBP"
	if string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", true
	}
	return "", false
}

func (p *Panel) apiGetProfile(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	user, ok := p.srv.UserProfile(uid)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (p *Panel) apiPatchProfile(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	var req api.ProfileUpdateRequest
	if !readJSON(w, r, &req) {
		return
	}
	user, err := p.srv.UpdateProfile(uid, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	if req.NewPassword != "" {
		p.revokeOtherSessions(r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (p *Panel) apiRotateProfileAuthToken(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	tok, err := p.srv.RotateUserAuthToken(uid)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"auth_token": tok,
	})
}

func (p *Panel) apiUploadAvatar(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	var data []byte
	var err error
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+(32<<10))
		if err = r.ParseMultipartForm(maxAvatarBytes + (32 << 10)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "头像文件过大，最大支持 128 KB"})
			return
		}
		file, _, ferr := r.FormFile("file")
		if ferr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择要上传的头像图片"})
			return
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, maxAvatarBytes+1))
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+1)
		data, err = io.ReadAll(io.LimitReader(r.Body, maxAvatarBytes+1))
	}
	if err != nil || len(data) > maxAvatarBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "头像文件过大，最大支持 128 KB"})
		return
	}
	mimeType, ok := detectAvatarMime(data)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "仅支持 PNG、JPEG 或 WebP 格式的头像图片"})
		return
	}
	ver, err := p.srv.SetUserAvatar(uid, mimeType, data)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"avatar_url": fmt.Sprintf("/nfr/api/avatars/%s?v=%d", uid, ver),
	})
}

func (p *Panel) apiDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	uid := sessionFrom(r).userID
	if err := p.srv.ClearUserAvatar(uid); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiGetAvatar(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	id := r.PathValue("id")
	if sess.role != "admin" && sess.userID != id {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问其他用户头像"})
		return
	}
	mimeType, data, ver, ok := p.srv.GetUserAvatar(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"av-%s-%d"`, id, ver)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// Admin-only user management endpoints

func (p *Panel) apiListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"users": p.srv.ListUsers(),
	})
}

func (p *Panel) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var req api.CreateUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	u, err := p.srv.CreateUser(req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (p *Panel) apiGetUserDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := p.srv.GetUserDetail(id)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (p *Panel) apiUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.UpdateUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	u, err := p.srv.UpdateUserQuotaAndStatus(id, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (p *Panel) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := p.srv.DeleteUser(id); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := p.srv.ResetUserPassword(id, req.Password); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Panel) apiAdminToggleUserTunnel(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	tid := r.PathValue("tid")
	cur, ok := p.srv.TunnelForOwner(uid, tid)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "隧道不存在"})
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
	if err := p.srv.SetTunnelEnabledForOwner(uid, tid, nextEnabled); err != nil {
		writeSrvErr(w, err)
		return
	}
	updated, _ := p.srv.TunnelForOwner(uid, tid)
	writeJSON(w, http.StatusOK, map[string]any{"tunnel": updated})
}

func (p *Panel) apiAdminKickUserTunnel(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	tid := r.PathValue("tid")
	kicked, err := p.srv.KickTunnelForOwner(uid, tid)
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

func (p *Panel) apiAdminDeleteUserTunnel(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	tid := r.PathValue("tid")
	if err := p.srv.DeleteTunnelForOwner(uid, tid); err != nil {
		writeSrvErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
