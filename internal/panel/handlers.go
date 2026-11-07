package panel

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/solitudeit/nfr/internal/api"
)

// serverCard is one selectable instance on the tunnel screens.
type serverCard struct {
	ID        string
	Name      string
	URL       string
	Local     bool
	Reachable bool
	Version   string
	Nodes     int
	Tunnels   int
}

type overviewData struct {
	base
	Tunnels []api.Tunnel
	Nodes   []api.Node
	Online  int
	Enabled int
	In      int64
	Out     int64
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request, s *session) {
	tunnels := p.srv.Tunnels()
	nodes := p.srv.Nodes()
	d := overviewData{base: p.newBase(r, s, "overview", "Overview", "overview"), Tunnels: tunnels, Nodes: nodes}
	for _, n := range nodes {
		if n.Online {
			d.Online++
		}
	}
	for _, t := range tunnels {
		if t.Enabled {
			d.Enabled++
		}
		d.In += t.In
		d.Out += t.Out
	}
	p.render(w, r, "overview", d)
}

type tunnelsData struct {
	base
	Cards     []serverCard
	Current   string
	CurName   string
	CurURL    string
	IsLocal   bool
	RemoteErr string
	Tunnels   []api.Tunnel
	Nodes     []api.Node
	PortRange [2]int
	Edit      *api.Tunnel
}

func (p *Panel) cards() []serverCard {
	out := []serverCard{{
		ID: "local", Name: p.srv.Info().Name, URL: p.srv.Info().PanelURL,
		Local: true, Reachable: true, Version: p.srv.Info().Version,
		Nodes: len(p.srv.Nodes()), Tunnels: len(p.srv.Tunnels()),
	}}
	for _, rem := range p.srv.Remotes() {
		out = append(out, serverCard{
			ID: rem.ID, Name: rem.Name, URL: rem.URL, Reachable: rem.Reachable,
			Version: rem.Version,
		})
	}
	return out
}

func (p *Panel) tunnels(w http.ResponseWriter, r *http.Request, s *session) {
	sel := r.URL.Query().Get("server")
	d := tunnelsData{
		base:      p.newBase(r, s, "tunnels", "Tunnels", "tunnels"),
		PortRange: p.srv.Settings().PortRange,
	}
	if sel == "" {
		p.refreshRemotes(r.Context())
		d.Cards = p.cards()
		p.render(w, r, "tunnels", d)
		return
	}
	d.Current = sel
	d.IsLocal = sel == "local"
	if d.IsLocal {
		d.CurName = p.srv.Info().Name
		d.CurURL = p.srv.Info().PanelURL
		d.Tunnels = p.srv.Tunnels()
		d.Nodes = p.srv.Nodes()
	} else {
		c, rec, err := p.srv.RemoteClient(sel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d.CurName = rec.Name
		d.CurURL = rec.URL
		ctx := r.Context()
		tunnels, err := c.Tunnels(ctx)
		if err != nil {
			d.RemoteErr = err.Error()
		} else {
			d.Tunnels = tunnels
			if nodes, err := c.Nodes(ctx); err == nil {
				d.Nodes = nodes
			}
		}
	}
	p.render(w, r, "tunnels", d)
}

func (p *Panel) tunnelForm(w http.ResponseWriter, r *http.Request, s *session, existing *api.Tunnel) {
	sel := r.URL.Query().Get("server")
	if sel == "" {
		sel = "local"
	}
	d := tunnelsData{
		base:      p.newBase(r, s, "tunnels", "New tunnel", "tunnel_form"),
		Current:   sel,
		IsLocal:   sel == "local",
		PortRange: p.srv.Settings().PortRange,
		Edit:      existing,
	}
	if d.IsLocal {
		d.CurName = p.srv.Info().Name
		d.Nodes = p.srv.Nodes()
	} else {
		c, rec, err := p.srv.RemoteClient(sel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d.CurName = rec.Name
		if nodes, err := c.Nodes(r.Context()); err == nil {
			d.Nodes = nodes
		}
	}
	p.render(w, r, "tunnel_form", d)
}

func (p *Panel) editForm(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.URL.Query().Get("id")
	sel := r.URL.Query().Get("server")
	if sel == "" {
		sel = "local"
	}
	if sel == "local" {
		t, ok := p.srv.Tunnel(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.tunnelForm(w, r, s, &t)
		return
	}
	c, _, err := p.srv.RemoteClient(sel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	list, err := c.Tunnels(r.Context())
	if err != nil {
		p.backErr(w, r, "/_nfr/tunnels", err)
		return
	}
	for i := range list {
		if list[i].ID == id {
			p.tunnelForm(w, r, s, &list[i])
			return
		}
	}
	http.NotFound(w, r)
}

func (p *Panel) tunnelSave(w http.ResponseWriter, r *http.Request, s *session) {
	if err := r.ParseForm(); err != nil {
		p.backErr(w, r, "/_nfr/tunnels", err)
		return
	}
	sel := r.FormValue("server")
	if sel == "" {
		sel = "local"
	}
	port, _ := strconv.Atoi(r.FormValue("remote_port"))
	enabled := r.FormValue("enabled") != ""
	req := api.TunnelRequest{
		Name:       strings.TrimSpace(r.FormValue("name")),
		NodeID:     r.FormValue("node_id"),
		Type:       r.FormValue("type"),
		Local:      strings.TrimSpace(r.FormValue("local")),
		RemotePort: port,
		Hosts:      splitHosts(r.FormValue("hosts")),
		Enabled:    &enabled,
		LeaseID:    r.FormValue("lease_id"),
	}
	id := r.FormValue("id")
	var err error
	if sel == "local" {
		if id == "" {
			_, err = p.srv.CreateTunnel(req)
		} else {
			_, err = p.srv.UpdateTunnel(id, req)
		}
	} else {
		c, _, cerr := p.srv.RemoteClient(sel)
		if cerr != nil {
			err = cerr
		} else if id == "" {
			_, err = c.CreateTunnel(r.Context(), req)
		} else {
			_, err = c.UpdateTunnel(r.Context(), id, req)
		}
	}
	if err != nil {
		p.backErr(w, r, "/_nfr/tunnels", err)
		return
	}
	if req.Type == "http" {
		p.ok(w, r, "/_nfr/tunnels", "tunnel saved")
		return
	}
	p.ok(w, r, "/_nfr/tunnels", "tunnel saved")
}

func (p *Panel) tunnelDelete(w http.ResponseWriter, r *http.Request, s *session) {
	_ = r.ParseForm()
	sel := r.FormValue("server")
	id := r.FormValue("id")
	var err error
	if sel == "" || sel == "local" {
		err = p.srv.DeleteTunnel(id)
	} else {
		c, _, cerr := p.srv.RemoteClient(sel)
		if cerr != nil {
			err = cerr
		} else {
			err = c.DeleteTunnel(r.Context(), id)
		}
	}
	if err != nil {
		p.backErr(w, r, "/_nfr/tunnels", err)
		return
	}
	p.ok(w, r, "/_nfr/tunnels", "tunnel deleted")
}

func (p *Panel) tunnelToggle(w http.ResponseWriter, r *http.Request, s *session) {
	_ = r.ParseForm()
	sel := r.FormValue("server")
	id := r.FormValue("id")
	on := r.FormValue("enabled") == "true"
	var err error
	if sel == "" || sel == "local" {
		err = p.srv.SetTunnelEnabled(id, on)
	} else {
		c, _, cerr := p.srv.RemoteClient(sel)
		if cerr != nil {
			err = cerr
		} else {
			_, err = c.UpdateTunnel(r.Context(), id, api.TunnelRequest{Enabled: &on})
		}
	}
	if err != nil {
		p.backErr(w, r, "/_nfr/tunnels", err)
		return
	}
	p.ok(w, r, "/_nfr/tunnels", "tunnel updated")
}

type nodesData struct {
	base
	Nodes   []api.Node
	Code    string
	Command string
}

func (p *Panel) nodes(w http.ResponseWriter, r *http.Request, s *session) {
	d := nodesData{base: p.newBase(r, s, "nodes", "Nodes", "nodes"), Nodes: p.srv.Nodes()}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		req := api.JoinCodeRequest{Name: strings.TrimSpace(r.FormValue("name"))}
		resp, err := p.srv.CreateJoinCode(req)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Code = resp.Code
			d.Command = resp.Command
		}
	}
	p.render(w, r, "nodes", d)
}

type serversData struct {
	base
	Cards []serverCard
	Pin   string
}

func (p *Panel) servers(w http.ResponseWriter, r *http.Request, s *session) {
	d := serversData{base: p.newBase(r, s, "servers", "Servers", "servers")}
	p.refreshRemotes(r.Context())
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		if r.FormValue("action") == "delete" {
			if err := p.srv.RemoveRemote(r.FormValue("id")); err != nil {
				d.Error = err.Error()
			} else {
				d.Notice = "server removed"
			}
			d.Cards = p.cards()[1:]
			d.Pin = p.srv.Pin()
			p.render(w, r, "servers", d)
			return
		}
		req := api.RemoteRequest{
			Name:   strings.TrimSpace(r.FormValue("name")),
			URL:    strings.TrimSpace(r.FormValue("url")),
			APIKey: strings.TrimSpace(r.FormValue("api_key")),
			Pin:    strings.TrimSpace(r.FormValue("pin")),
		}
		rec, err := p.srv.AddRemote(r.Context(), req)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Notice = "added " + rec.Name
		}
	}
	d.Cards = p.cards()[1:]
	d.Pin = p.srv.Pin()
	p.render(w, r, "servers", d)
}

type settingsData struct {
	base
	Name       string
	Domain     string
	APIEnabled bool
	PortLow    int
	PortHigh   int
	Email      string
	Pin        string
	PanelURL   string
	CertSource string
	CertMode   string
	CertExpiry string
}

func (p *Panel) settings(w http.ResponseWriter, r *http.Request, s *session) {
	d := settingsData{base: p.newBase(r, s, "settings", "Settings", "settings")}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		action := r.FormValue("action")
		switch action {
		case "password":
			old := r.FormValue("old_password")
			next := r.FormValue("new_password")
			if len(next) < 8 {
				d.Error = "the new password needs at least 8 characters"
				break
			}
			if !checkPassword(p.srv.PanelPasswordHash(), old) {
				d.Error = "the current password is wrong"
				break
			}
			hash, err := hashPassword(next)
			if err != nil {
				d.Error = err.Error()
				break
			}
			if err := p.srv.SetPanelPasswordHash(hash); err != nil {
				d.Error = err.Error()
				break
			}
			d.Notice = "password updated"
		default:
			low, _ := strconv.Atoi(r.FormValue("port_low"))
			high, _ := strconv.Atoi(r.FormValue("port_high"))
			err := p.srv.UpdateSettings(
				r.FormValue("domain"),
				r.FormValue("api_enabled") != "",
				[2]int{low, high},
				r.FormValue("email"),
			)
			if err != nil {
				d.Error = err.Error()
				break
			}
			if name := strings.TrimSpace(r.FormValue("name")); name != "" {
				if err := p.srv.SetInstanceName(name); err != nil {
					d.Error = err.Error()
					break
				}
			}
			d.Notice = "settings saved"
		}
	}
	set := p.srv.Settings()
	info := p.srv.Info()
	d.Name = info.Name
	d.Domain = set.Domain
	d.APIEnabled = set.APIEnabled
	d.PortLow, d.PortHigh = set.PortRange[0], set.PortRange[1]
	d.Email = set.HTTPSEmail
	d.Pin = info.Pinned
	d.PanelURL = info.PanelURL
	p.render(w, r, "settings", d)
}

type keysData struct {
	base
	Keys   []api.APIKey
	NewKey string
}

func (p *Panel) keys(w http.ResponseWriter, r *http.Request, s *session) {
	d := keysData{base: p.newBase(r, s, "keys", "API keys", "keys")}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		switch r.FormValue("action") {
		case "delete":
			if err := p.srv.DeleteAPIKey(r.FormValue("id")); err != nil {
				d.Error = err.Error()
			}
		default:
			req := api.APIKeyRequest{
				Label: strings.TrimSpace(r.FormValue("label")),
				Scope: r.FormValue("scope"),
			}
			if req.Scope == "" {
				req.Scope = "read"
			}
			resp, err := p.srv.CreateAPIKey(req)
			if err != nil {
				d.Error = err.Error()
			} else {
				d.NewKey = resp.Key
			}
		}
	}
	d.Keys = p.srv.APIKeys()
	p.render(w, r, "keys", d)
}

// allocate answers the "get a port" button without leaving the page.
func (p *Panel) allocate(w http.ResponseWriter, r *http.Request, s *session) {
	sel := r.URL.Query().Get("server")
	if sel == "" || sel == "local" {
		resp, err := p.srv.AllocatePort(r.FormValue("tunnel_id"))
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	c, _, err := p.srv.RemoteClient(sel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp, err := c.Allocate(r.Context(), r.FormValue("tunnel_id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) state(w http.ResponseWriter, r *http.Request, s *session) {
	writeJSON(w, http.StatusOK, map[string]any{
		"info":    p.srv.Info(),
		"tunnels": p.srv.Tunnels(),
		"nodes":   p.srv.Nodes(),
	})
}

// ---------------------------------------------------------------------------
// helpers

func (p *Panel) backErr(w http.ResponseWriter, r *http.Request, path string, err error) {
	sel := r.FormValue("server")
	if sel == "" {
		sel = "local"
	}
	q := "?server=" + sel + "&err=" + url.QueryEscape(err.Error())
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}

func splitHosts(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
