package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/transport"
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
func loadTLS(cfg config.Server, st *store.Store) (*tls.Config, string, string, string, *autocert.Manager, error) {
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return nil, "", "", "", nil, errors.New("tls_cert and tls_key must be set together")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, "", "", "", nil, fmt.Errorf("load tls_cert: %w", err)
		}
		leaf, err := leafOf(cert)
		if err != nil {
			return nil, "", "", "", nil, err
		}
		cert.Leaf = leaf
		tc := transport.ServerConfig(cert)
		return tc, transport.CertFingerprint(leaf), modePin, "file", nil, nil
	}

	if cfg.ACME.Enabled {
		baseDomain := strings.ToLower(strings.TrimSpace(cfg.Domain))
		email := strings.TrimSpace(cfg.ACME.Email)
		if st != nil {
			snap := st.Snapshot()
			if baseDomain == "" {
				baseDomain = strings.ToLower(strings.TrimSpace(snap.Settings.Domain))
			}
			if email == "" {
				email = strings.TrimSpace(snap.Settings.HTTPSEmail)
			}
		}
		if baseDomain == "" {
			return nil, "", "", "", nil, errors.New("acme requires domain to be set")
		}
		cache := cfg.ACME.Cache
		if cache == "" {
			cache = filepath.Join(cfg.DataDir, "acme")
		}
		manager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cache),
			HostPolicy: acmeHostPolicy(baseDomain, st),
			Email:      email,
		}
		if ca := strings.TrimSpace(cfg.ACME.CA); ca != "" {
			manager.Client = &acme.Client{DirectoryURL: ca}
		}
		tc := manager.TLSConfig()
		tc.MinVersion = tls.VersionTLS13
		return tc, "", modeCA, "acme", manager, nil
	}

	cert, pin, err := transport.LoadOrCreateCert(filepath.Join(cfg.DataDir, "tls"), cfg.Domain)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	return transport.ServerConfig(cert), pin, modePin, "self-signed", nil, nil
}

// acmeHostPolicy allows certificates for the instance's configured domain and
// for any hostname explicitly attached to an enabled HTTP tunnel, preventing
// arbitrary unconfigured hostnames from exhausting ACME rate limits.
func acmeHostPolicy(cfgDomain string, st *store.Store) autocert.HostPolicy {
	cfgDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfgDomain), "."))
	return func(_ context.Context, host string) error {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if host == "" {
			return errors.New("acme: empty host")
		}
		if cfgDomain != "" && host == cfgDomain {
			return nil
		}
		if st != nil {
			allowed := false
			_ = st.View(func(s *store.State) error {
				if d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Settings.Domain), ".")); d != "" && host == d {
					allowed = true
					return nil
				}
				for _, t := range s.Tunnels {
					if !t.Enabled || t.Type != store.TunnelHTTP {
						continue
					}
					for _, h := range t.Hosts {
						if strings.EqualFold(strings.TrimSuffix(h, "."), host) {
							allowed = true
							return nil
						}
					}
				}
				return nil
			})
			if allowed {
				return nil
			}
		}
		return fmt.Errorf("acme: host %q is not configured on this instance", host)
	}
}

func (s *Server) httpIngressHandler() http.Handler {
	base := http.HandlerFunc(s.serveIngress)
	if s.acmeMgr != nil {
		return s.acmeMgr.HTTPHandler(base)
	}
	return base
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
