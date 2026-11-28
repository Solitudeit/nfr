// Package panel serves the single-instance Vue 3 + Naive UI web console and
// its JSON API under /nfr/. Compiled frontend assets live in dist/ and are
// embedded into the nfrs binary so deployment stays a single self-contained
// file with zero external CDN requests.
package panel

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/version"
)

//go:embed dist/*
var embeddedDist embed.FS

const (
	sessionCookie      = "nfr_panel"
	sessionTTL         = 12 * time.Hour
	rememberSessionTTL = 30 * 24 * time.Hour
	maxFails           = 5
	longLockFails      = 10
	shortLockFor       = 2 * time.Minute
	longLockFor        = 10 * time.Minute
	failWindow         = 15 * time.Minute
	maxJSONBody        = 1 << 20 // 1 MiB cap for panel JSON API payloads
)

type ctxKey int

const ctxSessionKey ctxKey = 1

type session struct {
	userID string
	role   string
	csrf   string
	exp    time.Time
}

type failState struct {
	count    int
	lastFail time.Time
	until    time.Time
}

// Panel is the single-host console handler mounted at /nfr/.
type Panel struct {
	srv    *server.Server
	lg     *slog.Logger
	distFS fs.FS
	apiMux *http.ServeMux

	mu        sync.Mutex
	sessions  map[string]*session
	fails     map[string]*failState
	lastPrune time.Time

	gzMu    sync.RWMutex
	gzCache map[string][]byte
}

// New builds the console handler for one control node.
func New(srv *server.Server, lg *slog.Logger) *Panel {
	if lg == nil {
		lg = slog.Default()
	}
	sub, err := fs.Sub(embeddedDist, "dist")
	if err != nil {
		panic("panel: embed dist: " + err.Error())
	}
	p := &Panel{
		srv:      srv,
		lg:       lg,
		distFS:   sub,
		sessions: map[string]*session{},
		fails:    map[string]*failState{},
		gzCache:  map[string][]byte{},
	}
	if srv != nil {
		p.loadPersistedSessions()
		srv.SetOnStateReload(p.ReloadSessions)
		srv.SetOnUserRevoke(p.RevokeUserSessions)
	}
	p.apiMux = p.buildAPIMux()
	return p
}

func (p *Panel) loadPersistedSessions() {
	if p.srv == nil {
		return
	}
	adminID := ""
	if admin, ok := p.srv.AdminUser(); ok {
		adminID = admin.ID
	}
	next := make(map[string]*session)
	for _, ps := range p.srv.PanelSessions() {
		uid := ps.UserID
		if uid == "" {
			uid = adminID
		}
		role := store.RoleUser
		if u, ok := p.srv.UserByID(uid); ok {
			if u.Disabled {
				continue
			}
			role = u.Role
		} else {
			continue
		}
		next[ps.TokenHash] = &session{userID: uid, role: role, csrf: ps.CSRF, exp: ps.ExpiresAt}
	}
	p.mu.Lock()
	p.sessions = next
	p.mu.Unlock()
}

// ReloadSessions reloads persisted console sessions after switching storage to
// an already-populated MySQL database.
func (p *Panel) ReloadSessions() {
	p.loadPersistedSessions()
}

// RevokeUserSessions drops every active in-memory console session for userID.
func (p *Panel) RevokeUserSessions(userID string) {
	if userID == "" {
		return
	}
	p.mu.Lock()
	for k, s := range p.sessions {
		if s.userID == userID {
			delete(p.sessions, k)
		}
	}
	p.mu.Unlock()
}

// ServeHTTP routes /nfr/api/* to the JSON API and all other /nfr/* paths to
// the embedded Vue 3 SPA without requiring a session cookie on static assets.
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, r)
	if r.URL.Path == "/nfr" {
		http.Redirect(w, r, "/nfr/", http.StatusMovedPermanently)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/nfr/api/") {
		p.serveAPI(w, r)
		return
	}
	p.serveSPA(w, r)
}

func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	if isHTTPS(r) {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

func checkSameOrigin(r *http.Request) bool {
	orig := strings.TrimSpace(r.Header.Get("Origin"))
	if orig == "" {
		return true
	}
	u, err := url.Parse(orig)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (p *Panel) serveAPI(w http.ResponseWriter, r *http.Request) {
	if isStateChanging(r.Method) && !checkSameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid origin"})
		return
	}
	// Only two endpoints are reachable without a valid session cookie:
	// GET /nfr/api/auth/me and POST /nfr/api/auth/login.
	if r.URL.Path == "/nfr/api/auth/me" && r.Method == http.MethodGet {
		p.apiAuthMe(w, r)
		return
	}
	if r.URL.Path == "/nfr/api/auth/login" && r.Method == http.MethodPost {
		p.apiAuthLogin(w, r)
		return
	}

	sess := p.current(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	if isStateChanging(r.Method) && !p.checkCSRF(r, sess) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid csrf"})
		return
	}

	ctx := context.WithValue(r.Context(), ctxSessionKey, sess)
	p.apiMux.ServeHTTP(w, r.WithContext(ctx))
}

func sessionFrom(r *http.Request) *session {
	s, _ := r.Context().Value(ctxSessionKey).(*session)
	return s
}

func (p *Panel) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || sess.role != store.RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "仅管理员可访问此功能"})
			return
		}
		next(w, r)
	}
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// serveSPA serves exact embedded files from dist/ first (skipping the build
// placeholder), and falls back to dist/index.html for Vue Router History paths.
func (p *Panel) serveSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/nfr/")
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")

	// Never expose the git placeholder file as a public asset.
	if rel != "" && rel != "." && rel != "placeholder.txt" && !strings.HasPrefix(rel, "..") {
		if data, ok := p.readDistFile(rel); ok {
			if strings.HasPrefix(rel, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else if rel == "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			if ct := mime.TypeByExtension(path.Ext(rel)); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			p.writeStatic(w, r, rel, data)
			return
		}
	}

	// SPA History API fallback to index.html.
	indexHTML, ok := p.readDistFile("index.html")
	if !ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("NFR web console assets not built. Run: pnpm --dir web build\n"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	p.writeStatic(w, r, "index.html", indexHTML)
}

func isCompressibleExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".js", ".mjs", ".css", ".html", ".svg", ".json", ".txt", ".map":
		return true
	}
	return false
}

func (p *Panel) writeStatic(w http.ResponseWriter, r *http.Request, key string, raw []byte) {
	if len(raw) >= 512 && isCompressibleExt(path.Ext(key)) && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		gz := p.gzipped(key, raw)
		if len(gz) > 0 && len(gz) < len(raw) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(gz)
			}
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func (p *Panel) gzipped(key string, raw []byte) []byte {
	p.gzMu.RLock()
	if b, ok := p.gzCache[key]; ok {
		p.gzMu.RUnlock()
		return b
	}
	p.gzMu.RUnlock()

	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := gw.Write(raw); err != nil {
		return nil
	}
	if err := gw.Close(); err != nil {
		return nil
	}
	out := buf.Bytes()

	p.gzMu.Lock()
	if p.gzCache == nil {
		p.gzCache = map[string][]byte{}
	}
	p.gzCache[key] = out
	p.gzMu.Unlock()
	return out
}

func (p *Panel) readDistFile(rel string) ([]byte, bool) {
	if p.distFS == nil {
		return nil, false
	}
	info, err := fs.Stat(p.distFS, rel)
	if err != nil || info.IsDir() {
		return nil, false
	}
	b, err := fs.ReadFile(p.distFS, rel)
	if err != nil {
		return nil, false
	}
	return b, true
}

// ---------------------------------------------------------------------------
// sessions, auth & brute-force protection

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(b)
}

func (p *Panel) current(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	key := auth.Hash(c.Value)
	now := time.Now()
	p.mu.Lock()
	p.pruneLocked(now)
	s, ok := p.sessions[key]
	if !ok || now.After(s.exp) {
		delete(p.sessions, key)
		p.mu.Unlock()
		return nil
	}
	uid := s.userID
	p.mu.Unlock()

	if p.srv != nil {
		u, found := p.srv.UserByID(uid)
		if !found || u.Disabled {
			p.mu.Lock()
			delete(p.sessions, key)
			p.mu.Unlock()
			return nil
		}
		p.mu.Lock()
		if cur, still := p.sessions[key]; still {
			cur.role = u.Role
			s = cur
		}
		p.mu.Unlock()
	}
	return s
}

func (p *Panel) pruneLocked(now time.Time) {
	if now.Sub(p.lastPrune) < 5*time.Minute {
		return
	}
	p.lastPrune = now
	for id, s := range p.sessions {
		if now.After(s.exp) {
			delete(p.sessions, id)
		}
	}
	for ip, f := range p.fails {
		expiredLock := f.until.IsZero() || now.After(f.until)
		staleFail := f.lastFail.IsZero() || now.Sub(f.lastFail) > failWindow
		if expiredLock && staleFail {
			delete(p.fails, ip)
		}
	}
}

func (p *Panel) checkCSRF(r *http.Request, s *session) bool {
	token := strings.TrimSpace(r.Header.Get("X-CSRF"))
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
}

func (p *Panel) dbStatusForRole(role string) api.DBStatus {
	st := p.srv.DBStatus()
	if role != store.RoleAdmin {
		st.MandatorySetup = false
		st.Host = ""
		st.Port = 0
		st.Database = ""
		st.Username = ""
		st.HasPassword = false
		st.Error = ""
	}
	return st
}

func (p *Panel) infoForUser(uid, role string) api.Info {
	info := p.srv.Info()
	if role != store.RoleAdmin {
		tunnels := p.srv.TunnelsForOwner(uid)
		nodes := p.srv.NodesForOwner(uid)
		online := 0
		for _, n := range nodes {
			if n.Online {
				online++
			}
		}
		info.Nodes = online
		info.Tunnels = len(tunnels)
	}
	return info
}

func (p *Panel) apiAuthMe(w http.ResponseWriter, r *http.Request) {
	sess := p.current(r)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"version":       version.Version,
		})
		return
	}
	user, _ := p.srv.UserProfile(sess.userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          sess.csrf,
		"user":          user,
		"db":            p.dbStatusForRole(sess.role),
		"info":          p.infoForUser(sess.userID, sess.role),
		"version":       version.Version,
	})
}

func (p *Panel) apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if ok, retrySec := p.allowedWithRetry(ip); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":               "登录尝试过于频繁，请稍后再试",
			"retry_after_seconds": retrySec,
		})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	user, err := p.srv.AuthenticatePanel(req.Username, req.Password)
	if err != nil {
		low := strings.ToLower(err.Error())
		if strings.Contains(low, "停用") || strings.Contains(low, "disabled") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		p.failed(ip)
		p.lg.Warn("console login failed", "remote", ip, "username", strings.TrimSpace(req.Username))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	// Clear the one-time plaintext initial password once the admin signs in.
	if user.Role == store.RoleAdmin {
		p.srv.ClearInitialPassword()
	}

	ttl := sessionTTL
	if req.Remember {
		ttl = rememberSessionTTL
	}
	id := randomToken()
	key := auth.Hash(id)
	exp := time.Now().UTC().Add(ttl)
	s := &session{userID: user.ID, role: user.Role, csrf: randomToken(), exp: exp}
	p.mu.Lock()
	p.sessions[key] = s
	delete(p.fails, ip)
	p.mu.Unlock()
	if req.Remember {
		if err := p.srv.SavePanelSession(store.PanelSession{
			TokenHash: key,
			UserID:    user.ID,
			CSRF:      s.csrf,
			ExpiresAt: exp,
		}); err != nil {
			p.lg.Warn("persist remember session failed", "err", err)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/nfr",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  s.exp,
		MaxAge:   int(ttl.Seconds()),
	})
	prof, _ := p.srv.UserProfile(user.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          s.csrf,
		"user":          prof,
		"db":            p.dbStatusForRole(user.Role),
		"info":          p.infoForUser(user.ID, user.Role),
		"version":       version.Version,
	})
}

func (p *Panel) apiAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		key := auth.Hash(c.Value)
		p.mu.Lock()
		delete(p.sessions, key)
		p.mu.Unlock()
		_ = p.srv.DeletePanelSession(key)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/nfr",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// revokeOtherSessions invalidates every console session of the calling user
// except the caller's current cookie.
func (p *Panel) revokeOtherSessions(r *http.Request) {
	sess := sessionFrom(r)
	userID := ""
	if sess != nil {
		userID = sess.userID
	}
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		keep = auth.Hash(c.Value)
	}
	p.mu.Lock()
	for id, s := range p.sessions {
		if id != keep && (userID == "" || s.userID == userID) {
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	_ = p.srv.RevokeUserPanelSessions(userID, keep)
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(host), "[]"))
	return ip != nil && ip.IsLoopback()
}

func remoteHostOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return isLoopbackIP(remoteHostOf(r)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (p *Panel) allowedWithRetry(ip string) (bool, int) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fails[ip]
	if f == nil {
		return true, 0
	}
	if !f.until.IsZero() && now.Before(f.until) {
		sec := int(time.Until(f.until).Seconds())
		if sec < 1 {
			sec = 1
		}
		return false, sec
	}
	if !f.lastFail.IsZero() && now.Sub(f.lastFail) > failWindow {
		delete(p.fails, ip)
	}
	return true, 0
}

func (p *Panel) failed(ip string) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	f := p.fails[ip]
	if f == nil || (!f.lastFail.IsZero() && now.Sub(f.lastFail) > failWindow) {
		f = &failState{}
		p.fails[ip] = f
	}
	f.count++
	f.lastFail = now
	switch {
	case f.count >= longLockFails:
		f.until = now.Add(longLockFor)
	case f.count >= maxFails:
		f.until = now.Add(shortLockFor)
	}
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求 JSON 格式无效: " + err.Error()})
		return false
	}
	return true
}

// clientIP resolves the caller IP for login rate limiting. When the TCP peer is
// a trusted local reverse proxy (127.0.0.1 / ::1), it honours X-Real-IP or the
// rightmost non-loopback X-Forwarded-For entry so one bad actor behind nginx
// never locks out 127.0.0.1 for everyone else. Direct public connections always
// ignore proxy headers to prevent IP spoofing.
func clientIP(r *http.Request) string {
	remote := remoteHostOf(r)
	if !isLoopbackIP(remote) {
		return remote
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := net.ParseIP(strings.Trim(xri, "[]")); ip != nil {
			return ip.String()
		}
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			cand := strings.Trim(strings.TrimSpace(parts[i]), "[]")
			if ip := net.ParseIP(cand); ip != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return remote
}

func checkPassword(hash, plain string) bool {
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
