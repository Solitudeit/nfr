package agent

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/log"
	"github.com/solitudeit/nfr/internal/proto"
	"github.com/solitudeit/nfr/internal/transport"
)

func TestAgentExitsCleanlyOnKick(t *testing.T) {
	cert, pin, err := transport.LoadOrCreateCert(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatalf("self-signed cert: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvTLS := transport.ServerConfig(cert)
	shared := "nfr_t_kick_test_secret"
	acceptCount := 0

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			acceptCount++
			tc := tls.Server(raw, srvTLS)
			if err := tc.Handshake(); err != nil {
				raw.Close()
				continue
			}
			sess, err := yamux.Server(tc, transport.YamuxConfig())
			if err != nil {
				tc.Close()
				continue
			}
			ctrl, err := sess.Accept()
			if err != nil {
				sess.Close()
				continue
			}
			_, _ = proto.ReadFrame(ctrl) // hello
			nonce := auth.RandomBytes(32)
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeChallenge, Nonce: nonce})
			_, _ = proto.ReadFrame(ctrl) // auth
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeWelcome, NodeID: "n-kick", Revision: 1})
			_ = proto.WriteFrame(ctrl, &proto.Frame{Type: proto.TypeKick, Message: "强制下线"})
			time.Sleep(50 * time.Millisecond)
			sess.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ag := New(Options{
		Addr:      ln.Addr().String(),
		AuthToken: shared,
		Name:      "t-demo1234",
		Pin:       transport.FormatPin(pin),
		Logger:    log.New("error", ""),
	})

	done := make(chan struct{})
	go func() {
		ag.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Agent.Run did not exit after receiving TypeKick")
	}
	if acceptCount != 1 {
		t.Fatalf("expected agent to stop without reconnecting, got %d connections", acceptCount)
	}
}
