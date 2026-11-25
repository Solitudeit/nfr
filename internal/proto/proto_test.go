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
		Tunnels: []TunnelSpec{{ID: "t1", Name: "web", Type: "tcp", Local: "127.0.0.1:8080", PublicAddr: "154.219.109.24:20152"}},
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
	if out.Tunnels[0].Local != "127.0.0.1:8080" || out.Tunnels[0].PublicAddr != "154.219.109.24:20152" {
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

func TestProbeFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := &Frame{
		Type:     TypeProbe,
		Code:     "probe-1",
		NodeID:   "tun-1",
		Message:  "127.0.0.1:8080",
		Protocol: "tcp",
	}
	if err := WriteFrame(&buf, req); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	gotReq, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if gotReq.Type != TypeProbe || gotReq.Code != "probe-1" || gotReq.NodeID != "tun-1" ||
		gotReq.Message != "127.0.0.1:8080" || gotReq.Protocol != "tcp" {
		t.Fatalf("probe round trip mismatch: %+v", gotReq)
	}

	res := &Frame{
		Type:    TypeProbeResult,
		Code:    "probe-1",
		Status:  ProbeBusy,
		TS:      12,
		Message: "probe busy",
	}
	if err := WriteFrame(&buf, res); err != nil {
		t.Fatalf("write probe result: %v", err)
	}
	gotRes, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read probe result: %v", err)
	}
	if gotRes.Type != TypeProbeResult || gotRes.Code != "probe-1" || gotRes.Status != ProbeBusy ||
		gotRes.TS != 12 || gotRes.Message != "probe busy" {
		t.Fatalf("probe result round trip mismatch: %+v", gotRes)
	}
}

func TestKickFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &Frame{Type: TypeKick, Message: "强制下线"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("write kick: %v", err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read kick: %v", err)
	}
	if out.Type != TypeKick || out.Message != "强制下线" {
		t.Fatalf("kick round trip mismatch: %+v", out)
	}
}
