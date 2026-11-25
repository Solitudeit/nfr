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

// The console labels every instance by the country it sits in. Lookups go
// through a public geo IP service in the background and are cached in memory
// for a day: locate() never blocks page rendering, so offline or slow networks
// add zero latency to the UI.

const (
	geoEndpoint  = "https://ipwho.is/"
	geoTTL       = 24 * time.Hour
	geoRetry     = 5 * time.Minute
	geoTimeout   = 2 * time.Second
	geoBudget    = 200 * time.Millisecond
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
	sem      chan struct{}

	mu       sync.Mutex
	cache    map[string]geoEntry
	inflight map[string]bool
}

func newGeoResolver() *geoResolver {
	return &geoResolver{
		client:   &http.Client{Timeout: geoTimeout},
		endpoint: geoEndpoint,
		sem:      make(chan struct{}, 4),
		cache:    make(map[string]geoEntry, geoCacheSize),
		inflight: make(map[string]bool),
	}
}

// locate returns the cached Chinese country name and ISO code immediately
// without blocking on DNS or external HTTP calls. On a cache miss or expiry it
// triggers a bounded background refresh so subsequent views carry the label.
func (g *geoResolver) locate(_ context.Context, q geoQuery) region {
	key, valid := g.cacheKey(q)
	if !valid {
		return region{}
	}
	now := time.Now()
	g.mu.Lock()
	e, ok := g.cache[key]
	fresh := ok && now.Before(e.expires)
	if fresh {
		g.mu.Unlock()
		return region{Country: e.country, Code: e.code}
	}
	shouldFetch := !g.inflight[key]
	if shouldFetch {
		g.inflight[key] = true
	}
	g.mu.Unlock()

	if shouldFetch {
		go g.refresh(key, q)
	}
	if ok {
		return region{Country: e.country, Code: e.code}
	}
	return region{}
}

func (g *geoResolver) cacheKey(q geoQuery) (string, bool) {
	host := hostOf(q.Host)
	if host == "" {
		if q.FallbackSelf {
			return "self", true
		}
		return "", false
	}
	if parsed := net.ParseIP(host); parsed != nil {
		if publicAddr(parsed) {
			return parsed.String(), true
		}
		if q.FallbackSelf {
			return "self", true
		}
		return "", false
	}
	return host, true
}

func (g *geoResolver) refresh(key string, q geoQuery) {
	defer func() {
		g.mu.Lock()
		delete(g.inflight, key)
		g.mu.Unlock()
	}()
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), geoTimeout)
	defer cancel()

	ip := g.resolveIP(ctx, key, q)
	if ip == "" && key != "self" && !q.FallbackSelf {
		g.store(key, "", "", geoRetry)
		return
	}
	country, code, ok := g.query(ctx, ip)
	if !ok && ip != "" && q.FallbackSelf {
		country, code, ok = g.query(ctx, "")
	}
	ttl := geoTTL
	if !ok {
		country, code = "", ""
		ttl = geoRetry
	}
	g.store(key, country, code, ttl)
}

func (g *geoResolver) resolveIP(ctx context.Context, key string, q geoQuery) string {
	if key == "self" {
		return ""
	}
	if parsed := net.ParseIP(key); parsed != nil {
		if publicAddr(parsed) {
			return parsed.String()
		}
		return ""
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, key)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if publicAddr(a.IP) {
			return a.IP.String()
		}
	}
	return ""
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

func (g *geoResolver) store(key, country, code string, ttl time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.cache) >= geoCacheSize {
		for k := range g.cache {
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
