package version

import "testing"

func TestSemverCompareAndIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.13.0-preview.1", "v0.13.0-preview.2", -1},
		{"v0.13.0-preview.2", "v0.13.0-preview.10", -1},
		{"v0.13.0-preview.3", "v0.13.0", -1},
		{"v0.13.0", "v0.13.0-preview.9", 1},
		{"v0.13.0", "v0.13.1-preview.1", -1},
		{"v0.13.0-preview.2", "v0.13.0-preview.2", 0},
		{"v0.13.0-preview.2 (commit abc, built 2026)", "v0.13.0-preview.3", -1},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}

	if !IsNewer("v0.13.0-preview.3", "v0.13.0-preview.2") {
		t.Fatal("expected preview.3 newer than preview.2")
	}
	if IsNewer("v0.13.0-preview.2", "v0.13.0-preview.2") {
		t.Fatal("same version must not be newer")
	}
	if IsNewer("v0.13.0-preview.3", "dev") {
		t.Fatal("dev builds must not trigger IsNewer")
	}
	if !IsPrerelease("v0.13.0-preview.3") || IsPrerelease("v0.13.0") {
		t.Fatal("IsPrerelease mismatch")
	}
}
