package transport

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

func startTestTLSServer(t *testing.T, nextProtos []string) (string, string, func()) {
	t.Helper()
	cert, pin, err := LoadOrCreateCert(t.TempDir(), "localhost")
	if err != nil {
		t.Fatalf("LoadOrCreateCert: %v", err)
	}
	srvCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   nextProtos,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), pin, func() {
		_ = ln.Close()
		<-done
	}
}

func TestClientConfigFailsClosedWhenPinEmpty(t *testing.T) {
	addr, pin, stop := startTestTLSServer(t, []string{"nfr/1", "http/1.1"})
	defer stop()

	dial := func(cfg *tls.Config) (tls.ConnectionState, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr, cfg)
		if err != nil {
			return tls.ConnectionState{}, err
		}
		cs := conn.ConnectionState()
		_ = conn.Close()
		return cs, nil
	}

	if _, err := dial(ClientConfig("localhost", "")); err == nil || !strings.Contains(err.Error(), "pin is required") {
		t.Fatalf("ClientConfig with empty pin = %v, want pin-required error", err)
	}
	if _, err := dial(WebClientConfig("localhost", "")); err == nil || !strings.Contains(err.Error(), "pin is required") {
		t.Fatalf("WebClientConfig with empty pin = %v, want pin-required error", err)
	}
	if _, err := dial(ClientConfig("localhost", strings.Repeat("0", 64))); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("ClientConfig with wrong pin = %v, want mismatch error", err)
	}
	if _, err := dial(ClientConfig("localhost", pin)); err != nil {
		t.Fatalf("ClientConfig with exact hex pin failed: %v", err)
	}
	if _, err := dial(ClientConfig("localhost", FormatPin(pin))); err != nil {
		t.Fatalf("ClientConfig with formatted pin failed: %v", err)
	}
	cs, err := dial(DiscoveryWebClientConfig("localhost"))
	if err != nil {
		t.Fatalf("DiscoveryWebClientConfig failed: %v", err)
	}
	if got := PinOf(cs); got != pin {
		t.Fatalf("DiscoveryWebClientConfig PinOf = %q, want %q", got, pin)
	}
}
