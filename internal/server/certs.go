package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/nfr-tunnel/nfr/internal/config"
	"github.com/nfr-tunnel/nfr/internal/transport"
)

// certMode describes how peers should verify this node's certificate.
const (
	// modePin means the certificate is self-signed or operator supplied and
	// peers pin its fingerprint.
	modePin = "pin"
	// modeCA means the certificate comes from a public CA and peers verify it
	// against the system trust store.
	modeCA = "ca"
)

// loadTLS picks the certificate strategy for the HTTPS ingress and control
// port. Precedence: explicit certificate files, then ACME, then a generated
// self-signed pair.
func loadTLS(cfg config.Server) (*tls.Config, string, string, string, error) {
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return nil, "", "", "", errors.New("tls_cert and tls_key must be set together")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("load tls_cert: %w", err)
		}
		leaf, err := leafOf(cert)
		if err != nil {
			return nil, "", "", "", err
		}
		cert.Leaf = leaf
		tc := transport.ServerConfig(cert)
		return tc, transport.CertFingerprint(leaf), modePin, "file", nil
	}

	if cfg.ACME.Enabled {
		if strings.TrimSpace(cfg.Domain) == "" {
			return nil, "", "", "", errors.New("acme requires domain to be set")
		}
		cache := cfg.ACME.Cache
		if cache == "" {
			cache = filepath.Join(cfg.DataDir, "acme")
		}
		manager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cache),
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Email:      cfg.ACME.Email,
		}
		if ca := strings.TrimSpace(cfg.ACME.CA); ca != "" {
			manager.Client = &acme.Client{DirectoryURL: ca}
		}
		tc := manager.TLSConfig()
		tc.MinVersion = tls.VersionTLS13
		return tc, "", modeCA, "acme", nil
	}

	cert, pin, err := transport.LoadOrCreateCert(filepath.Join(cfg.DataDir, "tls"), cfg.Domain)
	if err != nil {
		return nil, "", "", "", err
	}
	return transport.ServerConfig(cert), pin, modePin, "self-signed", nil
}

func leafOf(cert tls.Certificate) (*x509.Certificate, error) {
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("certificate has no chain")
	}
	return x509.ParseCertificate(cert.Certificate[0])
}

// CertSummary describes the active certificate for the console and API.
func (s *Server) CertSummary() Cert {
	out := Cert{Mode: s.certMode, Source: s.certSource, Pin: s.pin}
	if len(s.tls.Certificates) == 0 {
		return out
	}
	leaf, err := leafOf(s.tls.Certificates[0])
	if err != nil {
		return out
	}
	out.Subject = leaf.Subject.CommonName
	out.DNSNames = append([]string(nil), leaf.DNSNames...)
	notAfter := leaf.NotAfter.UTC()
	out.NotAfter = &notAfter
	if out.Pin == "" {
		out.Pin = transport.CertFingerprint(leaf)
	}
	return out
}

// Cert is the JSON shape returned by /api/v1/certs.
type Cert struct {
	Source     string     `json:"source"`
	Mode       string     `json:"mode"`
	Pin        string     `json:"pin"`
	Subject    string     `json:"subject"`
	DNSNames   []string   `json:"dns_names"`
	NotAfter   *time.Time `json:"not_after,omitempty"`
	ACMEIssued bool       `json:"acme_issued,omitempty"`
}
