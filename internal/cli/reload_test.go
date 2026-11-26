package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/log"
)

func TestRunAgentsWithReloadDoesNotDuplicateOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.toml")
	valid := "server_addr = \"127.0.0.1:18443\"\nauth_token = \"tok-1\"\npin = \"0123456789abcdef\"\nname = \"n1\"\n"
	if err := os.WriteFile(cfgPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := config.LoadAgent(cfgPath)
	if err != nil || !ok {
		t.Fatalf("LoadAgent: %v ok=%v", err, ok)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloadCh := make(chan os.Signal, 4)

	var starts atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	runner := func(rctx context.Context, _ config.Agent, _ *slog.Logger) {
		starts.Add(1)
		cur := active.Add(1)
		for {
			prev := maxActive.Load()
			if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer active.Add(-1)
		<-rctx.Done()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentsReloadLoop(ctx, reloadCh, cfgPath, cfg, log.New("error", ""), runner)
	}()

	// Wait for initial start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && starts.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Corrupt config and send 2 SIGHUPs -> MUST NOT restart or spawn duplicate runners!
	if err := os.WriteFile(cfgPath, []byte("BROKEN = [[["), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadCh <- os.Interrupt
	reloadCh <- os.Interrupt
	time.Sleep(150 * time.Millisecond)

	if got := starts.Load(); got != 1 {
		t.Fatalf("bad SIGHUPs caused unexpected runner starts: got %d, want 1", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("bad SIGHUPs spawned concurrent duplicate runners: maxActive=%d, want 1", got)
	}

	// Fix config and send SIGHUP -> MUST reload cleanly once (starts=2, maxActive still 1).
	valid2 := "server_addr = \"127.0.0.1:18444\"\nauth_token = \"tok-2\"\npin = \"0123456789abcdef\"\nname = \"n1\"\n"
	if err := os.WriteFile(cfgPath, []byte(valid2), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadCh <- os.Interrupt
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && starts.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("valid SIGHUP did not reload: starts=%d, want 2", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("reload overlapped old and new runners: maxActive=%d, want 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload loop did not exit on ctx cancel")
	}
}

func TestReloadLoopExitsWhenAgentStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloadCh := make(chan os.Signal, 1)

	runner := func(_ context.Context, _ config.Agent, _ *slog.Logger) {
		// Simulates Agent.Run exiting cleanly after receiving TypeKick.
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentsReloadLoop(ctx, reloadCh, "", config.Agent{}, log.New("error", ""), runner)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		cancel()
		t.Fatal("reload loop hung after agent runner exited cleanly")
	}
}
