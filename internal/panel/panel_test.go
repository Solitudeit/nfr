package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	if st := p.srv.Settings(); st.LocalNode {
		t.Error("local_node should stay off unless explicitly ticked")
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

func TestTunnelsStatsCountsFullNodePopulation(t *testing.T) {
	p, password := newPanel(t)
	if err := p.srv.SetLocalNode(true); err != nil {
		t.Fatal(err)
	}
	cookies := login(t, p, password)

	// The stats block must count the same population the JS repaint uses:
	// every node in p.srv.Nodes(), console host included — not just the
	// agent nodes splitNodes hands the node list. With only the console
	// host present, that is one node, online (api.Node marks Local always
	// online), out of one.
	rec := do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnels = %d", rec.Code)
	}
	want := `data-stat="nodes-online">1 / 1<`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("tunnels stats missing full population %q in body: %.400s", want, rec.Body.String())
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

func TestNodesHideTheConsoleHostByDefault(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	// An inner address belongs to the machine that joined. Until the operator
	// opts in, the console host must not be offered as a tunnel target, and it
	// must never be labelled in a way that suggests it is a normal agent.
	for _, path := range []string{"/nfr/nodes", "/nfr/tunnels?server=local", "/nfr/tunnels/new?server=local"} {
		rec := do(p, http.MethodGet, path, nil, cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, `data-id="local"`) || strings.Contains(body, `data-nid="local"`) {
			t.Fatalf("%s offers the console host as a target", path)
		}
	}
}

func TestTunnelFormOffersTheConsoleHostWhenEnabled(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)

	if err := p.srv.SetLocalNode(true); err != nil {
		t.Fatalf("enable local node: %v", err)
	}
	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	body := rec.Body.String()
	if !strings.Contains(body, `data-local-node="local"`) {
		t.Fatal("the console host is missing from the form once it is enabled")
	}
	if !strings.Contains(body, "服务端本机") {
		t.Fatal("the console host is offered without the badge that separates it from agents")
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

// A healthy tunnel must not carry the offline advice: the hint is a repair for
// a node that is gone, and showing it everywhere would turn it into noise.
func TestOfflineHintStaysOnOfflineCards(t *testing.T) {
	p, password := newPanel(t)
	cookies := login(t, p, password)
	if err := p.srv.SetLocalNode(true); err != nil {
		t.Fatalf("enable local node: %v", err)
	}
	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	csrf := extractCSRF(t, rec.Body.String())
	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"local"},
		"name":       {"healthy"},
		"type":       {"tcp"},
		"local_host": {"127.0.0.1"},
		"local_port": {"8080"},
		"enabled":    {"1"},
	}, cookies)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d", rec.Code)
	}

	rec = do(p, http.MethodGet, "/nfr/tunnels?server=local", nil, cookies)
	if body := rec.Body.String(); strings.Contains(body, "不要重新 join") {
		t.Fatal("a healthy tunnel carries the offline restart hint")
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
	p, password := newPanel(t)
	cookies := login(t, p, password)

	if err := p.srv.SetLocalNode(true); err != nil {
		t.Fatalf("enable local node: %v", err)
	}
	// A joined tunnel: node_id=local goes through the ordinary create path.
	rec := do(p, http.MethodGet, "/nfr/tunnels/new?server=local", nil, cookies)
	csrf := extractCSRF(t, rec.Body.String())
	rec = do(p, http.MethodPost, "/nfr/tunnels/save", url.Values{
		"csrf":       {csrf},
		"server":     {"local"},
		"node_id":    {"local"},
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
