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
	lockFor            = 15 * time.Minute
	maxJSONBody        = 1 << 20 // 1 MiB cap for panel JSON API payloads
)

type session struct {
	csrf string
	exp  time.Time
}

type failState struct {
	count int
	until time.Time
}

// Panel is the single-host console handler mounted at /nfr/.
type Panel struct {
	srv    *server.Server
	lg     *slog.Logger
	geo    locator
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
		geo:      newGeoResolver(),
		distFS:   sub,
		sessions: map[string]*session{},
		fails:    map[string]*failState{},
		gzCache:  map[string][]byte{},
	}
	if srv != nil {
		for _, ps := range srv.PanelSessions() {
			p.sessions[ps.TokenHash] = &session{csrf: ps.CSRF, exp: ps.ExpiresAt}
		}
	}
	p.apiMux = p.buildAPIMux()
	return p
}

// ServeHTTP routes /nfr/api/* to the JSON API and all other /nfr/* paths to
// the embedded Vue 3 SPA without requiring a session cookie on static assets.
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (p *Panel) serveAPI(w http.ResponseWriter, r *http.Request) {
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

	p.apiMux.ServeHTTP(w, r)
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
	defer p.mu.Unlock()
	p.pruneLocked(now)
	s, ok := p.sessions[key]
	if !ok || now.After(s.exp) {
		delete(p.sessions, key)
		return nil
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
		if !f.until.IsZero() && now.After(f.until) {
			delete(p.fails, ip)
		}
	}
}

func (p *Panel) checkCSRF(r *http.Request, s *session) bool {
	token := strings.TrimSpace(r.Header.Get("X-CSRF"))
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
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
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          sess.csrf,
		"info":          p.srv.Info(),
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
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	hash := p.srv.PanelPasswordHash()
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		p.failed(ip)
		p.lg.Warn("console login failed", "remote", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}

	// Clear the one-time plaintext initial password once the operator signs in.
	p.srv.ClearInitialPassword()

	ttl := sessionTTL
	if req.Remember {
		ttl = rememberSessionTTL
	}
	id := randomToken()
	key := auth.Hash(id)
	exp := time.Now().UTC().Add(ttl)
	s := &session{csrf: randomToken(), exp: exp}
	p.mu.Lock()
	p.sessions[key] = s
	p.fails[ip] = &failState{}
	p.mu.Unlock()
	if req.Remember {
		if err := p.srv.SavePanelSession(store.PanelSession{
			TokenHash: key,
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
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf":          s.csrf,
		"info":          p.srv.Info(),
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

// revokeOtherSessions invalidates every console session except the caller's
// current cookie (used when rotating the admin password so stolen or old
// 30-day sessions cannot outlive a password change).
func (p *Panel) revokeOtherSessions(r *http.Request) {
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		keep = auth.Hash(c.Value)
	}
	p.mu.Lock()
	for id := range p.sessions {
		if id != keep {
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	_ = p.srv.RevokeOtherPanelSessions(keep)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (p *Panel) allowedWithRetry(ip string) (bool, int) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fails[ip]
	if f == nil {
		return true, 0
	}
	if f.count >= maxFails && now.Before(f.until) {
		sec := int(time.Until(f.until).Seconds())
		if sec < 1 {
			sec = 1
		}
		return false, sec
	}
	if !f.until.IsZero() && now.After(f.until) {
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
	if f == nil || (!f.until.IsZero() && now.After(f.until)) {
		f = &failState{}
		p.fails[ip] = f
	}
	f.count++
	f.until = now.Add(lockFor)
}

func (p *Panel) labelNodes(parent context.Context, nodes []api.Node) {
	if len(nodes) == 0 || p.geo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, geoBudget)
	defer cancel()
	for i := range nodes {
		if nodes[i].RemoteAddr == "" {
			continue
		}
		nodes[i].Region = p.geo.locate(ctx, geoQuery{Host: nodes[i].RemoteAddr}).Country
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

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
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
