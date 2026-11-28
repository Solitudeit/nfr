package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// LaunchTokenPrefix identifies compact single-argument `nfrc up` tokens.
	LaunchTokenPrefix = "nfr1_"
	launchTokenVer    = byte(1)
	launchFlagCA      = byte(1 << 0)
)

// LaunchSpec holds the minimal connection parameters needed to bring up one
// panel-managed tunnel with `nfrc up` without a local config file.
type LaunchSpec struct {
	ServerAddr string
	AuthToken  string
	TunnelID   string
	Pin        string // 64-char lowercase hex SHA-256 fingerprint (when !CA)
	CA         bool   // verify server against system CA roots (ACME / public cert)
}

// EncodeLaunchToken packs a LaunchSpec into a compact, shell-safe URL-base64
// token prefixed with "nfr1_". The binary layout is:
//
//	[1B version=1][1B flags][1B len(addr)][addr][1B len(token)][token][1B len(tid)][tid][optional 32B raw pin]
func EncodeLaunchToken(spec LaunchSpec) (string, error) {
	addr := NormalizeAddr(spec.ServerAddr)
	if addr == "" {
		return "", errors.New("server_addr is required")
	}
	tok := strings.TrimSpace(spec.AuthToken)
	if tok == "" {
		return "", errors.New("auth_token is required")
	}
	tid := strings.TrimSpace(spec.TunnelID)
	if tid == "" {
		return "", errors.New("tunnel_id is required")
	}
	if len(addr) > 255 || len(tok) > 255 || len(tid) > 255 {
		return "", errors.New("launch token field exceeds 255 bytes")
	}

	var flags byte
	var pinBytes []byte
	if spec.CA {
		flags |= launchFlagCA
	} else {
		cleanPin := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(spec.Pin)), ":", "")
		raw, err := hex.DecodeString(cleanPin)
		if err != nil || len(raw) != 32 {
			return "", errors.New("pin must be a 64-char SHA-256 hex fingerprint (or enable ca)")
		}
		pinBytes = raw
	}

	buf := make([]byte, 0, 2+1+len(addr)+1+len(tok)+1+len(tid)+len(pinBytes))
	buf = append(buf, launchTokenVer, flags)
	buf = append(buf, byte(len(addr)))
	buf = append(buf, addr...)
	buf = append(buf, byte(len(tok)))
	buf = append(buf, tok...)
	buf = append(buf, byte(len(tid)))
	buf = append(buf, tid...)
	buf = append(buf, pinBytes...)

	return LaunchTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// DecodeLaunchToken unpacks an "nfr1_..." token into a validated LaunchSpec.
func DecodeLaunchToken(raw string) (LaunchSpec, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, LaunchTokenPrefix) {
		return LaunchSpec{}, fmt.Errorf("invalid launch token: must start with %q", LaunchTokenPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, LaunchTokenPrefix))
	if err != nil {
		return LaunchSpec{}, fmt.Errorf("invalid launch token encoding: %w", err)
	}
	if len(data) < 5 {
		return LaunchSpec{}, errors.New("invalid launch token: truncated payload")
	}
	if data[0] != launchTokenVer {
		return LaunchSpec{}, fmt.Errorf("unsupported launch token version %d", data[0])
	}
	flags := data[1]
	if flags&^launchFlagCA != 0 {
		return LaunchSpec{}, fmt.Errorf("unsupported launch token flags 0x%02x", flags)
	}
	ca := flags&launchFlagCA != 0

	pos := 2
	readStr := func(field string) (string, error) {
		if pos >= len(data) {
			return "", fmt.Errorf("invalid launch token: missing %s", field)
		}
		n := int(data[pos])
		pos++
		if n == 0 || pos+n > len(data) {
			return "", fmt.Errorf("invalid launch token: invalid %s length", field)
		}
		v := string(data[pos : pos+n])
		pos += n
		return v, nil
	}

	addr, err := readStr("server_addr")
	if err != nil {
		return LaunchSpec{}, err
	}
	tok, err := readStr("auth_token")
	if err != nil {
		return LaunchSpec{}, err
	}
	tid, err := readStr("tunnel_id")
	if err != nil {
		return LaunchSpec{}, err
	}

	var pinHex string
	if ca {
		if pos != len(data) {
			return LaunchSpec{}, errors.New("invalid launch token: unexpected trailing bytes")
		}
	} else {
		if len(data)-pos != 32 {
			return LaunchSpec{}, errors.New("invalid launch token: missing or malformed 32-byte certificate pin")
		}
		pinHex = hex.EncodeToString(data[pos : pos+32])
	}

	return LaunchSpec{
		ServerAddr: NormalizeAddr(addr),
		AuthToken:  tok,
		TunnelID:   tid,
		Pin:        pinHex,
		CA:         ca,
	}, nil
}

// AgentConfig converts a LaunchSpec into a ready-to-run Agent configuration
// that binds to the specified panel-managed tunnel without a local file.
func (s LaunchSpec) AgentConfig() Agent {
	return Agent{
		ServerAddr: NormalizeAddr(s.ServerAddr),
		AuthToken:  strings.TrimSpace(s.AuthToken),
		Name:       "t-" + strings.TrimSpace(s.TunnelID),
		Pin:        strings.TrimSpace(s.Pin),
		CA:         s.CA,
	}
}
