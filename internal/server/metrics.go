package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nfr-tunnel/nfr/internal/version"
)

// buildMetrics serves a small Prometheus text exposition. It reuses the API
// authentication so tunnel names are not public: enable the API and mint a
// read key. Without a key the endpoint answers 403.
func (s *Server) buildMetrics() http.Handler {
	return s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, _ := r.Context().Value(ctxScope).(string)
		if scope == "" {
			writeErr(w, http.StatusForbidden, "insufficient_scope", "a read key is required")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(s.metricsText()))
	}))
}

func (s *Server) metricsText() string {
	info := s.Info()
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}
	line("# HELP nfr_up Whether this control node is serving requests.")
	line("# TYPE nfr_up gauge")
	line("nfr_up 1")
	line("# HELP nfr_build_info Build metadata.")
	line("# TYPE nfr_build_info gauge")
	line(`nfr_build_info{version=%q} 1`, version.Version)
	line("# HELP nfr_revision State revision, incremented on every change.")
	line("# TYPE nfr_revision gauge")
	line("nfr_revision %d", info.Revision)
	line("# HELP nfr_uptime_seconds Time since the process started serving.")
	line("# TYPE nfr_uptime_seconds counter")
	line("nfr_uptime_seconds %.0f", time.Since(s.started).Seconds())
	line("# HELP nfr_nodes_online Connected agents.")
	line("# TYPE nfr_nodes_online gauge")
	line("nfr_nodes_online %d", info.Nodes)
	line("# HELP nfr_tunnels Configured tunnels.")
	line("# TYPE nfr_tunnels gauge")
	line("nfr_tunnels %d", info.Tunnels)

	tunnels := s.Tunnels()
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].Name < tunnels[j].Name })
	enabled, online := 0, 0
	for _, t := range tunnels {
		if t.Enabled {
			enabled++
		}
		if t.Online {
			online++
		}
	}
	line("# HELP nfr_tunnels_enabled Tunnels that are switched on.")
	line("# TYPE nfr_tunnels_enabled gauge")
	line("nfr_tunnels_enabled %d", enabled)
	line("# HELP nfr_tunnels_online Enabled tunnels whose agent is connected.")
	line("# TYPE nfr_tunnels_online gauge")
	line("nfr_tunnels_online %d", online)

	line("# HELP nfr_tunnel_bytes_total Bytes forwarded through a tunnel.")
	line("# TYPE nfr_tunnel_bytes_total counter")
	for _, t := range tunnels {
		line(`nfr_tunnel_bytes_total{id=%q,name=%q,direction="in"} %d`, t.ID, t.Name, t.In)
		line(`nfr_tunnel_bytes_total{id=%q,name=%q,direction="out"} %d`, t.ID, t.Name, t.Out)
	}
	line("# HELP nfr_tunnel_connections_total Accepted public connections.")
	line("# TYPE nfr_tunnel_connections_total counter")
	for _, t := range tunnels {
		line(`nfr_tunnel_connections_total{id=%q,name=%q} %d`, t.ID, t.Name, t.Conns)
	}
	line("# HELP nfr_tunnel_errors_total Connections dropped before reaching the agent.")
	line("# TYPE nfr_tunnel_errors_total counter")
	for _, t := range tunnels {
		line(`nfr_tunnel_errors_total{id=%q,name=%q} %d`, t.ID, t.Name, t.Errs)
	}
	line("# HELP nfr_tunnel_active Connections currently in flight.")
	line("# TYPE nfr_tunnel_active gauge")
	for _, t := range tunnels {
		line(`nfr_tunnel_active{id=%q,name=%q} %d`, t.ID, t.Name, t.Active)
	}
	line("# HELP nfr_tunnel_bytes_per_second Current throughput, five minute window.")
	line("# TYPE nfr_tunnel_bytes_per_second gauge")
	for _, t := range tunnels {
		line(`nfr_tunnel_bytes_per_second{id=%q,name=%q,direction="in"} %.0f`, t.ID, t.Name, t.InRate)
		line(`nfr_tunnel_bytes_per_second{id=%q,name=%q,direction="out"} %.0f`, t.ID, t.Name, t.OutRate)
	}
	return b.String()
}
