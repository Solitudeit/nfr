// Package metrics keeps per tunnel counters and a short rolling history in
// memory. Nothing here is persisted: restarting a node resets the numbers.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// secWindow is the number of one second samples kept per tunnel.
	secWindow = 300
	// minWindow is the number of one minute samples kept per tunnel (24h).
	minWindow = 1440
)

// Counter is one tunnel's live totals.
type Counter struct {
	In     atomic.Int64
	Out    atomic.Int64
	Conns  atomic.Int64
	Errs   atomic.Int64
	Active atomic.Int64
}

// Point is a rate sample.
type Point struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

// Snapshot is the JSON friendly view of one tunnel.
type Snapshot struct {
	TunnelID string  `json:"tunnel_id"`
	In       int64   `json:"in"`
	Out      int64   `json:"out"`
	Conns    int64   `json:"conns"`
	Errs     int64   `json:"errs"`
	Active   int64   `json:"active"`
	InRate   float64 `json:"in_rate"`
	OutRate  float64 `json:"out_rate"`
	Seconds  []Point `json:"seconds,omitempty"`
	Minutes  []Point `json:"minutes,omitempty"`
}

type hist struct {
	sec     [secWindow]Point
	secI    int
	min     [minWindow]Point
	minI    int
	lastIn  float64
	lastOut float64
	accIn   float64
	accOut  float64
	ticks   int64
}

// Registry owns every counter.
type Registry struct {
	mu    sync.Mutex
	m     map[string]*Counter
	hist  map[string]*hist
	total Counter
	start time.Time
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		m:     make(map[string]*Counter),
		hist:  make(map[string]*hist),
		start: time.Now(),
	}
}

// For returns the counter for a tunnel, creating it on first use.
func (r *Registry) For(tunnelID string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[tunnelID]
	if !ok {
		c = &Counter{}
		r.m[tunnelID] = c
		r.hist[tunnelID] = &hist{}
	}
	return c
}

// Drop forgets a tunnel.
func (r *Registry) Drop(tunnelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, tunnelID)
	delete(r.hist, tunnelID)
}

// AddBytes records traffic for a tunnel.
func (r *Registry) AddBytes(tunnelID string, in, out int64) {
	c := r.For(tunnelID)
	if in != 0 {
		c.In.Add(in)
		r.total.In.Add(in)
	}
	if out != 0 {
		c.Out.Add(out)
		r.total.Out.Add(out)
	}
}

// Sample advances the rolling history. Call it once per second.
func (r *Registry) Sample() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.m {
		h := r.hist[id]
		if h == nil {
			h = &hist{}
			r.hist[id] = h
		}
		in := float64(c.In.Load())
		out := float64(c.Out.Load())
		di := in - h.lastIn
		do := out - h.lastOut
		h.lastIn, h.lastOut = in, out
		if di < 0 {
			di = 0
		}
		if do < 0 {
			do = 0
		}
		h.sec[h.secI] = Point{In: di, Out: do}
		h.secI = (h.secI + 1) % secWindow
		h.accIn += di
		h.accOut += do
		h.ticks++
		if h.ticks%60 == 0 {
			h.min[h.minI] = Point{In: h.accIn, Out: h.accOut}
			h.minI = (h.minI + 1) % minWindow
			h.accIn, h.accOut = 0, 0
		}
	}
}

// Snapshot renders every known tunnel.
func (r *Registry) Snapshot() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Snapshot, 0, len(r.m))
	for id, c := range r.m {
		h := r.hist[id]
		s := Snapshot{
			TunnelID: id,
			In:       c.In.Load(),
			Out:      c.Out.Load(),
			Conns:    c.Conns.Load(),
			Errs:     c.Errs.Load(),
			Active:   c.Active.Load(),
		}
		if h != nil {
			s.Seconds = h.secondsOrdered()
			s.Minutes = h.minutesOrdered()
			if n := len(s.Seconds); n > 0 {
				s.InRate = s.Seconds[n-1].In
				s.OutRate = s.Seconds[n-1].Out
			}
		}
		out = append(out, s)
	}
	return out
}

// Totals aggregates the whole instance.
func (r *Registry) Totals() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{In: r.total.In.Load(), Out: r.total.Out.Load()}
}

func (h *hist) secondsOrdered() []Point {
	out := make([]Point, 0, secWindow)
	for i := 0; i < secWindow; i++ {
		out = append(out, h.sec[(h.secI+i)%secWindow])
	}
	return out
}

func (h *hist) minutesOrdered() []Point {
	out := make([]Point, 0, minWindow)
	for i := 0; i < minWindow; i++ {
		out = append(out, h.min[(h.minI+i)%minWindow])
	}
	return out
}
