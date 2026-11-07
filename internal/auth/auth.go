// Package auth implements the secrets NFR hands out: node tokens, one-time
// join codes and API keys. Nothing secret is ever persisted in clear text
// except the join code, which only lives for a few minutes.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	// ErrExpired reports a join code past its lifetime.
	ErrExpired = errors.New("join code expired")
	// ErrUsed reports a join code already consumed.
	ErrUsed = errors.New("join code already used")
	// ErrUnknown reports a secret that matches no record.
	ErrUnknown = errors.New("unknown secret")
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var b32 = base32.NewEncoding(crockford).WithPadding(base32.NoPadding)

// RandomBytes returns n cryptographically random bytes.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}

// NewSecret returns a random secret with the given prefix.
func NewSecret(prefix string, n int) string {
	return prefix + strings.ToLower(b32.EncodeToString(RandomBytes(n)))
}

// Hash returns the hex sha256 of a secret. This is the value stored at rest;
// it doubles as the HMAC key used by the challenge-response handshake.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// MAC answers a server nonce using the stored hash form of a secret.
func MAC(secretHash string, nonce []byte) []byte {
	key, err := hex.DecodeString(secretHash)
	if err != nil {
		key = []byte(secretHash)
	}
	m := hmac.New(sha256.New, key)
	m.Write(nonce)
	return m.Sum(nil)
}

// CheckMAC verifies a challenge response in constant time.
func CheckMAC(secretHash string, nonce, mac []byte) bool {
	return hmac.Equal(MAC(secretHash, nonce), mac)
}

// NewJoinCode returns a six character code that is easy to read aloud.
func NewJoinCode() string {
	b := RandomBytes(6)
	out := make([]byte, 6)
	for i := range out {
		out[i] = crockford[int(b[i])%len(crockford)]
	}
	return string(out)
}

// NormalizeJoinCode uppercases and strips a user typed code.
func NormalizeJoinCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		switch r {
		case ' ', '\t', '-', '_', '.':
			continue
		case 'O':
			// Crockford base32 treats O as zero, which makes codes copied by
			// hand forgiving.
			b.WriteByte('0')
		case 'I', 'L':
			b.WriteByte('1')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// JoinCode is one pending invitation.
type JoinCode struct {
	Code      string     `json:"code"`
	NodeName  string     `json:"node_name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}

// Valid reports whether the code may still be redeemed at now.
func (j *JoinCode) Valid(now time.Time) error {
	switch {
	case j.UsedAt != nil:
		return ErrUsed
	case now.After(j.ExpiresAt):
		return ErrExpired
	}
	return nil
}

// Finder looks up join codes by their normalized value.
type Finder interface {
	FindJoinCode(code string) (*JoinCode, bool)
}

// APIKey is a credential for the HTTP API.
type APIKey struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Hash      string     `json:"hash"`
	Scope     string     `json:"scope"` // read | write | admin
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	AllowFrom []string   `json:"allow_from,omitempty"`
}

// ScopeAllows reports whether scope covers the requested action.
func ScopeAllows(scope, want string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return rank[scope] >= rank[want] && rank[want] > 0
}

// ConstantTimeEqual compares two strings without leaking length timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Fingerprint renders a sha256 in the conventional colon separated form.
func Fingerprint(sum []byte) string {
	h := hex.EncodeToString(sum)
	parts := make([]string, 0, len(h)/2)
	for i := 0; i+2 <= len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}
