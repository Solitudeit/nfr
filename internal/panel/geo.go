package panel

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The console labels every instance by the country it sits in. "本机" only
// makes sense while there is one server; once an operator runs machines in
// several places the useful question is which country each one is in.
//
// Lookups go through a public geo IP service, are cached in memory for a day
// and stay best effort: a blocked, slow or failing lookup only leaves the
// chip off the card, it never breaks the page.

const (
	geoEndpoint  = "https://ipwho.is/"
	geoTTL       = 24 * time.Hour
	geoRetry     = time.Minute
	geoTimeout   = 2 * time.Second
	geoBudget    = 2200 * time.Millisecond
	geoCacheSize = 256
)

type geoEntry struct {
	country string
	code    string
	expires time.Time
}

// region is what the console shows on a card.
type region struct {
	Country string
	Code    string
}

// geoQuery asks about one instance. An empty host means "this machine";
// FallbackSelf keeps that behaviour when the known address turns out to be
// private, which is the usual case behind NAT or on a LAN only host name.
type geoQuery struct {
	Host         string
	FallbackSelf bool
}

// locator answers "which country is this address in". *geoResolver is the real
// one; the tests swap in a stub so the suite never touches the network.
type locator interface {
	locate(ctx context.Context, q geoQuery) region
}

type geoResolver struct {
	client   *http.Client
	endpoint string

	mu    sync.Mutex
	cache map[string]geoEntry
}

func newGeoResolver() *geoResolver {
	return &geoResolver{
		client:   &http.Client{Timeout: geoTimeout},
		endpoint: geoEndpoint,
		cache:    make(map[string]geoEntry, geoCacheSize),
	}
}

// locate returns the Chinese country name and its ISO code for host. An empty
// host asks for this machine's own public address, which is the right answer
// for the instance the console runs on, even behind NAT. Hosts that are
// private, unresolvable or unknown come back empty.
func (g *geoResolver) locate(ctx context.Context, q geoQuery) region {
	key, ip := g.target(ctx, q)
	if key == "" {
		return region{}
	}
	if e, ok := g.cached(key); ok {
		return region{Country: e.country, Code: e.code}
	}
	country, code, ok := g.query(ctx, ip)
	if !ok && ip != "" && q.FallbackSelf {
		// The name resolved to an address the service knows nothing about -
		// a split horizon resolver, a carrier NAT or a proxy answering for a
		// benchmark range. The instance is still somewhere, so ask about our
		// own exit instead of showing no country at all.
		key, ip = "self", ""
		if e, ok := g.cached(key); ok {
			return region{Country: e.country, Code: e.code}
		}
		country, code, ok = g.query(ctx, ip)
	}
	ttl := geoTTL
	if !ok {
		country, code = "", ""
		ttl = geoRetry
	}
	if !ok && ctx.Err() != nil {
		// Our own budget ran out rather than the service saying "unknown".
		// Remembering that would hide the label for a minute after one slow
		// packet, so let the next view try again.
		return region{}
	}
	g.store(key, country, code, ttl)
	return region{Country: country, Code: code}
}

// target turns whatever the console knows about an instance into a cache key
// and the address to ask about. An empty ip means "this machine".
func (g *geoResolver) target(ctx context.Context, q geoQuery) (key, ip string) {
	host := hostOf(q.Host)
	if host == "" {
		return "self", ""
	}
	if parsed := net.ParseIP(host); parsed != nil {
		if publicAddr(parsed) {
			return host, parsed.String()
		}
		// A private or loopback address says nothing about the country; the
		// instance itself is still somewhere, so fall back to our own exit.
		if q.FallbackSelf {
			return "self", ""
		}
		return "", ""
	}
	// A name is only useful if it resolves to a public address; an internal
	// host name says nothing about the country.
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		if q.FallbackSelf {
			return "self", ""
		}
		return "", ""
	}
	for _, a := range addrs {
		if publicAddr(a.IP) {
			return host, a.IP.String()
		}
	}
	if q.FallbackSelf {
		return "self", ""
	}
	return "", ""
}

func (g *geoResolver) query(ctx context.Context, ip string) (string, string, bool) {
	target := g.endpoint
	if ip != "" {
		target += url.PathEscape(ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?lang=zh-CN", nil)
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var body struct {
		Success     bool   `json:"success"`
		Country     string `json:"country"`
		CountryCode string `json:"country_code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&body); err != nil {
		return "", "", false
	}
	if !body.Success || body.Country == "" {
		return "", "", false
	}
	return body.Country, strings.ToUpper(body.CountryCode), true
}

func (g *geoResolver) cached(key string) (geoEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.cache[key]
	if !ok || time.Now().After(e.expires) {
		return geoEntry{}, false
	}
	return e, true
}

func (g *geoResolver) store(key, country, code string, ttl time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.cache) >= geoCacheSize {
		for k := range g.cache { // the map is tiny; any entry will do
			delete(g.cache, k)
			break
		}
	}
	g.cache[key] = geoEntry{country: country, code: code, expires: time.Now().Add(ttl)}
}

// hostOf pulls the bare host out of a URL, host:port pair or plain host.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// publicAddr reports whether ip is routed on the public internet. Private,
// loopback, link local and carrier grade NAT answers have no country.
func publicAddr(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false // 100.64.0.0/10, behind carrier NAT
		}
		ip = ip4
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(),
		ip.IsMulticast(), ip.IsUnspecified():
		return false
	}
	return true
}
