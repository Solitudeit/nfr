package server

import (
	"sync"
	"testing"
	"time"

	"github.com/solitudeit/nfr/internal/store"
)

func TestBucketPacesThroughput(t *testing.T) {
	// 256 KB/s, 1 second burst. Sending 1 MB must take roughly (1MB - burst)/rate.
	b := newBucket(256)
	start := time.Now()
	b.take(1024 * 1024)
	elapsed := time.Since(start)
	want := time.Duration((1024*1024 - 256*1024) / (256 * 1024) * float64(time.Second))
	if elapsed < want/2 {
		t.Fatalf("1MB at 256KB/s took %v, expected at least %v/2", elapsed, want)
	}
	if elapsed > want*4 {
		t.Fatalf("1MB at 256KB/s took %v, way beyond expected %v", elapsed, want)
	}
}

func TestBucketUnlimitedIsInstant(t *testing.T) {
	var b *bucket
	start := time.Now()
	b.take(8 << 20) // nil bucket: no-op
	if time.Since(start) > 10*time.Millisecond {
		t.Fatal("nil bucket delayed traffic")
	}
}

func TestBucketSmallWritesStayFast(t *testing.T) {
	// Small payloads within the burst must not sleep at all.
	b := newBucket(64)
	start := time.Now()
	for i := 0; i < 64; i++ {
		b.take(1024) // 64 KB total, well within the 64 KB burst
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("in-burst traffic was delayed")
	}
}

func TestBucketConcurrentWritersShareBudget(t *testing.T) {
	// Four writers each sending 256 KB through a 256 KB/s bucket take the
	// same time as one writer sending the full 1 MB.
	b := newBucket(256)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.take(256 * 1024)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	want := time.Duration(3 * float64(time.Second)) // (1MB - burst) / 256KB/s
	if elapsed < want/2 {
		t.Fatalf("concurrent writers finished in %v, budget sharing looks broken", elapsed)
	}
}

func TestApplyUsageQuotaLifecycle(t *testing.T) {
	tun := &store.Tunnel{Name: "friend", QuotaMB: 100, Enabled: true}

	if applyUsage(tun, 50<<20, "2026-01") {
		t.Fatal("50MB under a 100MB quota reported over")
	}
	if tun.UsedBytes != 50<<20 || tun.UsageMonth != "2026-01" {
		t.Fatalf("usage = %d in %s", tun.UsedBytes, tun.UsageMonth)
	}

	if !applyUsage(tun, 60<<20, "2026-01") {
		t.Fatal("110MB over a 100MB quota did not report over")
	}

	// Month rollover resets usage and resumes a quota-paused tunnel.
	tun.QuotaPaused = true
	tun.Enabled = false
	if applyUsage(tun, 1, "2026-02") {
		t.Fatal("fresh month reported over quota")
	}
	if tun.UsedBytes != 1 {
		t.Fatalf("usage after rollover = %d, want 1", tun.UsedBytes)
	}
	if !tun.Enabled || tun.QuotaPaused {
		t.Fatal("quota-paused tunnel did not resume after rollover")
	}
}

func TestApplyUsageNoQuota(t *testing.T) {
	tun := &store.Tunnel{Enabled: true}
	if applyUsage(tun, 1<<40, "2026-01") {
		t.Fatal("tunnel without a quota reported over")
	}
	if tun.UsedBytes != 1<<40 {
		t.Fatal("usage was not accumulated without a quota")
	}
}
