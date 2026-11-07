package proto

import (
	"bytes"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &Frame{
		Type: TypeConfig, NodeID: "abc", Revision: 7,
		Tunnels: []TunnelSpec{{ID: "t1", Name: "web", Type: "tcp", Local: "127.0.0.1:8080"}},
	}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Type != in.Type || out.Revision != in.Revision || len(out.Tunnels) != 1 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if out.Tunnels[0].Local != "127.0.0.1:8080" {
		t.Fatalf("tunnel spec lost: %+v", out.Tunnels[0])
	}
}

func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	hdr := []byte{0x7f, 0xff, 0xff, 0xff} // 2 GiB
	buf.Write(hdr)
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected an error for an oversized frame")
	} else if !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := StreamHeader{Tunnel: "abc", Remote: "1.2.3.4:55"}
	if err := WriteStreamHeader(&buf, &in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadStreamHeader(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Tunnel != in.Tunnel || out.Remote != in.Remote {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}
