package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/agent"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/version"
)

func buildTestTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUpdaterSelfUpgradeAndBadChecksumRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script smoke test runs on Unix")
	}

	targetVer := "v9.9.9-preview.1"
	archiveName, err := releaseArchiveName(targetVer, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}

	newNfrsScript := []byte("#!/bin/sh\necho 'nfrs v9.9.9-preview.1'\n")
	newNfrcScript := []byte("#!/bin/sh\necho 'nfrc v9.9.9-preview.1'\n")
	archiveBytes := buildTestTarGz(t, map[string][]byte{
		"nfr/nfrs": newNfrsScript,
		"nfr/nfrc": newNfrcScript,
	})
	sum := sha256.Sum256(archiveBytes)
	validSums := hex.EncodeToString(sum[:]) + "  " + archiveName + "\n"
	badSums := strings.Repeat("0", 64) + "  " + archiveName + "\n"

	serveBadSum := false
	srvHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			if serveBadSum {
				_, _ = w.Write([]byte(badSums))
			} else {
				_, _ = w.Write([]byte(validSums))
			}
		case archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srvHTTP.Close()

	t.Setenv("NFR_UPDATE_CURRENT_VERSION", "v0.13.0-preview.2")
	t.Setenv("NFR_UPDATE_DOWNLOAD_BASE", srvHTTP.URL)
	t.Setenv("NFR_DISABLE_UPDATE_CHECK", "1")
	t.Setenv("NFR_ALLOW_SELF_UPDATE_IN_CONTAINER", "1")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.DataDir = t.TempDir()
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	fakeExe := filepath.Join(binDir, "nfrs")
	fakeNfrc := filepath.Join(binDir, "nfrc")
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\necho old-nfrs\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeNfrc, []byte("#!/bin/sh\necho old-nfrc\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Bad SHA-256 must abort without replacing the original binary.
	serveBadSum = true
	srv.updater.runUpgrade(fakeExe, targetVer, "github", "")
	if p := srv.UpgradeProgress(); p.State != "error" || !strings.Contains(p.Message, "SHA-256") {
		t.Fatalf("expected SHA-256 error, got %+v", p)
	}
	if got, _ := os.ReadFile(fakeExe); !strings.Contains(string(got), "old-nfrs") {
		t.Fatalf("binary was modified despite bad checksum: %s", got)
	}

	// 2. Valid SHA-256 replaces nfrs and sibling nfrc atomically and keeps .bak backup.
	serveBadSum = false
	restarted := make(chan string, 1)
	srv.updater.restartHook = func(p string) { restarted <- p }
	srv.updater.runUpgrade(fakeExe, targetVer, "github", "")

	select {
	case gotPath := <-restarted:
		if gotPath != fakeExe {
			t.Fatalf("restarted path = %q, want %q", gotPath, fakeExe)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("restart hook not called, progress=%+v", srv.UpgradeProgress())
	}
	if p := srv.UpgradeProgress(); p.State != "restarting" || p.Percent != 100 {
		t.Fatalf("expected restarting 100%%, got %+v", p)
	}
	if got, _ := os.ReadFile(fakeExe); !strings.Contains(string(got), "v9.9.9-preview.1") {
		t.Fatalf("new nfrs not installed: %s", got)
	}
	if got, _ := os.ReadFile(fakeNfrc); !strings.Contains(string(got), "v9.9.9-preview.1") {
		t.Fatalf("sibling nfrc not updated: %s", got)
	}
	if bak, err := os.ReadFile(fakeExe + ".bak"); err != nil || !strings.Contains(string(bak), "old-nfrs") {
		t.Fatalf("backup nfrs.bak missing or wrong: err=%v content=%s", err, bak)
	}
}

func TestAgentLogsTailUpdateHintWhenOutdated(t *testing.T) {
	origVer := version.Version
	version.Version = "v0.13.0-preview.2"
	defer func() { version.Version = origVer }()

	manifestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"preview":{"version":"v0.13.0-preview.9","notes":"update"}}`))
	}))
	defer manifestSrv.Close()

	t.Setenv("NFR_UPDATE_MANIFEST_URL", manifestSrv.URL)
	t.Setenv("NFR_DISABLE_UPDATE_CHECK", "1")

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.DataDir = dir
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()

	// Wait for control listener address
	var ctlAddr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		if srv.ctlLn != nil {
			ctlAddr = srv.ctlLn.Addr().String()
		}
		srv.mu.Unlock()
		if ctlAddr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ctlAddr == "" {
		t.Fatal("control listener did not start")
	}

	// Populate server's update cache so Welcome advertises v0.13.0-preview.9.
	if _, err := srv.CheckForUpdates(ctx, "github", "preview"); err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}

	en := true
	tun, err := srv.CreateStandaloneTunnel(api.TunnelRequest{
		Name: "hintdemo", Type: "tcp", Local: "127.0.0.1:18088", Enabled: &en,
	})
	if err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "nfrc.log")
	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	ag := agent.New(agent.Options{
		Addr:      ctlAddr,
		AuthToken: srv.AuthToken(),
		Name:      "t-" + tun.ID,
		Pin:       srv.Pin(),
		Proxies:   []proto.ProxySpec{{Name: tun.Name, Type: tun.Type, Local: tun.Local, RemotePort: tun.RemotePort}},
		Logger:    log.NewClient("info", logFile),
	})
	go ag.Run(agCtx)

	deadline = time.Now().Add(5 * time.Second)
	var content string
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(logFile)
		content = string(b)
		if strings.Contains(content, "发现新版本") && strings.Contains(content, "v0.13.0-preview.9") {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !strings.Contains(content, "发现新版本") || !strings.Contains(content, "v0.13.0-preview.9") {
		t.Fatalf("expected outdated nfrc log tail hint, got:\n%s", content)
	}
	// Verify update hint appears AFTER tunnel start message.
	if strings.Index(content, "发现新版本") < strings.Index(content, "启动 [tcp] 隧道成功") {
		t.Fatalf("expected update hint at tail of startup logs, got:\n%s", content)
	}
}

func TestUpdaterDisableSelfUpdateEnv(t *testing.T) {
	t.Setenv("NFR_DISABLE_SELF_UPDATE", "1")
	t.Setenv("NFR_DISABLE_UPDATE_CHECK", "1")
	t.Setenv("NFR_UPDATE_CURRENT_VERSION", "v0.13.0-preview.5")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.DataDir = t.TempDir()
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}

	status := srv.UpdateStatus()
	if status.CanSelfUpdate {
		t.Fatalf("expected CanSelfUpdate=false when NFR_DISABLE_SELF_UPDATE=1")
	}
	if !strings.Contains(status.CannotUpdateReason, "NFR_DISABLE_SELF_UPDATE") {
		t.Fatalf("unexpected CannotUpdateReason: %q", status.CannotUpdateReason)
	}
	if _, err := srv.StartSelfUpgrade("github", "", "v0.13.0-preview.6"); err == nil {
		t.Fatalf("expected StartSelfUpgrade to fail when NFR_DISABLE_SELF_UPDATE=1")
	}
}

func TestUpdaterMirrorPrefersOfficialSHA256(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script smoke test runs on Unix")
	}

	targetVer := "v9.9.9-preview.2"
	archiveName, err := releaseArchiveName(targetVer, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}

	realArchive := buildTestTarGz(t, map[string][]byte{
		"nfr/nfrs": []byte("#!/bin/sh\necho 'real-nfrs v9.9.9-preview.2'\n"),
	})
	tamperedArchive := buildTestTarGz(t, map[string][]byte{
		"nfr/nfrs": []byte("#!/bin/sh\necho 'tampered-nfrs'\n"),
	})
	realSum := sha256.Sum256(realArchive)
	tamperedSum := sha256.Sum256(tamperedArchive)
	realSumsTxt := hex.EncodeToString(realSum[:]) + "  " + archiveName + "\n"
	tamperedSumsTxt := hex.EncodeToString(tamperedSum[:]) + "  " + archiveName + "\n"

	// Mirror serves tampered archive + matching tampered SHA256SUMS.
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			_, _ = w.Write([]byte(tamperedSumsTxt))
		case archiveName:
			_, _ = w.Write(tamperedArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mirrorSrv.Close()

	// Official GitHub serves the real SHA256SUMS.
	officialFail := false
	officialSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if officialFail {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(realSumsTxt))
	}))
	defer officialSrv.Close()

	t.Setenv("NFR_UPDATE_CURRENT_VERSION", "v0.13.0-preview.5")
	t.Setenv("NFR_UPDATE_DOWNLOAD_BASE", mirrorSrv.URL)
	t.Setenv("NFR_UPDATE_OFFICIAL_SUMS_URL", officialSrv.URL+"/SHA256SUMS")
	t.Setenv("NFR_MIRROR_PREFIXES", mirrorSrv.URL+"/")
	t.Setenv("NFR_DISABLE_UPDATE_CHECK", "1")
	t.Setenv("NFR_ALLOW_SELF_UPDATE_IN_CONTAINER", "1")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.ControlListen = "127.0.0.1:0"
	cfg.HTTPListen = "off"
	cfg.HTTPSListen = "off"
	cfg.DataDir = t.TempDir()
	srv, _, err := New(Options{Config: cfg, Store: st, Logger: log.New("error", "")})
	if err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	fakeExe := filepath.Join(binDir, "nfrs")
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\necho old-nfrs\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Official SHA256SUMS reachable -> tampered mirror archive MUST be rejected!
	srv.updater.runUpgrade(fakeExe, targetVer, "mirror", mirrorSrv.URL+"/")
	if p := srv.UpgradeProgress(); p.State != "error" || !strings.Contains(p.Message, "SHA-256") || p.ChecksumSource != "official" {
		t.Fatalf("expected official SHA-256 mismatch error, got %+v", p)
	}
	if got, _ := os.ReadFile(fakeExe); !strings.Contains(string(got), "old-nfrs") {
		t.Fatalf("binary modified despite official checksum mismatch: %s", got)
	}

	// 2. Official SHA256SUMS unreachable -> falls back cleanly to mirror SHA256SUMS.
	officialFail = true
	restarted := make(chan string, 1)
	srv.updater.restartHook = func(p string) { restarted <- p }
	srv.updater.runUpgrade(fakeExe, targetVer, "mirror", mirrorSrv.URL+"/")
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("mirror fallback upgrade did not complete: %+v", srv.UpgradeProgress())
	}
	if p := srv.UpgradeProgress(); p.ChecksumSource != "mirror" {
		t.Fatalf("expected ChecksumSource=mirror on fallback, got %+v", p)
	}
}
