package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeadersAndOriginCheck(t *testing.T) {
	_, p, pw := newTestMultiUserPanel(t)

	// 1. SPA and API responses include anti-clickjacking & MIME-sniffing headers
	for _, path := range []string{"/nfr/", "/nfr/api/auth/me"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "127.0.0.1:43210"
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)

		if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s X-Frame-Options = %q, want DENY", path, got)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Fatalf("%s CSP = %q, want frame-ancestors 'none'", path, got)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Fatalf("%s Referrer-Policy = %q, want same-origin", path, got)
		}
		if got := w.Header().Get("Strict-Transport-Security"); got == "" {
			t.Fatalf("%s missing HSTS header behind trusted HTTPS reverse proxy", path)
		}
	}

	// Non-loopback plain HTTP spoofing X-Forwarded-Proto must NOT receive HSTS
	rSpoof := httptest.NewRequest(http.MethodGet, "/nfr/api/auth/me", nil)
	rSpoof.RemoteAddr = "198.51.100.20:43210"
	rSpoof.Header.Set("X-Forwarded-Proto", "https")
	wSpoof := httptest.NewRecorder()
	p.ServeHTTP(wSpoof, rSpoof)
	if got := wSpoof.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("unexpected HSTS when non-loopback spoofed X-Forwarded-Proto: %q", got)
	}

	// 2. Cross-origin POST is rejected with 403
	body, _ := json.Marshal(map[string]any{"username": "admin", "password": pw})
	rBadOrig := httptest.NewRequest(http.MethodPost, "https://nfr.example.com/nfr/api/auth/login", bytes.NewReader(body))
	rBadOrig.Header.Set("Content-Type", "application/json")
	rBadOrig.Header.Set("Origin", "https://evil.example.org")
	wBadOrig := httptest.NewRecorder()
	p.ServeHTTP(wBadOrig, rBadOrig)
	if wBadOrig.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login status = %d, want 403", wBadOrig.Code)
	}
}

func TestReverseProxyRateLimitIsolatesRealIPs(t *testing.T) {
	_, p, pw := newTestMultiUserPanel(t)

	doLogin := func(remoteAddr, xRealIP, xff, password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"username": "admin", "password": password})
		r := httptest.NewRequest(http.MethodPost, "/nfr/api/auth/login", bytes.NewReader(body))
		r.RemoteAddr = remoteAddr
		r.Header.Set("Content-Type", "application/json")
		if xRealIP != "" {
			r.Header.Set("X-Real-IP", xRealIP)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	// Attacker behind local nginx fails 5 times -> attacker's real IP gets 429 (2 min)
	for i := 0; i < maxFails; i++ {
		w := doLogin("127.0.0.1:50000", "203.0.113.9", "", "wrong-pass")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, w.Code)
		}
	}
	wLocked := doLogin("127.0.0.1:50001", "203.0.113.9", "", pw)
	if wLocked.Code != http.StatusTooManyRequests {
		t.Fatalf("expected attacker IP to be locked (429), got %d", wLocked.Code)
	}
	var lockResp struct {
		RetryAfter int `json:"retry_after_seconds"`
	}
	_ = json.Unmarshal(wLocked.Body.Bytes(), &lockResp)
	if lockResp.RetryAfter <= 0 || lockResp.RetryAfter > int(shortLockFor.Seconds())+1 {
		t.Fatalf("expected short progressive lock (~120s), got %ds", lockResp.RetryAfter)
	}

	// Legitimate admin from a DIFFERENT real IP through the SAME local nginx MUST still succeed!
	wAdmin := doLogin("127.0.0.1:50002", "198.51.100.77", "", pw)
	if wAdmin.Code != http.StatusOK {
		t.Fatalf("admin from distinct real IP behind nginx was locked out: %d %s", wAdmin.Code, wAdmin.Body.String())
	}

	// Direct public attacker (non-loopback) CANNOT spoof X-Real-IP / X-Forwarded-For to bypass lockout!
	for i := 0; i < maxFails; i++ {
		spoofIP := "192.0.2." + string(rune('1'+i))
		w := doLogin("198.51.100.55:44000", spoofIP, spoofIP, "wrong-pass")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("direct spoof attempt %d: got %d, want 401", i+1, w.Code)
		}
	}
	wDirectLocked := doLogin("198.51.100.55:44001", "192.0.2.99", "192.0.2.99", pw)
	if wDirectLocked.Code != http.StatusTooManyRequests {
		t.Fatalf("direct attacker bypassed lockout via header spoof! got %d", wDirectLocked.Code)
	}
}
