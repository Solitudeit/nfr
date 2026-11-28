package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/store"
)

func TestServeConsoleLocalOnlyAndAllowedHosts(t *testing.T) {
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
	cfg.Panel.LocalOnly = true
	cfg.Panel.AllowedHosts = []string{"nfr.example.com"}

	srv, _, err := New(Options{Config: cfg, Store: st})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv.SetPanel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// 1. Public IP hitting :8443 directly must get 404 when LocalOnly=true
	rPub := httptest.NewRequest(http.MethodGet, "https://nfr.example.com/nfr/api/auth/me", nil)
	rPub.RemoteAddr = "203.0.113.10:55555"
	wPub := httptest.NewRecorder()
	srv.serveConsole(wPub, rPub)
	if wPub.Code != http.StatusNotFound {
		t.Fatalf("public IP with LocalOnly=true got %d, want 404", wPub.Code)
	}

	// 2. Loopback proxy with unknown Host (DNS rebinding) must get 421
	rBadHost := httptest.NewRequest(http.MethodGet, "http://evil-rebind.attacker.test/nfr/", nil)
	rBadHost.RemoteAddr = "127.0.0.1:55556"
	wBadHost := httptest.NewRecorder()
	srv.serveConsole(wBadHost, rBadHost)
	if wBadHost.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unknown Host got %d, want 421", wBadHost.Code)
	}

	// 3. Loopback proxy with allowed Host succeeds cleanly
	rOK := httptest.NewRequest(http.MethodGet, "http://nfr.example.com/nfr/", nil)
	rOK.RemoteAddr = "127.0.0.1:55557"
	wOK := httptest.NewRecorder()
	srv.serveConsole(wOK, rOK)
	if wOK.Code != http.StatusOK {
		t.Fatalf("loopback proxy with allowed Host got %d, want 200", wOK.Code)
	}
}
