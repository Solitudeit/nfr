package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	secretKeyFile = "secret.key"
	encPrefix     = "enc:v1:"
)

// KeyFingerprint returns a deterministic SHA-256 hex digest of the local
// encryption key so a MySQL database can detect secret.key mismatches safely.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// loadOrCreateKey reads the 32-byte AES-256 key from NFR_SECRET_KEY (if set) or
// dir/secret.key, creating one with mode 0600 on first run.
func loadOrCreateKey(dir string) ([]byte, error) {
	if envHex := strings.TrimSpace(os.Getenv("NFR_SECRET_KEY")); envHex != "" {
		key, err := hex.DecodeString(envHex)
		if err != nil || len(key) != 32 {
			return nil, errors.New("invalid NFR_SECRET_KEY: expected 64-char hex (32 bytes)")
		}
		return key, nil
	}
	path := filepath.Join(dir, secretKeyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("invalid %s: expected 32-byte hex key", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".secret-*.key")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := os.Chmod(name, 0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(name, path); err != nil {
		return nil, err
	}
	return key, nil
}

// encryptSecret protects a stored credential with AES-256-GCM.
func encryptSecret(key []byte, plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, encPrefix) {
		return plain, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// decryptSecret reverses encryptSecret. Legacy plaintext values without the
// encPrefix pass through unchanged so upgrades from older state files work.
func decryptSecret(key []byte, stored string) (string, error) {
	if stored == "" || !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted secret: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("encrypted secret truncated")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}
