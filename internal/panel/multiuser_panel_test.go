package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

type panelClient struct {
	t       *testing.T
	handler http.Handler
	cookies []*http.Cookie
	csrf    string
}

func (c *panelClient) req(method, path string, body any, contentType string) *httptest.ResponseRecorder {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			rdr = bytes.NewReader(v)
		case string:
			rdr = bytes.NewBufferString(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				c.t.Fatalf("marshal: %v", err)
			}
			rdr = bytes.NewReader(b)
			if contentType == "" {
				contentType = "application/json"
			}
		}
	}
	r := httptest.NewRequest(method, path, rdr)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for _, ck := range c.cookies {
		r.AddCookie(ck)
	}
	if c.csrf != "" && isStateChanging(method) {
		r.Header.Set("X-CSRF", c.csrf)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	if respCookies := w.Result().Cookies(); len(respCookies) > 0 {
		c.cookies = respCookies
	}
	return w
}

func (c *panelClient) login(username, password string) *httptest.ResponseRecorder {
	payload := map[string]any{"password": password}
	if username != "" {
		payload["username"] = username
	}
	w := c.req(http.MethodPost, "/nfr/api/auth/login", payload, "")
	if w.Code == http.StatusOK {
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if csrf, ok := resp["csrf"].(string); ok {
			c.csrf = csrf
		}
	}
	return w
}

func newTestMultiUserPanel(t *testing.T) (*server.Server, *Panel, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.PortRange = [2]int{32000, 39999}

	srv, pw, err := server.New(server.Options{Config: cfg, Store: st})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	p := New(srv, nil)
	return srv, p, pw
}

func TestMultiUserPanelRBACIsolationAndAvatars(t *testing.T) {
	t.Setenv("NFR_STORAGE", "")
	_, p, adminPw := newTestMultiUserPanel(t)

	admin := &panelClient{t: t, handler: p}
	if w := admin.login("admin", adminPw); w.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d %s", w.Code, w.Body.String())
	}

	// GET /nfr/api/auth/me reports db.mandatory_setup = true for admin when NFR_STORAGE unset
	wMe := admin.req(http.MethodGet, "/nfr/api/auth/me", nil, "")
	var meAdmin struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
		DB struct {
			Mode           string `json:"mode"`
			MandatorySetup bool   `json:"mandatory_setup"`
		} `json:"db"`
	}
	if err := json.Unmarshal(wMe.Body.Bytes(), &meAdmin); err != nil {
		t.Fatalf("unmarshal meAdmin: %v", err)
	}
	if !meAdmin.Authenticated || meAdmin.User.Role != "admin" || !meAdmin.DB.MandatorySetup {
		t.Fatalf("unexpected admin /auth/me: %+v", meAdmin)
	}

	// Admin creates an admin tunnel
	wAdminTun := admin.req(http.MethodPost, "/nfr/api/tunnels", map[string]any{
		"name":  "admin-ssh",
		"type":  "tcp",
		"local": "127.0.0.1:22",
	}, "")
	if wAdminTun.Code != http.StatusCreated {
		t.Fatalf("admin create tunnel: %d %s", wAdminTun.Code, wAdminTun.Body.String())
	}
	var adminTunResp struct {
		Tunnel struct {
			ID string `json:"id"`
		} `json:"tunnel"`
	}
	_ = json.Unmarshal(wAdminTun.Body.Bytes(), &adminTunResp)

	// Admin creates normal user alice
	wCreateAlice := admin.req(http.MethodPost, "/nfr/api/users", map[string]any{
		"username":         "alice",
		"password":         "alicepass123",
		"max_tunnels":      5,
		"max_bandwidth_kb": 1024,
		"monthly_quota_mb": 100,
	}, "")
	if wCreateAlice.Code != http.StatusCreated {
		t.Fatalf("create alice: %d %s", wCreateAlice.Code, wCreateAlice.Body.String())
	}
	var aliceResp struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	_ = json.Unmarshal(wCreateAlice.Body.Bytes(), &aliceResp)
	aliceID := aliceResp.User.ID
	if aliceID == "" {
		t.Fatalf("empty alice ID: %s", wCreateAlice.Body.String())
	}

	// Alice logs in
	alice := &panelClient{t: t, handler: p}
	if w := alice.login("alice", "alicepass123"); w.Code != http.StatusOK {
		t.Fatalf("alice login failed: %d %s", w.Code, w.Body.String())
	}

	// Alice /auth/me must have mandatory_setup = false
	wAliceMe := alice.req(http.MethodGet, "/nfr/api/auth/me", nil, "")
	var meAlice struct {
		User struct {
			Role string `json:"role"`
		} `json:"user"`
		DB struct {
			MandatorySetup bool `json:"mandatory_setup"`
		} `json:"db"`
	}
	_ = json.Unmarshal(wAliceMe.Body.Bytes(), &meAlice)
	if meAlice.User.Role != "user" || meAlice.DB.MandatorySetup {
		t.Fatalf("unexpected alice /auth/me: %+v", meAlice)
	}

	// Review Focus #5: Non-admin privilege escalation on direct API calls -> 403
	forbiddenRoutes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/nfr/api/settings"},
		{http.MethodPatch, "/nfr/api/settings"},
		{http.MethodGet, "/nfr/api/update"},
		{http.MethodPost, "/nfr/api/update/check"},
		{http.MethodGet, "/nfr/api/users"},
		{http.MethodPost, "/nfr/api/users"},
		{http.MethodGet, "/nfr/api/db"},
		{http.MethodPost, "/nfr/api/db/test"},
		{http.MethodGet, "/nfr/api/configs"},
		{http.MethodGet, "/nfr/api/api-keys"},
	}
	for _, rt := range forbiddenRoutes {
		w := alice.req(rt.method, rt.path, map[string]any{}, "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for alice on %s %s, got %d (%s)", rt.method, rt.path, w.Code, w.Body.String())
		}
	}

	// Alice creates her own tunnel
	wAliceTun := alice.req(http.MethodPost, "/nfr/api/tunnels", map[string]any{
		"name":  "alice-web",
		"type":  "tcp",
		"local": "127.0.0.1:8080",
	}, "")
	if wAliceTun.Code != http.StatusCreated {
		t.Fatalf("alice create tunnel: %d %s", wAliceTun.Code, wAliceTun.Body.String())
	}
	var aliceTunResp struct {
		Tunnel struct {
			ID string `json:"id"`
		} `json:"tunnel"`
	}
	_ = json.Unmarshal(wAliceTun.Body.Bytes(), &aliceTunResp)
	aliceTunID := aliceTunResp.Tunnel.ID

	// Strict isolation: Admin's own dashboard does NOT include Alice's tunnel; Alice's does NOT include Admin's
	wAdminDash := admin.req(http.MethodGet, "/nfr/api/dashboard", nil, "")
	if bytes.Contains(wAdminDash.Body.Bytes(), []byte(aliceTunID)) {
		t.Fatalf("admin dashboard leaked alice's tunnel: %s", wAdminDash.Body.String())
	}
	wAliceDash := alice.req(http.MethodGet, "/nfr/api/dashboard", nil, "")
	if bytes.Contains(wAliceDash.Body.Bytes(), []byte(adminTunResp.Tunnel.ID)) {
		t.Fatalf("alice dashboard leaked admin's tunnel: %s", wAliceDash.Body.String())
	}

	// Alice trying to delete or view config of admin's tunnel gets 404
	if w := alice.req(http.MethodDelete, "/nfr/api/tunnels/"+adminTunResp.Tunnel.ID, nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when alice deletes admin tunnel, got %d", w.Code)
	}
	if w := alice.req(http.MethodGet, "/nfr/api/tunnels/"+adminTunResp.Tunnel.ID+"/config", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when alice fetches admin tunnel config, got %d", w.Code)
	}

	// Admin viewing /nfr/api/users/{aliceID} sees Alice's tunnel and can toggle & delete it
	wDetail := admin.req(http.MethodGet, "/nfr/api/users/"+aliceID, nil, "")
	if wDetail.Code != http.StatusOK || !bytes.Contains(wDetail.Body.Bytes(), []byte(aliceTunID)) {
		t.Fatalf("admin user detail missing alice tunnel: %d %s", wDetail.Code, wDetail.Body.String())
	}
	if w := admin.req(http.MethodPost, "/nfr/api/users/"+aliceID+"/tunnels/"+aliceTunID+"/toggle", map[string]any{"enabled": false}, ""); w.Code != http.StatusOK {
		t.Fatalf("admin toggle alice tunnel failed: %d %s", w.Code, w.Body.String())
	}
	if w := admin.req(http.MethodDelete, "/nfr/api/users/"+aliceID+"/tunnels/"+aliceTunID, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("admin delete alice tunnel failed: %d %s", w.Code, w.Body.String())
	}

	// Review Focus #4: Avatar magic-byte validation & size limit & nosniff header
	svgPayload := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if w := alice.req(http.MethodPost, "/nfr/api/profile/avatar", svgPayload, "image/png"); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disguised SVG avatar, got %d (%s)", w.Code, w.Body.String())
	}
	// Oversized PNG (> 128 KiB)
	bigPNG := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 130*1024)...)
	if w := alice.req(http.MethodPost, "/nfr/api/profile/avatar", bigPNG, "image/png"); w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400/413 for oversized avatar, got %d", w.Code)
	}
	// Valid minimal 1x1 PNG
	validPNG := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 'I', 'D', 'A',
		'T', 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d,
		0xb0, 0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N',
		'D', 0xae, 0x42, 0x60, 0x82,
	}
	wUp := alice.req(http.MethodPost, "/nfr/api/profile/avatar", validPNG, "image/png")
	if wUp.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid PNG avatar, got %d (%s)", wUp.Code, wUp.Body.String())
	}
	wGet := alice.req(http.MethodGet, "/nfr/api/avatars/"+aliceID, nil, "")
	if wGet.Code != http.StatusOK {
		t.Fatalf("GET avatar failed: %d", wGet.Code)
	}
	if wGet.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("unexpected avatar Content-Type: %q", wGet.Header().Get("Content-Type"))
	}
	if wGet.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options: nosniff, got %q", wGet.Header().Get("X-Content-Type-Options"))
	}
}
