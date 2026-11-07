// Package panel serves the built in console. Everything is rendered on the
// server with html/template so the binary stays a single file: no npm, no
// frontend build step, no CDN dependency.
package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/server"
)

//go:embed templates/*.html
var files embed.FS

const (
	sessionCookie = "nfr_panel"
	sessionTTL    = 12 * time.Hour
	maxFails      = 5
	lockFor       = 15 * time.Minute
)

type session struct {
	csrf string
	exp  time.Time
}

type failState struct {
	count int
	until time.Time
}

// Panel is the console handler mounted at /_nfr/.
type Panel struct {
	srv   *server.Server
	lg    *slog.Logger
	pages map[string]*template.Template

	mu       sync.Mutex
	sessions map[string]*session
	fails    map[string]*failState
}

// New builds the console for a node.
func New(srv *server.Server, lg *slog.Logger) *Panel {
	if lg == nil {
		lg = slog.Default()
	}
	funcs := template.FuncMap{
		"bytes": humanBytes,
		"ago":   humanAgo,
		"short": shortTime,
		"port":  portLabel,
		"join":  strings.Join,
	}
	base := template.Must(template.New("layout").Funcs(funcs).ParseFS(files, "templates/layout.html"))
	pages := map[string]*template.Template{}
	entries, err := files.ReadDir("templates")
	if err != nil {
		panic("panel: " + err.Error())
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" || !strings.HasSuffix(name, ".html") {
			continue
		}
		body, err := files.ReadFile("templates/" + name)
		if err != nil {
			panic("panel: " + err.Error())
		}
		clone, err := base.Clone()
		if err != nil {
			panic("panel: " + err.Error())
		}
		t := template.Must(clone.New("content").Parse(string(body)))
		pages[strings.TrimSuffix(name, ".html")] = t
	}
	return &Panel{
		srv: srv, lg: lg, pages: pages,
		sessions: map[string]*session{},
		fails:    map[string]*failState{},
	}
}

// ServeHTTP routes the console.
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_nfr":
		http.Redirect(w, r, "/_nfr/", http.StatusMovedPermanently)
		return
	case r.URL.Path == "/_nfr/login":
		p.login(w, r)
		return
	case r.URL.Path == "/_nfr/logout":
		p.logout(w, r)
		return
	}
	sess := p.current(r)
	if sess == nil {
		if strings.HasPrefix(r.URL.Path, "/_nfr/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		http.Redirect(w, r, "/_nfr/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if !p.checkCSRF(r, sess) {
			http.Error(w, "invalid CSRF token, reload the page", http.StatusForbidden)
			return
		}
	}
	switch {
	case r.URL.Path == "/_nfr/" || r.URL.Path == "/_nfr/overview":
		p.overview(w, r, sess)
	case r.URL.Path == "/_nfr/tunnels":
		p.tunnels(w, r, sess)
	case r.URL.Path == "/_nfr/tunnels/new":
		p.tunnelForm(w, r, sess, nil)
	case r.URL.Path == "/_nfr/tunnels/edit":
		p.editForm(w, r, sess)
	case r.URL.Path == "/_nfr/tunnels/save":
		p.tunnelSave(w, r, sess)
	case r.URL.Path == "/_nfr/tunnels/delete":
		p.tunnelDelete(w, r, sess)
	case r.URL.Path == "/_nfr/tunnels/toggle":
		p.tunnelToggle(w, r, sess)
	case r.URL.Path == "/_nfr/nodes":
		p.nodes(w, r, sess)
	case r.URL.Path == "/_nfr/servers":
		p.servers(w, r, sess)
	case r.URL.Path == "/_nfr/settings":
		p.settings(w, r, sess)
	case r.URL.Path == "/_nfr/keys":
		p.keys(w, r, sess)
	case r.URL.Path == "/_nfr/api/allocate":
		p.allocate(w, r, sess)
	case r.URL.Path == "/_nfr/api/state":
		p.state(w, r, sess)
	default:
		http.NotFound(w, r)
	}
}

// refreshRemotes probes every managed instance in parallel so the cards show
// live reachability instead of the state of the last mutation.
func (p *Panel) refreshRemotes(parent context.Context) {
	remotes := p.srv.Remotes()
	if len(remotes) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, r := range remotes {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, 4*time.Second)
			defer cancel()
			if _, err := p.srv.RefreshRemote(ctx, id); err != nil {
				p.lg.Debug("managed server unreachable", "id", id, "err", err)
			}
		}(r.ID)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// sessions

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
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[c.Value]
	if !ok || time.Now().After(s.exp) {
		delete(p.sessions, c.Value)
		return nil
	}
	return s
}

func (p *Panel) checkCSRF(r *http.Request, s *session) bool {
	token := r.FormValue("csrf")
	if token == "" {
		token = r.Header.Get("X-CSRF")
	}
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
}

func (p *Panel) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if p.current(r) != nil {
			http.Redirect(w, r, "/_nfr/", http.StatusSeeOther)
			return
		}
		p.render(w, r, "login", base{Title: "Sign in", Nav: "login", Error: r.URL.Query().Get("err")})
		return
	}
	ip := clientIP(r)
	if !p.allowed(ip) {
		http.Redirect(w, r, "/_nfr/login?err="+url.QueryEscape("too many attempts, try again later"), http.StatusSeeOther)
		return
	}
	password := r.FormValue("password")
	hash := p.srv.PanelPasswordHash()
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		p.failed(ip)
		p.lg.Warn("console login failed", "remote", ip)
		http.Redirect(w, r, "/_nfr/login?err="+url.QueryEscape("wrong password"), http.StatusSeeOther)
		return
	}
	id := randomToken()
	s := &session{csrf: randomToken(), exp: time.Now().Add(sessionTTL)}
	p.mu.Lock()
	p.sessions[id] = s
	p.fails[ip] = &failState{}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/_nfr",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  s.exp,
	})
	http.Redirect(w, r, "/_nfr/", http.StatusSeeOther)
}

func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		p.mu.Lock()
		delete(p.sessions, c.Value)
		p.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/_nfr", MaxAge: -1, Secure: true, HttpOnly: true})
	http.Redirect(w, r, "/_nfr/login", http.StatusSeeOther)
}

func (p *Panel) allowed(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fails[ip]
	if f == nil {
		return true
	}
	if time.Now().Before(f.until) {
		return false
	}
	if f.count >= maxFails && !f.until.IsZero() && time.Now().After(f.until) {
		f.count = 0
		f.until = time.Time{}
	}
	return true
}

func (p *Panel) failed(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fails[ip]
	if f == nil {
		f = &failState{}
		p.fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.until = time.Now().Add(lockFor)
	}
}

// ---------------------------------------------------------------------------
// rendering

type base struct {
	Title  string
	Page   string
	Nav    string
	CSRF   string
	Error  string
	Notice string
	Info   api.Info
}

func (p *Panel) newBase(r *http.Request, s *session, nav, title, page string) base {
	q := r.URL.Query()
	return base{
		Title:  title,
		Page:   page,
		Nav:    nav,
		CSRF:   s.csrf,
		Error:  q.Get("err"),
		Notice: q.Get("ok"),
		Info:   p.srv.Info(),
	}
}

func (p *Panel) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	t, ok := p.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		p.lg.Error("render console", "page", page, "err", err)
	}
}

func (p *Panel) back(w http.ResponseWriter, r *http.Request, path string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	}
	if v := r.FormValue("server"); v != "" {
		q.Set("server", v)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (p *Panel) ok(w http.ResponseWriter, r *http.Request, path, msg string) {
	q := url.Values{}
	q.Set("ok", msg)
	if v := r.FormValue("server"); v != "" {
		q.Set("server", v)
	}
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// template helpers

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanAgo(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("01-02 15:04")
}

func portLabel(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func hashPassword(plain string) (string, error) { return config.HashPassword(plain) }

func checkPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
