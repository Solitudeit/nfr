// Package transport builds the TLS material used on the control channel and
// by the console. Self-signed certificates are the default: an NFR node is
// usually reached by address, and pinning beats a certificate authority the
// operator does not want to run.
package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/proto"
)

// CertFingerprint returns the lowercase hex sha256 of a certificate.
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// PinOf returns the fingerprint of the peer certificate seen on a live
// connection, if any.
func PinOf(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	return CertFingerprint(cs.PeerCertificates[0])
}

// LoadOrCreateCert reads cert.pem/key.pem from dir, generating a self-signed
// pair on first use. The returned fingerprint identifies the generated pair.
func LoadOrCreateCert(dir, host string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		fp := ""
		if len(c.Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
				c.Leaf = leaf
				fp = CertFingerprint(leaf)
			}
		}
		return c, fp, nil
	}
	certPEM, keyPEM, err := selfSigned(host)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", err
	}
	c.Leaf = leaf
	return c, CertFingerprint(leaf), nil
}

// CertFingerprintOfPEM parses a certificate file and fingerprints it.
func CertFingerprintOfPEM(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return CertFingerprint(cert), nil
}

func selfSigned(host string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "nfr", Organization: []string{"NFR"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost", "nfr"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if host != "" && host != "localhost" {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// ServerConfig wraps a certificate for the control listener.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{proto.ALPN},
	}
}

// ClientConfig is the tunnel (data plane) TLS config. It offers the tunnel
// ALPN so the control port routes the connection to the agent handler.
func ClientConfig(serverName, pin string) *tls.Config {
	return clientConfig(serverName, pin, proto.ALPN)
}

// WebClientConfig is the plain HTTPS config used for discovery and for
// calling another node's REST API. It must not offer the tunnel ALPN.
func WebClientConfig(serverName, pin string) *tls.Config {
	return clientConfig(serverName, pin, "http/1.1")
}

func clientConfig(serverName, pin, alpn string) *tls.Config {
	pin = normalizePin(pin)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		InsecureSkipVerify: true,
		ServerName:         serverName,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			if pin == "" {
				return nil
			}
			if got := CertFingerprint(cert); got != pin {
				return fmt.Errorf("certificate fingerprint mismatch: got %s want %s", got, pin)
			}
			return nil
		},
	}
}

func normalizePin(pin string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(pin)), ":", "")
}

// FormatPin renders a fingerprint for humans.
func FormatPin(hexPin string) string {
	h := normalizePin(hexPin)
	if len(h) != 64 {
		return hexPin
	}
	parts := make([]string, 0, 32)
	for i := 0; i < 64; i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// PublicClientConfig verifies the peer against the system roots. It is used
// when the server presents a publicly trusted certificate (ACME), where
// pinning a fingerprint would break on every renewal.
func PublicClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{proto.ALPN},
		ServerName: serverName,
	}
}
