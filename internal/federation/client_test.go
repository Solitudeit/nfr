package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/transport"
)

func newPeer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/nfr", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "peer", "version": "test", "control": ":8443", "verify": "pin",
		})
	})
	mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "unauthorized", Code: "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(api.Info{ID: "peer1", Name: "peer", Version: "test", Revision: 3})
	})
	mux.HandleFunc("/api/v1/tunnels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req api.TunnelRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(api.Tunnel{ID: "t1", Name: req.Name, Type: req.Type, Enabled: true, CreatedAt: time.Now().UTC()})
			return
		}
		_ = json.NewEncoder(w).Encode([]api.Tunnel{{ID: "t1", Name: "web"}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func pinOf(srv *httptest.Server) string {
	return transport.CertFingerprint(srv.Certificate())
}

func TestInfoAndTunnels(t *testing.T) {
	srv := newPeer(t)
	c := New(srv.URL+"/", "good", pinOf(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := c.Probe(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Name != "peer" || info.Revision != 3 {
		t.Fatalf("unexpected info: %+v", info)
	}
	tunnels, err := c.Tunnels(ctx)
	if err != nil {
		t.Fatalf("tunnels: %v", err)
	}
	if len(tunnels) != 1 || tunnels[0].Name != "web" {
		t.Fatalf("unexpected tunnels: %+v", tunnels)
	}
	created, err := c.CreateTunnel(ctx, api.TunnelRequest{Name: "api", Type: "http"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "api" {
		t.Fatalf("unexpected created tunnel: %+v", created)
	}
}

func TestAuthFailure(t *testing.T) {
	srv := newPeer(t)
	c := New(srv.URL, "bad", pinOf(srv))
	ctx := context.Background()
	_, err := c.Probe(ctx)
	if err == nil {
		t.Fatal("expected an error for a bad key")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error should surface the peer message: %v", err)
	}
}

func TestPinMismatch(t *testing.T) {
	srv := newPeer(t)
	c := New(srv.URL, "good", strings.Repeat("ab", 32))
	if _, err := c.Probe(context.Background()); err == nil {
		t.Fatal("expected the pinned fingerprint check to fail")
	}
}

func TestVerifyMode(t *testing.T) {
	srv := newPeer(t)
	mode, pin, err := VerifyMode(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("verify mode: %v", err)
	}
	if mode != "pin" {
		t.Fatalf("mode = %q", mode)
	}
	if pin != pinOf(srv) {
		t.Fatalf("pin = %q, want %q", pin, pinOf(srv))
	}
}
