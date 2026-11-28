package metrics

import "testing"

func TestRecentRatesColdStartAndWrap(t *testing.T) {
	r := New()
	if got := r.RecentRates(); len(got) != 0 {
		t.Fatalf("expected 0 points before first sample, got %d", len(got))
	}

	// Cold start: 5 samples with varying traffic should return exactly 5 points
	// in chronological order without leading zero padding.
	for i := 1; i <= 5; i++ {
		r.AddBytes("t1", int64(i*100), int64(i*50))
		r.Sample()
	}
	got := r.RecentRates()
	if len(got) != 5 {
		t.Fatalf("cold-start len = %d, want 5", len(got))
	}
	for i := 0; i < 5; i++ {
		wantIn := float64((i + 1) * 100)
		wantOut := float64((i + 1) * 50)
		if got[i].InRate != wantIn || got[i].OutRate != wantOut {
			t.Fatalf("point[%d] = (%v, %v), want (%v, %v)", i, got[i].InRate, got[i].OutRate, wantIn, wantOut)
		}
		if got[i].TS == 0 {
			t.Fatalf("point[%d] missing timestamp", i)
		}
	}

	// Wrap around past 60 samples (total 75 samples -> newest 60 kept: 16..75).
	for i := 6; i <= 75; i++ {
		r.AddBytes("t1", int64(i*10), int64(i*5))
		r.Sample()
	}
	wrapped := r.RecentRates()
	if len(wrapped) != rateWindow {
		t.Fatalf("wrapped len = %d, want %d", len(wrapped), rateWindow)
	}
	if wrapped[0].InRate != 160 || wrapped[0].OutRate != 80 {
		t.Fatalf("oldest wrapped point = (%v, %v), want (160, 80)", wrapped[0].InRate, wrapped[0].OutRate)
	}
	last := wrapped[len(wrapped)-1]
	if last.InRate != 750 || last.OutRate != 375 {
		t.Fatalf("newest wrapped point = (%v, %v), want (750, 375)", last.InRate, last.OutRate)
	}
}

func TestRecentRatesClampsNegativeDelta(t *testing.T) {
	r := New()
	r.AddBytes("t1", 500, 300)
	r.Sample()
	// Simulate counter reset below last sampled total.
	r.For("t1").In.Store(100)
	r.For("t1").Out.Store(50)
	r.Sample()

	got := r.RecentRates()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[1].InRate != 0 || got[1].OutRate != 0 {
		t.Fatalf("negative delta not clamped: %+v", got[1])
	}
}

func TestRecentRatesTracksDirectCounterIncrements(t *testing.T) {
	r := New()
	c1 := r.For("tcp-1")
	c2 := r.For("http-1")
	c1.In.Add(4096)
	c1.Out.Add(8192)
	c2.In.Add(1024)
	c2.Out.Add(2048)
	r.Sample()

	got := r.RecentRates()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].InRate != 5120 || got[0].OutRate != 10240 {
		t.Fatalf("RecentRates[0] = (%v, %v), want (5120, 10240)", got[0].InRate, got[0].OutRate)
	}
	tot := r.Totals()
	if tot.In != 5120 || tot.Out != 10240 {
		t.Fatalf("Totals = (%d, %d), want (5120, 10240)", tot.In, tot.Out)
	}
}

func TestRecentRatesForIsolatesOwners(t *testing.T) {
	r := New()
	// Tick 1..3: only user A's tunnel tA has traffic
	for i := 1; i <= 3; i++ {
		r.AddBytes("tA", 1000, 2000)
		r.Sample()
	}
	// Tick 4..5: user B's tunnel tB starts sending traffic too
	for i := 4; i <= 5; i++ {
		r.AddBytes("tA", 500, 1000)
		r.AddBytes("tB", 3000, 4000)
		r.Sample()
	}

	ratesA := r.RecentRatesFor(map[string]struct{}{"tA": {}})
	if len(ratesA) != 5 {
		t.Fatalf("ratesA len = %d, want 5", len(ratesA))
	}
	if ratesA[0].InRate != 1000 || ratesA[0].OutRate != 2000 {
		t.Fatalf("ratesA[0] = %+v, want (1000, 2000)", ratesA[0])
	}
	if ratesA[4].InRate != 500 || ratesA[4].OutRate != 1000 {
		t.Fatalf("ratesA[4] = %+v, want (500, 1000)", ratesA[4])
	}

	ratesB := r.RecentRatesFor(map[string]struct{}{"tB": {}})
	if len(ratesB) != 5 {
		t.Fatalf("ratesB len = %d, want 5", len(ratesB))
	}
	// First 3 ticks before tB existed must be 0, NEVER leaking tA's bytes
	for i := 0; i < 3; i++ {
		if ratesB[i].InRate != 0 || ratesB[i].OutRate != 0 {
			t.Fatalf("ratesB[%d] leaked traffic: %+v", i, ratesB[i])
		}
	}
	for i := 3; i < 5; i++ {
		if ratesB[i].InRate != 3000 || ratesB[i].OutRate != 4000 {
			t.Fatalf("ratesB[%d] = %+v, want (3000, 4000)", i, ratesB[i])
		}
	}

	// Empty set returns zeroed timeline (never leaks other users)
	ratesEmpty := r.RecentRatesFor(nil)
	if len(ratesEmpty) != 5 {
		t.Fatalf("ratesEmpty len = %d, want 5", len(ratesEmpty))
	}
	for i, p := range ratesEmpty {
		if p.InRate != 0 || p.OutRate != 0 {
			t.Fatalf("ratesEmpty[%d] leaked traffic: %+v", i, p)
		}
	}
}
