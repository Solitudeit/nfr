package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestLoginLandsOnTunnels(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// The overview page is gone: the console root and /nfr/overview both send
	// the operator to this instance's tunnels.
	rec := do(p, http.MethodGet, "/nfr/", nil, cookies)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/nfr/tunnels?server=local" {
		t.Fatalf("root = %d %s, want 303 to the local tunnels", rec.Code, rec.Header().Get("Location"))
	}
	if rec = do(p, http.MethodGet, "/nfr/overview", nil, cookies); rec.Code != http.StatusSeeOther {
		t.Fatalf("overview = %d, want 303", rec.Code)
	}

	rec = do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnels = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "example.com") {
		t.Fatalf("tunnels does not show the instance: %s", body[:200])
	}
	// The session is required on every page, not just the tunnels.
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

func TestSettingsExposeThePublicAddress(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	page := do(p, http.MethodGet, "/nfr/settings", nil, cookies)
	if page.Code != http.StatusOK {
		t.Fatalf("settings page = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `name="public_host"`) {
		t.Fatal("settings page must offer a public address field")
	}

	// Saving it round-trips, and the other groups' hidden fields must not
	// wipe it out when they post.
	token := extractCSRF(t, body)
	form := url.Values{
		"csrf": {token}, "name": {"NFR"}, "domain": {""},
		"public_host": {"203.0.113.7"}, "port_low": {"20000"}, "port_high": {"29999"},
		"email": {""}, "api_enabled": {"1"},
	}
	if rec := do(p, http.MethodPost, "/nfr/settings", form, cookies); rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("save settings = %d", rec.Code)
	}
	after := do(p, http.MethodGet, "/nfr/settings", nil, cookies).Body.String()
	if !strings.Contains(after, `value="203.0.113.7"`) {
		t.Error("public address did not persist")
	}
}

func TestFormSwitchReadsCheckboxesAndBooleans(t *testing.T) {
	// Browsers post "1" when ticked and nothing when not; scripts post
	// "true"/"false". Only the empty-ish values may mean off, otherwise a
	// caller asking for "false" would silently turn a tunnel on.
	for _, on := range []string{"1", "on", "true", "TRUE", "yes"} {
		if !formSwitch(on) {
			t.Errorf("formSwitch(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"", "0", "false", "off", "no", "  "} {
		if formSwitch(off) {
			t.Errorf("formSwitch(%q) = true, want false", off)
		}
	}
}

func TestSingleInstanceSkipsTheServerPicker(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// Federated instances are an advanced feature. With none configured the
	// tunnel page belongs to this instance, so asking which server to manage
	// makes no sense.
	rec := do(p, http.MethodGet, "/nfr/tunnels", nil, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("tunnels = %d, want a redirect to this instance", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/nfr/tunnels?server=local" {
		t.Fatalf("Location = %q", got)
	}
	body := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies).Body.String()
	if strings.Contains(body, "选择一台服务器") {
		t.Fatal("the tunnel list still offers server selection")
	}
}

func TestTunnelsCarriesStatsAndFourNavItems(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// The tunnels page absorbed the overview's stats block, and the nav
	// dropped the overview entry.
	rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnels = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-stat="nodes-online"`, `data-stat="tunnels-enabled"`, `data-stat="total-in"`, `data-stat="total-out"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("tunnels page is missing the stat %s", want)
		}
	}
	if strings.Contains(body, "概览") {
		t.Fatal("the nav still offers the overview page")
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

func TestNodesNeverOfferConsoleHost(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	for _, path := range []string{"/nfr/nodes", "/nfr/tunnels?server=local", "/nfr/tunnels/new?server=local"} {
		rec := do(p, http.MethodGet, path, nil, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, `data-id="local"`) || strings.Contains(body, `data-nid="local"`) || strings.Contains(body, `data-local-node=`) {
			t.Fatalf("%s offers the console host as a target", path)
		}
	}
}

func TestTunnelFormOffersNewDevice(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("form status = %d", rec.Code)
	}
	for _, want := range []string{"data-new-device", "＋ 新设备", `<details class="adv"`, "留空自动生成"} {
		if !strings.Contains(body, want) {
			t.Fatalf("new-device form is missing %q", want)
		}
	}
	// The manual grab button must ship hidden (the [hidden] attribute, which
	// app.css turns into display:none — a bare class would not). grab() never
	// clears the attribute, and the details-open auto-grab calls it without
	// un-hiding, so the button stays invisible unless markup changes.
	m := regexp.MustCompile(`<button[^>]*class="[^"]*grab-btn[^"]*"[^>]*>`)
	btn := m.FindString(body)
	if btn == "" {
		t.Fatal("no grab button rendered in the staged form")
	}
	if !strings.Contains(btn, " hidden") || strings.Contains(btn, `class="[^"]*hidden`) {
		t.Fatalf("grab button must hide via the hidden attribute, got: %s", btn)
	}
	if strings.Contains(btn, `class="btn grab-btn hidden"`) {
		t.Fatalf("grab button must not rely on the non-existent .hidden class: %s", btn)
	}
	for _, gone := range []string{"192.168.1.1", "combo-list"} {
		if strings.Contains(body, gone) {
			t.Fatalf("staged form still contains fake-data leftover %q", gone)
		}
	}
}

// newDeviceTunnel posts the new-device form and returns the created awaiting
// tunnel id and its join code as rendered on the tunnels page.
func newDeviceTunnel(t *testing.T, p *Panel, cookies []*http.Cookie) (string, string) {
	t.Helper()
	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	csrf := extractCSRF(t, rec.Body.String())

	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"new"},
		"type":       {"tcp"},
		"local_host": {"127.0.0.1"},
		"local_port": {"8080"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/nfr/tunnels?server=local#t-") {
		t.Fatalf("save Location = %q, want the tunnel card anchor", loc)
	}
	id := strings.TrimPrefix(loc, "/nfr/tunnels?server=local#t-")

	rec = do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	body := rec.Body.String()
	if !strings.Contains(body, "等待接入") {
		t.Fatal("the awaiting tunnel does not show the 等待接入 status")
	}
	if !strings.Contains(body, `id="t-`+id+`"`) {
		t.Fatal("the tunnel card has no anchor id for the redirect")
	}
	if !strings.Contains(body, "nfrc join ") {
		t.Fatal("the awaiting card does not show the copy-pasteable join command")
	}
	if !strings.Contains(body, "data-expires") {
		t.Fatal("the awaiting card does not carry a countdown expiry")
	}
	if !strings.Contains(body, "/nfr/tunnels/regen-code") {
		t.Fatal("the awaiting card has no regenerate button")
	}
	start := strings.Index(body, `class="join-code">`)
	if start < 0 {
		t.Fatal("no join-code span rendered on the tunnels page")
	}
	rest := body[start+len(`class="join-code">`):]
	code := rest[:strings.Index(rest, "</span>")]
	return id, code
}

func TestTunnelSaveNewDeviceCreatesAwaiting(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)
	id, code := newDeviceTunnel(t, p, cookies)
	if id == "" || code == "" {
		t.Fatal("no awaiting tunnel or code was produced")
	}
}

// The waiting card has to tell the operator how to bring the client back
// after a reboot: the join code is spent once redeemed, so the restart path
// is the saved agent.toml, not the join command.
func TestAwaitingCardShowsConfigRestartPath(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)
	newDeviceTunnel(t, p, cookies)

	rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	body := rec.Body.String()
	if !strings.Contains(body, "nfrc -c") {
		t.Fatal("the awaiting card never mentions starting the client from its saved config")
	}
	if !strings.Contains(body, "agent.toml") {
		t.Fatal("the awaiting card does not name the config file the client restarts from")
	}
}

// The failure that actually happens in the field: the device joined, the
// tunnel went live, then the client died. Re-running `nfrc join` is the wrong
// repair (it registers a second node), so the card of a tunnel whose node is
// offline must point at the saved config instead.
func TestOfflineTunnelPointsAtConfigRestart(t *testing.T) {
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
	// A node that has joined before but has no live session: the seeded
	// server never registers an agent, so this node reads as offline.
	if err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes, store.Node{ID: "gone", Name: "edge-1"})
		s.Tunnels = append(s.Tunnels, store.Tunnel{
			ID: "offline1", Name: "offline-tunnel", NodeID: "gone",
			Type: "tcp", Local: "127.0.0.1:8080", RemotePort: 20011, Enabled: true,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	body := rec.Body.String()
	if !strings.Contains(body, "节点离线") {
		t.Fatal("the offline tunnel is not reported as 节点离线")
	}
	card := body[strings.Index(body, `id="t-offline1"`):]
	card = card[:strings.Index(card, "</article>")]
	if !strings.Contains(card, "nfrc -c") {
		t.Fatal("the offline card never says how to bring the client back")
	}
	if !strings.Contains(card, "不要重新 join") {
		t.Fatal("the offline card does not warn against re-running join")
	}
}

// A disabled or awaiting tunnel must not carry the offline advice: the hint is
// a repair for an enabled tunnel whose node is offline.
func TestOfflineHintStaysOnOfflineCards(t *testing.T) {
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
	if err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes, store.Node{ID: "edge1", Name: "edge-1"})
		s.Tunnels = append(s.Tunnels, store.Tunnel{
			ID: "disabled1", Name: "disabled-tunnel", NodeID: "edge1",
			Type: "tcp", Local: "127.0.0.1:8080", RemotePort: 20012, Enabled: false,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if body := rec.Body.String(); strings.Contains(body, "不要重新 join") {
		t.Fatal("a disabled tunnel carries the offline restart hint")
	}
}

func TestRegenCodeRoute(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)
	id, code := newDeviceTunnel(t, p, cookies)

	csrf := extractCSRF(t, func() string {
		rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
		return rec.Body.String()
	}())
	rec := do(p, http.MethodPost, "/nfr/tunnels/regen-code", url.Values{
		"csrf":   {csrf},
		"server": {"local"},
		"id":     {id},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("regen-code status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("regen-code failed: %s", rec.Header().Get("Location"))
	}

	rec = do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if strings.Contains(rec.Body.String(), `class="join-code">`+code+`</span>`) {
		t.Fatal("the tunnels page still shows the old code after regeneration")
	}
	// A fresh code must be present in its place.
	body := rec.Body.String()
	start := strings.Index(body, `class="join-code">`)
	if start < 0 {
		t.Fatal("no join-code rendered after regeneration")
	}
	rest := body[start+len(`class="join-code">`):]
	fresh := rest[:strings.Index(rest, "</span>")]
	if fresh == code {
		t.Fatal("regeneration returned the same code")
	}
}

func TestRegenCodeAlreadyJoinedConflict(t *testing.T) {
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
	if err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes, store.Node{ID: "n1", Name: "edge-1"})
		return nil
	}); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	csrf := extractCSRF(t, rec.Body.String())
	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"n1"},
		"name":       {"joined"},
		"type":       {"tcp"},
		"local_host": {"127.0.0.1"},
		"local_port": {"8081"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body: %s", rec.Code, rec.Body.String())
	}

	rec = do(p, http.MethodPost, "/nfr/tunnels/regen-code", url.Values{
		"csrf":   {csrf},
		"server": {"local"},
		"id":     {"joined"},
	}, cookies)
	// "joined" is the tunnel name; the regen route keys on the tunnel id, so
	// first resolve the real id from the tunnels page.
	rec = do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	body := rec.Body.String()
	idx := strings.Index(body, `data-tid="`)
	if idx < 0 {
		t.Fatal("joined tunnel not found on the tunnels page")
	}
	rest := body[idx+len(`data-tid="`):]
	id := rest[:strings.Index(rest, `"`)]

	csrf = extractCSRF(t, body)
	rec = do(p, http.MethodPost, "/nfr/tunnels/regen-code", url.Values{
		"csrf":   {csrf},
		"server": {"local"},
		"id":     {id},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("regen-code status = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u, perr := url.Parse(loc)
	if perr != nil {
		t.Fatalf("regen-code Location = %q: %v", loc, perr)
	}
	if u.Query().Get("err") != "this tunnel already has a node" {
		t.Fatalf("regen-code Location = %q, want an already_joined error", loc)
	}
}

func TestRemoteFormHidesNewDevice(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// /nfr/tunnels (no server param) with no configured remote redirects to
	// the local form surface; the form itself defaults to server=local.
	rec := do(p, http.MethodGet, "/nfr/tunnels", nil, cookies)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "server=local") {
		t.Fatalf("no remotes: expect redirect to the local view, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	if !strings.Contains(rec.Body.String(), "data-new-device") {
		t.Fatal("local form must offer the new-device card")
	}
}

func TestHiddenAttributeBeatsComponentDisplay(t *testing.T) {
	css, err := files.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	// .flash and .btn both set an explicit display, which beats the UA
	// stylesheet's [hidden] rule. Without a global override the empty
	// #nodewarn strip and the manual grab button render on the tunnel form.
	if !regexp.MustCompile(`(?m)^\s*\[hidden\]\s*\{[^}]*display:\s*none\s*!important`).Match(css) {
		t.Fatal(`app.css must carry a global "[hidden] { display: none !important }" rule`)
	}
}

// The parked first-run password exists only so the operator can find it once.
// A successful sign-in means it has been found, so the plaintext copy must not
// outlive that moment.
func TestLoginClearsTheParkedPassword(t *testing.T) {
	p, password := newPanel(t)
	parked := server.InitialPasswordPath(p.srv.DataDir())
	if err := os.WriteFile(parked, []byte("  密码 "+password+"\n"), 0o600); err != nil {
		t.Fatalf("park password: %v", err)
	}

	login(t, p, password)

	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Fatalf("the parked password survived a successful sign-in: %v", err)
	}
}

// A wrong password must leave the file alone: the operator still has to be
// able to read it.
func TestFailedLoginKeepsTheParkedPassword(t *testing.T) {
	p, password := newPanel(t)
	parked := server.InitialPasswordPath(p.srv.DataDir())
	if err := os.WriteFile(parked, []byte("  密码 "+password+"\n"), 0o600); err != nil {
		t.Fatalf("park password: %v", err)
	}

	do(p, http.MethodPost, "/nfr/login", url.Values{"password": {"not-it"}}, nil)

	if _, err := os.Stat(parked); err != nil {
		t.Fatalf("a failed sign-in deleted the parked password: %v", err)
	}
}

func TestSettingsFormsDoNotClobberEachOther(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	page := do(p, http.MethodGet, "/nfr/settings", nil, cookies)
	csrf := extractCSRF(t, page.Body.String())

	// Enable REST API via its dedicated action.
	rec := do(p, http.MethodPost, "/nfr/settings", url.Values{
		"csrf":        {csrf},
		"action":      {"api"},
		"api_enabled": {"1"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable api status = %d", rec.Code)
	}
	if !p.srv.Settings().APIEnabled {
		t.Fatal("APIEnabled did not turn on")
	}

	// Saving instance settings must NOT turn off APIEnabled.
	rec = do(p, http.MethodPost, "/nfr/settings", url.Values{
		"csrf":        {csrf},
		"action":      {"instance"},
		"name":        {"EdgeHub"},
		"domain":      {"hub.example.com"},
		"public_host": {"203.0.113.9"},
		"port_low":    {"21000"},
		"port_high":   {"22000"},
		"email":       {"ops@example.com"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("save instance status = %d", rec.Code)
	}
	st := p.srv.Settings()
	if !st.APIEnabled {
		t.Fatal("saving instance settings silently disabled REST API")
	}
	if st.Domain != "hub.example.com" || st.PublicHost != "203.0.113.9" || st.PortRange != [2]int{21000, 22000} {
		t.Fatalf("instance settings lost: %+v", st)
	}

	// Toggling REST API off must NOT wipe instance settings.
	rec = do(p, http.MethodPost, "/nfr/settings", url.Values{
		"csrf":   {csrf},
		"action": {"api"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable api status = %d", rec.Code)
	}
	st = p.srv.Settings()
	if st.APIEnabled {
		t.Fatal("REST API did not turn off")
	}
	if st.Domain != "hub.example.com" || st.PublicHost != "203.0.113.9" {
		t.Fatalf("toggling API clobbered instance settings: %+v", st)
	}
}

func TestEditHTTPTunnelPrefillsHosts(t *testing.T) {
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
	if err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes, store.Node{ID: "n1", Name: "edge-1"})
		s.Tunnels = append(s.Tunnels, store.Tunnel{
			ID: "h1", Name: "blog", NodeID: "n1", Type: "http",
			Local: "127.0.0.1:8080", Hosts: []string{"a.example.com", "b.example.com"}, Enabled: true,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	cookies := login(t, p, password)

	rec := do(p, http.MethodGet, "/nfr/tunnels/edit?id=h1&server=local", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "a.example.com, b.example.com") {
		t.Fatalf("edit form did not prefill hosts textarea")
	}
}

func TestGeoResolverNeverBlocksRender(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"country":"新加坡","country_code":"SG"}`)
	}))
	defer slow.Close()

	g := newGeoResolver()
	g.endpoint = slow.URL + "/"

	start := time.Now()
	first := g.locate(context.Background(), geoQuery{Host: "1.1.1.1"})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("locate blocked caller for %v", elapsed)
	}
	if first.Country != "" {
		t.Fatalf("expected empty cache-miss result, got %+v", first)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := g.locate(context.Background(), geoQuery{Host: "1.1.1.1"})
		if got.Country == "新加坡" && got.Code == "SG" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("background geo refresh never populated cache")
}

func TestTunnelSaveAutoNameAndDomainlessHTTPAndNodeSwitch(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultServer()
	cfg.DataDir = dir
	cfg.ControlListen = "off"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.Domain = "" // bare-IP server with no domain configured
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Update(func(s *store.State) error {
		s.Nodes = append(s.Nodes,
			store.Node{ID: "n1", Name: "edge-1"},
			store.Node{ID: "n2", Name: "edge-2"},
		)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv, password, err := server.New(server.Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	p := New(srv, log.New("error", ""))
	p.geo = stubLocator{}
	cookies := login(t, p, password)

	page := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	body := page.Body.String()
	if !strings.Contains(body, `value="127.0.0.1"`) {
		t.Fatal("new tunnel form should prefill local_host with 127.0.0.1")
	}
	csrf := extractCSRF(t, body)

	// 1) Creating an HTTP tunnel with blank name, blank hosts, blank port on a
	// domainless server must succeed by auto-naming and auto-allocating a port.
	rec := do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"n1"},
		"type":       {"http"},
		"local_host": {"127.0.0.1"},
		"local_port": {"8080"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("domainless http auto-save = %d, body: %s", rec.Code, rec.Body.String())
	}
	tunnels := srv.Tunnels()
	if len(tunnels) != 1 || tunnels[0].Name != "http-8080" || tunnels[0].RemotePort == 0 {
		t.Fatalf("unexpected auto-created tunnel: %+v", tunnels)
	}

	// 2) Validation error re-renders the form in place preserving typed values.
	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"n1"},
		"name":       {"http-8080"}, // duplicate name
		"type":       {"tcp"},
		"local_host": {"10.0.0.5"},
		"local_port": {"9999"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected in-place re-render on error, got %d", rec.Code)
	}
	errBody := rec.Body.String()
	if !strings.Contains(errBody, "已有同名隧道") || !strings.Contains(errBody, "10.0.0.5") || !strings.Contains(errBody, "9999") {
		t.Fatalf("error form did not preserve user input or friendly message")
	}

	// 3) Edit form allows migrating the tunnel to another node (n1 -> n2).
	editRec := do(p, http.MethodGet, "/nfr/tunnels/edit?id="+tunnels[0].ID+"&server=local", nil, cookies)
	if !strings.Contains(editRec.Body.String(), `data-id="n2"`) {
		t.Fatal("edit form must offer other nodes for migration")
	}
	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"id":         {tunnels[0].ID},
		"node_id":    {"n2"},
		"name":       {tunnels[0].Name},
		"type":       {"http"},
		"local_host": {"127.0.0.1"},
		"local_port": {"8080"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("migrate node save = %d", rec.Code)
	}
	updated, ok := srv.Tunnel(tunnels[0].ID)
	if !ok || updated.NodeID != "n2" {
		t.Fatalf("tunnel did not migrate to n2: %+v", updated)
	}
}

func TestPanelEventsEndpointAndStateEvents(t *testing.T) {
	p, password := newPanel(t)
	// Anonymous access to /nfr/api/events must return 401.
	if rec := do(p, http.MethodGet, "/nfr/api/events", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /nfr/api/events = %d, want 401", rec.Code)
	}
	cookies := login(t, p, password)
	// Trigger a change event by rotating auth_token via settings.
	page := do(p, http.MethodGet, "/nfr/settings", nil, cookies)
	csrf := extractCSRF(t, page.Body.String())
	if rec := do(p, http.MethodPost, "/nfr/settings", url.Values{
		"csrf":       {csrf},
		"action":     {"auth_token"},
		"auth_token": {"custom-token-999"},
	}, cookies); rec.Code != http.StatusOK {
		t.Fatalf("update auth_token = %d", rec.Code)
	}
	if p.srv.AuthToken() != "custom-token-999" {
		t.Fatalf("AuthToken = %q, want custom-token-999", p.srv.AuthToken())
	}
	stRec := do(p, http.MethodGet, "/nfr/api/state", nil, cookies)
	if stRec.Code != http.StatusOK || !strings.Contains(stRec.Body.String(), "auth token updated") {
		t.Fatalf("/nfr/api/state did not include recent events: %s", stRec.Body.String())
	}
}
