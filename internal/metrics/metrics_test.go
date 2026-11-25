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
	r.total.In.Store(100)
	r.total.Out.Store(50)
	r.Sample()

	got := r.RecentRates()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[1].InRate != 0 || got[1].OutRate != 0 {
		t.Fatalf("negative delta not clamped: %+v", got[1])
	}
}
