// Package proto defines the wire format spoken between a control node and its
// edge agents: length prefixed JSON control frames on the control stream and a
// tiny JSON header in front of every tunnelled byte stream.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is bumped whenever the frame set changes incompatibly.
const Version = 1

// ALPN is the TLS protocol identifier used on the control listener.
const ALPN = "nfr/1"

// Frame types.
const (
	TypeJoin      = "join"
	TypeHello     = "hello"
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
	TypeWelcome   = "welcome"
	TypeConfig    = "config"
	TypeAck       = "ack"
	TypePing      = "ping"
	TypePong      = "pong"
	TypeError     = "error"
	// TypeDialFailed is sent by an agent when it could not reach the inner
	// address of a tunnel. Without it the server only sees a closed stream and
	// cannot tell a dead local service from a quiet connection.
	TypeDialFailed = "dial_failed"
)

// MaxFrameSize bounds a single control frame.
const MaxFrameSize = 8 << 20

// Frame is the single envelope used by every control message; unused fields
// are omitted from the wire.
type Frame struct {
	Type    string `json:"t"`
	Proto   int    `json:"v,omitempty"`
	Node    string `json:"node,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Version string `json:"ver,omitempty"`
	Nonce   []byte `json:"nonce,omitempty"`
	MAC     []byte `json:"mac,omitempty"`
	NodeID  string `json:"node_id,omitempty"`
	Token   string `json:"token,omitempty"`

	// Config carries the full desired state for one node.
	Revision uint64       `json:"rev,omitempty"`
	Tunnels  []TunnelSpec `json:"tunnels,omitempty"`

	Code    string `json:"code,omitempty"`
	Message string `json:"msg,omitempty"`
	TS      int64  `json:"ts,omitempty"`
}

// TunnelSpec is the agent's view of one tunnel: where to dial locally.
type TunnelSpec struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"` // tcp | http | udp
	Local string `json:"local"`
}

// StreamHeader prefixes every proxied connection so the agent knows which
// local service to dial.
type StreamHeader struct {
	Tunnel string `json:"tunnel"`
	Remote string `json:"remote,omitempty"`
}

// WriteFrame writes one length prefixed frame.
func WriteFrame(w io.Writer, f *Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("frame too large: %d", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one length prefixed frame.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, fmt.Errorf("frame too large: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// WriteStreamHeader writes the 2 byte length prefixed tunnel header.
func WriteStreamHeader(w io.Writer, h *StreamHeader) error {
	body, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(body) > 0xffff {
		return errors.New("stream header too large")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// ReadStreamHeader reads the 2 byte length prefixed tunnel header.
func ReadStreamHeader(r io.Reader) (*StreamHeader, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var h StreamHeader
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// WriteFrameReader is the read side counterpart used by streaming callers.
type WriteFrameReader struct{ r io.Reader }

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *WriteFrameReader { return &WriteFrameReader{r: r} }

// Read reads the next frame.
func (f *WriteFrameReader) Read() (*Frame, error) { return ReadFrame(f.r) }
