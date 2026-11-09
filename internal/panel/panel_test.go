package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/store"
)

func newPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.Domain = "example.com"
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	// The suite must never depend on a geo IP service being reachable.
	p.geo = stubLocator{}
	return p, password
}

// stubLocator answers every address with the same country, so tests can assert
// the chip without touching the network.
type stubLocator struct{}

func (stubLocator) locate(context.Context, geoQuery) region {
	return region{Country: "香港", Code: "HK"}
}

func do(p *Panel, method, path string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, p *Panel, password string) []*http.Cookie {
	t.Helper()
	rec := do(p, http.MethodPost, "/nfr/login", url.Values{"password": {password}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure cookie: %+v", c)
	}
	return cookies
}

func TestRequiresLogin(t *testing.T) {
	p, _ := newPanel(t)
	rec := do(p, http.MethodGet, "/nfr/", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/nfr/login" {
		t.Fatalf("unauthenticated request = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Login page itself is public.
	if rec := do(p, http.MethodGet, "/nfr/login", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("login page = %d", rec.Code)
	}
}

func TestLoginAndOverview(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "example.com") {
		t.Fatalf("overview does not show the instance: %s", body[:200])
	}
	// The session is required on every page, not just the overview.
	if rec := do(p, http.MethodGet, "/nfr/tunnels", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("tunnels without a session = %d", rec.Code)
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	p, _ := newPanel(t)
	rec := do(p, http.MethodPost, "/nfr/login", url.Values{"password": {"nope"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "wrong") {
		t.Fatalf("redirect = %q", loc)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a failed login must not set a session cookie")
	}
}

func TestCSRFRequired(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)
	rec := do(p, http.MethodPost, "/nfr/settings", url.Values{
		"action": {"general"}, "name": {"renamed"},
	}, cookies)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("post without a csrf token = %d, want 403", rec.Code)
	}
}

func TestCreateJoinCode(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// The nodes page carries the csrf token and minting needs it.
	page := do(p, http.MethodGet, "/nfr/nodes", nil, cookies)
	if page.Code != http.StatusOK {
		t.Fatalf("nodes page = %d", page.Code)
	}
	token := extractCSRF(t, page.Body.String())

	rec := do(p, http.MethodPost, "/nfr/nodes", url.Values{"csrf": {token}}, cookies)
	if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("mint join code = %d", rec.Code)
	}
}

func TestTunnelsAlwaysShowsServerCards(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// A single instance still gets a card: the card is where the country and
	// the tunnel counts live, so skipping it hid both.
	rec := do(p, http.MethodGet, "/nfr/tunnels", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnels = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/nfr/tunnels?server=local"`) {
		t.Fatal("tunnels does not link the local instance card")
	}
	if !strings.Contains(body, "香港") {
		t.Fatal("server card does not carry the region label")
	}
	if strings.Contains(body, ">本机<") {
		t.Fatal("the console should name the country, not call the instance local")
	}
}

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no csrf token on page: %s", body[:min(200, len(body))])
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("malformed csrf token")
	}
	return rest[:j]
}

func TestNodesShowCountryNotThisHost(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// The built-in node is a node like any other on screen: it has to be
	// selectable and labelled by its country, never by the words "this host".
	for _, path := range []string{"/nfr/nodes", "/nfr/tunnels?server=local", "/nfr/tunnels/new?server=local"} {
		rec := do(p, http.MethodGet, path, nil, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "香港") {
			t.Fatalf("%s carries no country label for the built-in node", path)
		}
		if strings.Contains(body, "本机") {
			t.Fatalf("%s still calls the instance local", path)
		}
	}
}

func TestTunnelFormOffersTheBuiltInNode(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	body := rec.Body.String()
	if !strings.Contains(body, `data-id="local"`) {
		t.Fatal("the built-in node is missing from the node picker")
	}
}
