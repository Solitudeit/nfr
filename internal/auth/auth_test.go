package auth

import (
	"testing"
	"time"
)

func TestNormalizeJoinCode(t *testing.T) {
	cases := map[string]string{
		"abcd12":     "ABCD12",
		"abcd-12":    "ABCD12",
		" ab cd 12 ": "ABCD12",
		"0o1l":       "0011",
	}
	for in, want := range cases {
		if got := NormalizeJoinCode(in); got != want {
			t.Errorf("NormalizeJoinCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinCodeValid(t *testing.T) {
	now := time.Now().UTC()
	jc := JoinCode{Code: "ABC123", ExpiresAt: now.Add(time.Minute)}
	if err := jc.Valid(now); err != nil {
		t.Fatalf("fresh code rejected: %v", err)
	}
	if err := jc.Valid(now.Add(2 * time.Minute)); err != ErrExpired {
		t.Fatalf("expired code error = %v, want ErrExpired", err)
	}
	used := now.Add(-time.Second)
	jc.UsedAt = &used
	if err := jc.Valid(now); err != ErrUsed {
		t.Fatalf("used code error = %v, want ErrUsed", err)
	}
}

func TestMAC(t *testing.T) {
	secret := NewSecret("nfr_n_", 24)
	hash := Hash(secret)
	nonce := RandomBytes(16)
	mac := MAC(hash, nonce)
	if !CheckMAC(hash, nonce, mac) {
		t.Fatal("MAC did not verify")
	}
	if CheckMAC(hash, RandomBytes(16), mac) {
		t.Fatal("MAC verified with a different nonce")
	}
	if CheckMAC(Hash(NewSecret("", 8)), nonce, mac) {
		t.Fatal("MAC verified with a different secret")
	}
}

func TestNewJoinCodeLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code := NewJoinCode()
		if len(code) != 6 {
			t.Fatalf("join code %q is %d characters, want 6", code, len(code))
		}
		if NormalizeJoinCode(code) != code {
			t.Fatalf("join code %q is not normalised", code)
		}
		seen[code] = true
	}
	if len(seen) < 150 {
		t.Fatalf("only %d distinct codes in 200 draws", len(seen))
	}
}

func TestScopeAllows(t *testing.T) {
	if !ScopeAllows("admin", "write") || !ScopeAllows("write", "read") {
		t.Fatal("stronger scopes must satisfy weaker requirements")
	}
	if ScopeAllows("read", "write") || ScopeAllows("write", "admin") {
		t.Fatal("weaker scopes must not satisfy stronger requirements")
	}
}
