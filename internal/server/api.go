package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/federation"
	"github.com/solitudeit/nfr/internal/store"
)

type ctxKey int

const (
	ctxScope ctxKey = iota
	ctxKeyID
)

// buildAPI wires the REST surface. The panel uses the same routes, so every
// feature must be reachable over HTTP before it gets a screen.
func (s *Server) buildAPI() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/info", s.needScope("read", s.apiInfo))
	mux.HandleFunc("GET /api/v1/health", s.needScope("read", s.apiHealth))
	mux.HandleFunc("GET /api/v1/certs", s.needScope("read", s.apiCert))

	mux.HandleFunc("GET /api/v1/tunnels", s.needScope("read", s.apiListTunnels))
	mux.HandleFunc("POST /api/v1/tunnels", s.needScope("write", s.apiCreateTunnel))
	mux.HandleFunc("GET /api/v1/tunnels/{id}", s.needScope("read", s.apiGetTunnel))
	mux.HandleFunc("PATCH /api/v1/tunnels/{id}", s.needScope("write", s.apiUpdateTunnel))
	mux.HandleFunc("DELETE /api/v1/tunnels/{id}", s.needScope("write", s.apiDeleteTunnel))
	mux.HandleFunc("POST /api/v1/ports/allocate", s.needScope("write", s.apiAllocate))

	mux.HandleFunc("GET /api/v1/nodes", s.needScope("read", s.apiListNodes))
	mux.HandleFunc("PATCH /api/v1/nodes/{id}", s.needScope("write", s.apiRenameNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", s.needScope("write", s.apiDeleteNode))
	mux.HandleFunc("POST /api/v1/nodes/join-codes", s.needScope("write", s.apiJoinCode))

	mux.HandleFunc("GET /api/v1/servers", s.needScope("admin", s.apiListServers))
	mux.HandleFunc("POST /api/v1/servers", s.needScope("admin", s.apiAddServer))
	mux.HandleFunc("DELETE /api/v1/servers/{id}", s.needScope("admin", s.apiDeleteServer))

	mux.HandleFunc("GET /api/v1/api-keys", s.needScope("admin", s.apiListKeys))
	mux.HandleFunc("POST /api/v1/api-keys", s.needScope("admin", s.apiCreateKey))
	mux.HandleFunc("DELETE /api/v1/api-keys/{id}", s.needScope("admin", s.apiDeleteKey))

	mux.HandleFunc("GET /api/v1/settings", s.needScope("admin", s.apiGetSettings))
	mux.HandleFunc("PATCH /api/v1/settings", s.needScope("admin", s.apiPatchSettings))

	mux.HandleFunc("GET /api/v1/events", s.needScope("read", s.apiEvents))

	return s.apiAuth(mux)
}

// needScope enforces the minimum credential level for a handler.
func (s *Server) needScope(want string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, _ := r.Context().Value(ctxScope).(string)
		if !auth.ScopeAllows(scope, want) {
			writeErr(w, http.StatusForbidden, "insufficient_scope", "this key is read-only")
			return
		}
		next(w, r)
	}
}

// apiAuth resolves the bearer token and records its use.
func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			secret = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		if secret == "" {
			secret = r.URL.Query().Get("access_token")
		}
		var (
			enabled bool
			scope   string
			keyID   string
			allowOK bool
			ip      = clientIP(r)
		)
		s.st.View(func(st *store.State) error {
			enabled = st.Settings.APIEnabled
			if !enabled || secret == "" {
				return nil
			}
			rec, ok := st.APIKeyByHash(auth.Hash(secret))
			if !ok {
				return nil
			}
			if !allowFrom(rec.AllowFrom, ip) {
				return nil
			}
			scope, keyID, allowOK = rec.Scope, rec.ID, true
			return nil
		})
		if !enabled {
			writeErr(w, http.StatusForbidden, "api_disabled", "the API is disabled on this instance")
			return
		}
		if secret == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "send an Authorization: Bearer key")
			return
		}
		if !allowOK {
			writeErr(w, http.StatusForbidden, "forbidden", "this key is not valid from this address")
			return
		}
		now := time.Now()
		s.mu.Lock()
		s.lastUsed[keyID] = now
		s.mu.Unlock()
		ctx := context.WithValue(r.Context(), ctxScope, scope)
		ctx = context.WithValue(ctx, ctxKeyID, keyID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func allowFrom(list []string, ip string) bool {
	if len(list) == 0 {
		return true
	}
	addr := net.ParseIP(ip)
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			if addr != nil && cidr.Contains(addr) {
				return true
			}
			continue
		}
		if entry == ip {
			return true
		}
	}
	return false
}

// target resolves ?server= so the panel can operate a managed instance with
// exactly the same call it uses locally.
func (s *Server) target(r *http.Request) (string, *federation.Client, error) {
	id := r.URL.Query().Get("server")
	if id == "" {
		return "", nil, nil
	}
	c, _, err := s.RemoteClient(id)
	if err != nil {
		return "", nil, err
	}
	return id, c, nil
}

func (s *Server) apiInfo(w http.ResponseWriter, r *http.Request) {
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
	} else if c != nil {
		info, err := c.Info(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, info)
		return
	}
	writeJSON(w, http.StatusOK, s.Info())
}

// apiCert reports how this instance's TLS certificate can be verified.
func (s *Server) apiCert(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.CertSummary())
}

func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func (s *Server) apiListTunnels(w http.ResponseWriter, r *http.Request) {
	_, c, err := s.target(r)
	if err != nil {
		writeErr2(w, err)
		return
	}
	if c != nil {
		list, err := c.Tunnels(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	writeJSON(w, http.StatusOK, s.Tunnels())
}

func (s *Server) apiGetTunnel(w http.ResponseWriter, r *http.Request) {
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		list, err := c.Tunnels(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		for _, t := range list {
			if t.ID == r.PathValue("id") {
				writeJSON(w, http.StatusOK, t)
				return
			}
		}
		writeErr(w, http.StatusNotFound, "not_found", "tunnel not found")
		return
	}
	t, ok := s.Tunnel(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "tunnel not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) apiCreateTunnel(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelRequest
	if !decode(w, r, &req) {
		return
	}
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		t, err := c.CreateTunnel(r.Context(), req)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, t)
		return
	}
	t, err := s.CreateTunnel(req)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) apiUpdateTunnel(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelRequest
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		t, err := c.UpdateTunnel(r.Context(), id, req)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, t)
		return
	}
	t, err := s.UpdateTunnel(id, req)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) apiDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		if err := c.DeleteTunnel(r.Context(), id); err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.DeleteTunnel(id); err != nil {
		writeErr2(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiAllocate(w http.ResponseWriter, r *http.Request) {
	var req api.AllocateRequest
	if !decode(w, r, &req) {
		return
	}
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		resp, err := c.Allocate(r.Context(), req.TunnelID, req.Network)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp, err := s.AllocatePort(req.TunnelID, req.Network)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) apiListNodes(w http.ResponseWriter, r *http.Request) {
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		list, err := c.Nodes(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	writeJSON(w, http.StatusOK, s.Nodes())
}

func (s *Server) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		writeErr(w, http.StatusBadRequest, "unsupported", "manage nodes on the instance that owns them")
		return
	}
	if err := s.RemoveNode(r.PathValue("id")); err != nil {
		writeErr2(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRenameNode(w http.ResponseWriter, r *http.Request) {
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		writeErr(w, http.StatusBadRequest, "unsupported", "manage nodes on the instance that owns them")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.SetNodeName(r.PathValue("id"), req.Name); err != nil {
		writeErr2(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiJoinCode(w http.ResponseWriter, r *http.Request) {
	var req api.JoinCodeRequest
	if !decode(w, r, &req) {
		return
	}
	if _, c, err := s.target(r); err != nil {
		writeErr2(w, err)
		return
	} else if c != nil {
		resp, err := c.JoinCode(r.Context(), req)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "remote_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp, err := s.CreateJoinCode(req)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) apiListServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Remotes())
}

func (s *Server) apiAddServer(w http.ResponseWriter, r *http.Request) {
	var req api.RemoteRequest
	if !decode(w, r, &req) {
		return
	}
	rec, err := s.AddRemote(r.Context(), req)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) apiDeleteServer(w http.ResponseWriter, r *http.Request) {
	if err := s.RemoveRemote(r.PathValue("id")); err != nil {
		writeErr2(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.APIKeys())
}

func (s *Server) apiCreateKey(w http.ResponseWriter, r *http.Request) {
	var req api.APIKeyRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.CreateAPIKey(req)
	if err != nil {
		writeErr2(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) apiDeleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteAPIKey(r.PathValue("id")); err != nil {
		writeErr2(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type settingsView struct {
	Domain     string `json:"domain"`
	APIEnabled bool   `json:"api_enabled"`
	PortRange  [2]int `json:"port_range"`
	HTTPSEmail string `json:"https_email"`
}

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	st := s.st.Snapshot()
	writeJSON(w, http.StatusOK, settingsView{
		Domain: st.Settings.Domain, APIEnabled: st.Settings.APIEnabled,
		PortRange: st.Settings.PortRange, HTTPSEmail: st.Settings.HTTPSEmail,
	})
}

func (s *Server) apiPatchSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsView
	if !decode(w, r, &req) {
		return
	}
	if err := s.UpdateSettings(req.Domain, req.APIEnabled, req.PortRange, req.HTTPSEmail); err != nil {
		writeErr2(w, err)
		return
	}
	s.apiGetSettings(w, r)
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no_stream", "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch, cancel := s.hub.subscribe()
	defer cancel()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.Error{Error: msg, Code: code})
}

func writeErr2(w http.ResponseWriter, err error) {
	status, code := statusOf(err)
	writeErr(w, status, code, err.Error())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return false
	}
	return true
}
