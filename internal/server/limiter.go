package server

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/store"
)

// Bandwidth shaping without any third party dependency. Each tunnel gets one
// tunnelLimit with a token bucket per direction; every connection on the
// tunnel shares the same buckets, so the cap applies to the tunnel as a whole
// rather than per connection. Small bursts are allowed (one second of slack),
// which keeps TLS handshakes and DNS style request/response patterns fluid.

const throttleChunk = 32 << 10 // slice large writes so the bucket shapes the flow

// bucket is a byte oriented token bucket.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // refill speed in bytes per second
	burst  float64 // capacity: one second of slack
	tokens float64
	last   time.Time
}

func newBucket(rateKB int) *bucket {
	r := float64(rateKB) * 1024
	return &bucket{rate: r, burst: r, tokens: r, last: time.Now()}
}

// take accounts for n bytes, sleeping as long as the bucket needs to refill.
// A nil bucket is an unlimited bucket.
func (b *bucket) take(n int) {
	if b == nil || n <= 0 {
		return
	}
	for remain := n; remain > 0; {
		chunk := remain
		if float64(chunk) > b.burst {
			chunk = int(b.burst)
		}
		if d := b.reserve(chunk); d > 0 {
			time.Sleep(d)
		}
		remain -= chunk
	}
}

// reserve consumes n tokens and returns how long to wait before sending.
// The wait covers the current shortfall only; the n bytes are charged in
// full regardless, so a writer that outruns the bucket does not loop hot:
// its next bytes are already paid for by future refills, and concurrent
// writers on the same tunnel queue behind the same debt. Charging the
// shortfall instead (tokens -= need) would compound the debt on every
// already-overdrawn call and let traffic run faster than the rate.
func (b *bucket) reserve(n int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return 0
	}
	wait := (float64(n) - b.tokens) / b.rate
	b.tokens -= float64(n)
	return time.Duration(wait * float64(time.Second))
}

// tunnelLimit is one tunnel's shared per direction budget.
type tunnelLimit struct {
	rateKB int
	up     *bucket // visitor -> origin (traffic in)
	down   *bucket // origin -> visitor (traffic out)
}

// limiterFor returns the tunnel's limiter, creating or rebuilding it when the
// configured rate changed. It returns nil for unlimited tunnels.
func (s *Server) limiterFor(t store.Tunnel) *tunnelLimit {
	if t.BandwidthKB <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limits[t.ID]
	if l == nil || l.rateKB != t.BandwidthKB {
		l = &tunnelLimit{rateKB: t.BandwidthKB, up: newBucket(t.BandwidthKB), down: newBucket(t.BandwidthKB)}
		s.limits[t.ID] = l
	}
	return l
}

// dropLimit forgets a tunnel's limiter and usage flush baseline.
func (s *Server) dropLimit(id string) {
	s.mu.Lock()
	delete(s.limits, id)
	delete(s.flushed, id)
	s.mu.Unlock()
}

// pruneLimits forgets limiters of tunnels that vanished or were disabled;
// called from reconcile so editing a tunnel resets its buckets too.
func (s *Server) pruneLimits(tunnels []store.Tunnel) {
	keep := make(map[string]bool, len(tunnels))
	for _, t := range tunnels {
		if t.Enabled {
			keep[t.ID] = true
		}
	}
	s.mu.Lock()
	for id := range s.limits {
		if !keep[id] {
			delete(s.limits, id)
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// throttling wrappers

// throttledConn caps a visitor connection in both directions.
type throttledConn struct {
	net.Conn
	up   *bucket // reads from the visitor
	down *bucket // writes to the visitor
}

func (c *throttledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.up.take(n)
	}
	return n, err
}

func (c *throttledConn) Write(p []byte) (int, error) {
	return writeThrottled(c.Conn, c.down, p)
}

// throttleReader paces a reader.
type throttleReader struct {
	r io.Reader
	b *bucket
}

func (t throttleReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.b.take(n)
	}
	return n, err
}

// throttleReadCloser paces a ReadCloser, e.g. a proxied request body.
type throttleReadCloser struct {
	io.ReadCloser
	b *bucket
}

func (t *throttleReadCloser) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.b.take(n)
	}
	return n, err
}

// throttleWriter paces a writer.
type throttleWriter struct {
	w io.Writer
	b *bucket
}

func (t throttleWriter) Write(p []byte) (int, error) {
	return writeThrottled(t.w, t.b, p)
}

// writeThrottled sends p through b in chunks, yielding to the bucket between
// chunks so io.Copy sized buffers do not bypass the cap.
func writeThrottled(w io.Writer, b *bucket, p []byte) (int, error) {
	if b == nil {
		return w.Write(p)
	}
	total := 0
	for total < len(p) {
		end := total + throttleChunk
		if end > len(p) {
			end = len(p)
		}
		b.take(end - total)
		n, err := w.Write(p[total:end])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
