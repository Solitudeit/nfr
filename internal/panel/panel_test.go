package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

func newTestPanel(t *testing.T) (*Panel, *server.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.Domain = "example.com"
	cfg.PublicHost = "203.0.113.10"
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Seed one initial password file to verify login clears it.
	if err := os.WriteFile(filepath.Join(dir, "initial-password.txt"), []byte(password+"\n"), 0o600); err != nil {
		t.Fatalf("seed initial password file: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	return p, srv, password
}

type stubLocator struct{}

func (stubLocator) locate(context.Context, geoQuery) region {
	return region{Country: "香港", Code: "HK"}
}

func doJSON(p *Panel, method, path string, body any, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF", csrf)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func signIn(t *testing.T, p *Panel, password string) ([]*http.Cookie, string) {
	t.Helper()
	rec := doJSON(p, http.MethodPost, "/nfr/api/auth/login", map[string]string{"password": password}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure cookie: %+v", c)
	}
	var out struct {
		Authenticated bool   `json:"authenticated"`
		CSRF          string `json:"csrf"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.Authenticated || out.CSRF == "" {
		t.Fatalf("unexpected login response: %s", rec.Body.String())
	}
	return cookies, out.CSRF
}

func TestSPAServingFallbackAndUnbuilt503(t *testing.T) {
	p, _, _ := newTestPanel(t)

	// Unbuilt distFS containing only placeholder.txt must return 503 with build hint
	// and never serve placeholder.txt directly.
	p.distFS = fstest.MapFS{
		"placeholder.txt": {Data: []byte("placeholder")},
	}
	rec := doJSON(p, http.MethodGet, "/nfr/", nil, nil, "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "pnpm --dir web build") {
		t.Fatalf("unbuilt root = %d %q", rec.Code, rec.Body.String())
	}
	rec = doJSON(p, http.MethodGet, "/nfr/placeholder.txt", nil, nil, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("placeholder.txt should not be served directly, got %d", rec.Code)
	}

	// Built distFS serves exact assets with immutable cache and SPA history fallback without cookie.
	p.distFS = fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><div id=app></div>")},
		"favicon.svg":       {Data: []byte("<svg></svg>")},
		"assets/app-abc.js": {Data: []byte("console.log(1)")},
	}
	rec = doJSON(p, http.MethodGet, "/nfr/create", nil, nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=app") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("spa fallback = %d cc=%q body=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	rec = doJSON(p, http.MethodGet, "/nfr/assets/app-abc.js", nil, nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d cc=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = doJSON(p, http.MethodGet, "/nfr/favicon.svg", nil, nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg>") {
		t.Fatalf("favicon = %d body=%q", rec.Code, rec.Body.String())
	}
	// /nfr redirects to /nfr/.
	rec = doJSON(p, http.MethodGet, "/nfr", nil, nil, "")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/nfr/" {
		t.Fatalf("bare /nfr = %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthCSRFAndBruteForceLockout(t *testing.T) {
	p, srv, password := newTestPanel(t)

	// Unauthenticated /auth/me returns 200 {authenticated: false}.
	rec := doJSON(p, http.MethodGet, "/nfr/api/auth/me", nil, nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("auth/me = %d %s", rec.Code, rec.Body.String())
	}
	// Protected endpoints return 401 JSON (never 302 HTML redirect).
	rec = doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard unauth = %d, want 401", rec.Code)
	}

	// 5 wrong passwords trigger 429 lockout for that IP.
	for i := 0; i < maxFails; i++ {
		rec = doJSON(p, http.MethodPost, "/nfr/api/auth/login", map[string]string{"password": "wrong"}, nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad login %d = %d, want 401", i, rec.Code)
		}
	}
	rec = doJSON(p, http.MethodPost, "/nfr/api/auth/login", map[string]string{"password": password}, nil, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login = %d, want 429", rec.Code)
	}

	// Clear lockout for normal login test and verify initial password is cleared.
	p.mu.Lock()
	p.fails = map[string]*failState{}
	p.mu.Unlock()

	cookies, csrf := signIn(t, p, password)
	if _, err := os.Stat(filepath.Join(srv.DataDir(), "initial-password.txt")); !os.IsNotExist(err) {
		t.Fatalf("initial-password.txt was not cleared after login: %v", err)
	}

	// All state-changing methods must reject missing/wrong X-CSRF with 403.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/nfr/api/ports/allocate"},
		{http.MethodPatch, "/nfr/api/settings"},
		{http.MethodDelete, "/nfr/api/tunnels/missing"},
		{http.MethodPost, "/nfr/api/auth/logout"},
	} {
		r := doJSON(p, tc.method, tc.path, map[string]any{}, cookies, "bad-csrf")
		if r.Code != http.StatusForbidden {
			t.Fatalf("%s %s without valid CSRF = %d, want 403", tc.method, tc.path, r.Code)
		}
	}

	// Logout invalidates session.
	rec = doJSON(p, http.MethodPost, "/nfr/api/auth/logout", nil, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	rec = doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, cookies, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout dashboard = %d, want 401", rec.Code)
	}
}

func TestSingleHostAPIFullLifecycle(t *testing.T) {
	p, srv, password := newTestPanel(t)
	cookies, csrf := signIn(t, p, password)

	// Panel multi-host federation route is gone.
	rec := doJSON(p, http.MethodGet, "/nfr/api/servers", nil, cookies, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/nfr/api/servers = %d, want 404", rec.Code)
	}

	// Seed a node via store through join code + direct test node isn't needed; create join code first.
	rec = doJSON(p, http.MethodPost, "/nfr/api/nodes/join-codes", map[string]any{"name": "nas", "ttl_seconds": 300}, cookies, csrf)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "nfrc join") {
		t.Fatalf("join-codes = %d %s", rec.Code, rec.Body.String())
	}

	// Allocate port (TCP and UDP).
	rec = doJSON(p, http.MethodPost, "/nfr/api/ports/allocate", map[string]any{"network": "tcp"}, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("allocate tcp = %d %s", rec.Code, rec.Body.String())
	}
	var alloc struct {
		Port    int    `json:"port"`
		LeaseID string `json:"lease_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &alloc)
	if alloc.Port <= 0 || alloc.LeaseID == "" {
		t.Fatalf("bad allocate response: %s", rec.Body.String())
	}

	// Create an awaiting tunnel via backend helper to verify legacy awaiting tunnels render & can be probed/deleted.
	awaitTun, _, err := srv.CreateTunnelAwaiting(api.TunnelRequest{
		Name:       "legacy-ssh",
		NodeID:     "new",
		Type:       "tcp",
		Local:      "127.0.0.1:22",
		RemotePort: alloc.Port,
		LeaseID:    alloc.LeaseID,
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("CreateTunnelAwaiting: %v", err)
	}

	// Dashboard returns info, tunnels, nodes, events, and rates array.
	rec = doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, cookies, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard = %d %s", rec.Code, rec.Body.String())
	}
	var dash map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	for _, k := range []string{"info", "tunnels", "nodes", "events", "rates", "settings"} {
		if _, ok := dash[k]; !ok {
			t.Fatalf("dashboard missing key %q", k)
		}
	}

	// Single probe and probe-all on awaiting tunnel return awaiting cleanly.
	rec = doJSON(p, http.MethodPost, "/nfr/api/tunnels/"+awaitTun.ID+"/probe", nil, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"awaiting"`) {
		t.Fatalf("probe awaiting = %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(p, http.MethodPost, "/nfr/api/tunnels/probe-all", nil, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"awaiting"`) {
		t.Fatalf("probe-all = %d %s", rec.Code, rec.Body.String())
	}

	// Toggle and delete tunnel.
	rec = doJSON(p, http.MethodPost, "/nfr/api/tunnels/"+awaitTun.ID+"/toggle", map[string]bool{"enabled": false}, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("toggle = %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(p, http.MethodDelete, "/nfr/api/tunnels/"+awaitTun.ID, nil, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete tunnel = %d %s", rec.Code, rec.Body.String())
	}

	// Configs endpoint returns server TOML/YAML and client template TOML/YAML.
	rec = doJSON(p, http.MethodGet, "/nfr/api/configs", nil, cookies, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "server_toml") || !strings.Contains(rec.Body.String(), "auth_token") {
		t.Fatalf("configs = %d %s", rec.Code, rec.Body.String())
	}

	// Settings patch, auth-token update, REST API toggle, API keys create & revoke, password change.
	rec = doJSON(p, http.MethodPatch, "/nfr/api/settings", map[string]any{
		"name":        "prod-hk",
		"domain":      "example.com",
		"public_host": "203.0.113.10",
		"port_range":  [2]int{20000, 29999},
		"email":       "ops@example.com",
	}, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "prod-hk") {
		t.Fatalf("patch settings = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(p, http.MethodPost, "/nfr/api/settings/auth-token", map[string]string{"auth_token": "nfr_custom_token_123"}, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nfr_custom_token_123") {
		t.Fatalf("set auth-token = %d %s", rec.Code, rec.Body.String())
	}

	// Empty auth_token triggers random token regeneration (review fix #1).
	rec = doJSON(p, http.MethodPost, "/nfr/api/settings/auth-token", map[string]string{"auth_token": ""}, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auth_token":"nfr_t_`) {
		t.Fatalf("regenerate auth-token = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(p, http.MethodPost, "/nfr/api/settings/api-toggle", map[string]bool{"enabled": true}, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"api_enabled":true`) {
		t.Fatalf("api-toggle = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(p, http.MethodPost, "/nfr/api/api-keys", map[string]string{"label": "ci", "scope": "admin"}, cookies, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create api key = %d %s", rec.Code, rec.Body.String())
	}
	var keyResp struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &keyResp)
	if keyResp.ID == "" || keyResp.Key == "" {
		t.Fatalf("bad key response: %s", rec.Body.String())
	}
	rec = doJSON(p, http.MethodDelete, "/nfr/api/api-keys/"+keyResp.ID, nil, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete api key = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(p, http.MethodPost, "/nfr/api/settings/password", map[string]string{
		"old_password": password,
		"new_password": "new-strong-password-99",
	}, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d %s", rec.Code, rec.Body.String())
	}
}

func TestStandaloneTunnelConfigAndKickAPI(t *testing.T) {
	p, _, password := newTestPanel(t)
	cookies, csrf := signIn(t, p, password)

	// Create standalone tunnel without node_id.
	rec := doJSON(p, http.MethodPost, "/nfr/api/tunnels", map[string]any{
		"name":        "jiaoshi",
		"type":        "tcp",
		"local":       "127.0.0.1:8080",
		"remote_port": 23456,
		"enabled":     true,
	}, cookies, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("standalone create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Tunnel api.Tunnel `json:"tunnel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Tunnel.ID == "" || created.Tunnel.NodeID != "" || created.Tunnel.RemotePort != 23456 {
		t.Fatalf("unexpected standalone tunnel: %+v", created.Tunnel)
	}

	// Fetch per-tunnel generated config.
	rec = doJSON(p, http.MethodGet, "/nfr/api/tunnels/"+created.Tunnel.ID+"/config", nil, cookies, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnel config = %d %s", rec.Code, rec.Body.String())
	}
	var cfgResp struct {
		TunnelID     string `json:"tunnel_id"`
		FilenameTOML string `json:"filename_toml"`
		TOML         string `json:"toml"`
		YAML         string `json:"yaml"`
		RunHint      string `json:"run_hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfgResp.TunnelID != created.Tunnel.ID ||
		!strings.Contains(cfgResp.TOML, `name = "t-`+created.Tunnel.ID+`"`) ||
		!strings.Contains(cfgResp.TOML, `name = "jiaoshi"`) ||
		!strings.Contains(cfgResp.TOML, `remote_port = 23456`) ||
		!strings.Contains(cfgResp.YAML, "jiaoshi") ||
		cfgResp.FilenameTOML == "" || cfgResp.RunHint == "" {
		t.Fatalf("bad per-tunnel config payload: %+v", cfgResp)
	}

	// Kick offline tunnel returns 200 ok with kicked=false.
	rec = doJSON(p, http.MethodPost, "/nfr/api/tunnels/"+created.Tunnel.ID+"/kick", nil, cookies, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kicked":false`) {
		t.Fatalf("kick offline tunnel = %d %s", rec.Code, rec.Body.String())
	}
}

func TestClientDownloadsAPIAndTraversalBlocked(t *testing.T) {
	clientsDir := t.TempDir()
	t.Setenv("NFR_CLIENTS_DIR", clientsDir)

	if err := os.WriteFile(filepath.Join(clientsDir, "nfrc_linux_amd64.tar.gz"), []byte("TGZ-LINUX-ARCHIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientsDir, "nfrc_windows_amd64.zip"), []byte("ZIP-WIN-ARCHIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Secret file in parent dir to test path traversal rejection.
	parentSecret := filepath.Join(filepath.Dir(clientsDir), "secret.txt")
	_ = os.WriteFile(parentSecret, []byte("TOP-SECRET"), 0o600)

	p, _, password := newTestPanel(t)
	cookies, _ := signIn(t, p, password)

	// Catalog always returns all 8 platforms with GitHub & Mirror archive URLs
	// (no Gitee), plus mirror probe nodes and local source when local archives exist.
	rec := doJSON(p, http.MethodGet, "/nfr/api/downloads", nil, cookies, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("downloads catalog = %d %s", rec.Code, rec.Body.String())
	}
	var cat struct {
		Sources []struct {
			Key     string `json:"key"`
			Label   string `json:"label"`
			BaseURL string `json:"base_url"`
		} `json:"sources"`
		Mirrors []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Prefix   string `json:"prefix"`
			ProbeURL string `json:"probe_url"`
		} `json:"mirrors"`
		Items []struct {
			OS             string            `json:"os"`
			Arch           string            `json:"arch"`
			Filename       string            `json:"filename"`
			BinaryName     string            `json:"binary_name"`
			LocalAvailable bool              `json:"local_available"`
			Size           int64             `json:"size_bytes"`
			URLs           map[string]string `json:"urls"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(cat.Items) != 8 {
		t.Fatalf("expected 8 platform items, got %d (%s)", len(cat.Items), rec.Body.String())
	}
	if len(cat.Mirrors) < 3 {
		t.Fatalf("expected at least 3 mirror nodes, got %+v", cat.Mirrors)
	}
	srcKeys := map[string]bool{}
	for _, s := range cat.Sources {
		srcKeys[s.Key] = true
	}
	if !srcKeys["github"] || !srcKeys["mirror"] || !srcKeys["local"] || srcKeys["gitee"] {
		t.Fatalf("expected github, mirror and local sources (no gitee), got %+v", cat.Sources)
	}
	for _, it := range cat.Items {
		if !strings.HasPrefix(it.URLs["github"], "https://github.com/Solitudeit/nfr/releases/download/") ||
			!strings.HasSuffix(it.URLs["github"], "/"+it.Filename) {
			t.Fatalf("bad github url: %+v", it)
		}
		if !strings.Contains(it.URLs["mirror"], "https://github.com/Solitudeit/nfr/releases/download/") ||
			!strings.HasSuffix(it.URLs["mirror"], "/"+it.Filename) {
			t.Fatalf("bad mirror url: %+v", it)
		}
		if _, hasGitee := it.URLs["gitee"]; hasGitee {
			t.Fatalf("unexpected gitee url still present: %+v", it)
		}
		if it.OS == "windows" && it.BinaryName != "nfrc.exe" {
			t.Fatalf("windows binary_name = %q, want nfrc.exe", it.BinaryName)
		}
		if it.OS != "windows" && it.BinaryName != "nfrc" {
			t.Fatalf("%s binary_name = %q, want nfrc", it.OS, it.BinaryName)
		}
	}

	// Direct local download streams exact archive bytes with attachment header.
	req := httptest.NewRequest(http.MethodGet, "/nfr/api/downloads/nfrc_linux_amd64.tar.gz", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	dlRec := httptest.NewRecorder()
	p.ServeHTTP(dlRec, req)
	if dlRec.Code != http.StatusOK || dlRec.Body.String() != "TGZ-LINUX-ARCHIVE" ||
		!strings.Contains(dlRec.Header().Get("Content-Disposition"), "nfrc_linux_amd64.tar.gz") {
		t.Fatalf("download file = %d hdr=%q body=%q", dlRec.Code, dlRec.Header().Get("Content-Disposition"), dlRec.Body.String())
	}

	// Path traversal and unknown filenames must be blocked.
	for _, bad := range []string{"../secret.txt", "..%2fsecret.txt", "unknown.bin", "nfrc_linux_amd64.tar.gz/../../etc/passwd"} {
		badReq := httptest.NewRequest(http.MethodGet, "/nfr/api/downloads/"+bad, nil)
		for _, c := range cookies {
			badReq.AddCookie(c)
		}
		badRec := httptest.NewRecorder()
		p.ServeHTTP(badRec, badReq)
		if badRec.Code == http.StatusOK || strings.Contains(badRec.Body.String(), "TOP-SECRET") {
			t.Fatalf("traversal %q was NOT blocked: status=%d body=%q", bad, badRec.Code, badRec.Body.String())
		}
	}
}

func TestLoginRememberSetsLongCookie(t *testing.T) {
	p, _, password := newTestPanel(t)

	rec := doJSON(p, http.MethodPost, "/nfr/api/auth/login", map[string]any{
		"password": password,
		"remember": true,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remember login = %d %s", rec.Code, rec.Body.String())
	}
	var sessCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil {
		t.Fatalf("missing session cookie")
	}
	if sessCookie.MaxAge < int((29*24*time.Hour).Seconds()) {
		t.Fatalf("remember cookie MaxAge = %d, want >= 29 days", sessCookie.MaxAge)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	p, _, password := newTestPanel(t)
	cookiesA, csrfA := signIn(t, p, password)
	cookiesB, _ := signIn(t, p, password)

	// Session B is initially valid.
	if rec := doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, cookiesB, ""); rec.Code != http.StatusOK {
		t.Fatalf("session B before password change = %d", rec.Code)
	}

	// Session A rotates the admin password.
	rec := doJSON(p, http.MethodPost, "/nfr/api/settings/password", map[string]string{
		"old_password": password,
		"new_password": "new-secure-password-2026",
	}, cookiesA, csrfA)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password = %d %s", rec.Code, rec.Body.String())
	}

	// Session A stays alive; session B is immediately revoked.
	if rec := doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, cookiesA, ""); rec.Code != http.StatusOK {
		t.Fatalf("session A after password change = %d, want 200", rec.Code)
	}
	if rec := doJSON(p, http.MethodGet, "/nfr/api/dashboard", nil, cookiesB, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session B after password change = %d, want 401", rec.Code)
	}
}

func TestRememberSessionSurvivesRestartAndRevokesOnLogout(t *testing.T) {
	p1, srv, password := newTestPanel(t)

	rec := doJSON(p1, http.MethodPost, "/nfr/api/auth/login", map[string]any{
		"password": password,
		"remember": true,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remember login = %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	var loginBody struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &loginBody)

	// Simulate an nfrs restart by constructing a brand-new Panel on the same Server/Store.
	p2 := New(srv, log.New("error", ""))
	if me := doJSON(p2, http.MethodGet, "/nfr/api/dashboard", nil, cookies, ""); me.Code != http.StatusOK {
		t.Fatalf("remembered session after restart = %d, want 200", me.Code)
	}

	// Explicit logout revokes the persisted session across future restarts too.
	if out := doJSON(p2, http.MethodPost, "/nfr/api/auth/logout", nil, cookies, loginBody.CSRF); out.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", out.Code, out.Body.String())
	}
	p3 := New(srv, log.New("error", ""))
	if me := doJSON(p3, http.MethodGet, "/nfr/api/dashboard", nil, cookies, ""); me.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session after restart = %d, want 401", me.Code)
	}
}
