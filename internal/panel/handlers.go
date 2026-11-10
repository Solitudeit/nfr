package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/api"
)

// serverCard is one selectable instance on the tunnel screens.
type serverCard struct {
	ID         string
	Name       string
	URL        string
	Local      bool
	Reachable  bool
	Version    string
	Nodes      int
	Tunnels    int
	Region     string // Chinese country name from the instance's address
	RegionCode string // ISO 3166-1 alpha-2, for the flag
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
	d := overviewData{base: p.newBase(r, s, "overview", "概览", "overview"), Tunnels: tunnels, Nodes: nodes}
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
	CurRegion string
	IsLocal   bool
	RemoteErr string
	Tunnels   []api.Tunnel
	Nodes     []api.Node
	// OnlineNodes counts agents holding a control connection right now. The
	// form uses it to say plainly that nothing can carry traffic yet instead of
	// letting an operator believe a tunnel is live.
	OnlineNodes int
	LocalNodeOn bool
	// LocalTarget is the console host offered as a tunnel target when the
	// operator turned that on. It is kept out of Nodes on purpose: it is not an
	// agent and must never look like one.
	LocalTarget *api.Node
	// Draft marks a new tunnel that cannot be live yet: no agent is connected,
	// so the form saves it switched off and says why.
	Draft     bool
	PortRange [2]int
	Edit      *api.Tunnel
	LocalHost string // Edit.Local split for the two-field form
	LocalPort string
	SelNode   *api.Node // node picked outside the form (entry link / edit); nil → form shows the picker
	// Codes carries the live join command of every awaiting tunnel on the
	// current server, keyed by tunnel id, so the card can show what a new
	// device must run without a second request.
	Codes map[string]api.JoinCodeResponse
}

// cards renders every instance the console can reach, the local one first.
// Each card carries the country its address belongs to: with one machine that
// is the country it sits in, with several it is how an operator tells them
// apart. Lookups are cached and time boxed, so an offline geo service costs at
// most geoBudget and only loses the label.
func (p *Panel) cards(ctx context.Context) []serverCard {
	info := p.srv.Info()
	out := []serverCard{{
		ID: "local", Name: info.Name, URL: info.PanelURL,
		Local: true, Reachable: true, Version: info.Version,
		Nodes: len(p.srv.Nodes()), Tunnels: len(p.srv.Tunnels()),
	}}
	for _, rem := range p.srv.Remotes() {
		out = append(out, serverCard{
			ID: rem.ID, Name: rem.Name, URL: rem.URL, Reachable: rem.Reachable,
			Version: rem.Version, Nodes: rem.Nodes, Tunnels: rem.Tunnels,
		})
	}
	p.labelRegions(ctx, out)
	return out
}

// labelRegions fills Region and RegionCode for every card, in parallel. The
// local card falls back to this machine's own exit address when its public URL
// is a LAN name, which is the normal case for a home server.
func (p *Panel) labelRegions(parent context.Context, cards []serverCard) {
	ctx, cancel := context.WithTimeout(parent, geoBudget)
	defer cancel()
	regions := make([]region, len(cards))
	var wg sync.WaitGroup
	for i := range cards {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			regions[i] = p.geo.locate(ctx, geoQuery{Host: cards[i].URL, FallbackSelf: cards[i].Local})
		}(i)
	}
	wg.Wait()
	for i := range cards {
		cards[i].Region = regions[i].Country
		cards[i].RegionCode = regions[i].Code
	}
}

// region names the country one instance sits in, for the page that has already
// been narrowed down to it. It is the same lookup the cards use, so it costs
// nothing after the first view.
func (p *Panel) region(parent context.Context, rawURL string, local bool) string {
	ctx, cancel := context.WithTimeout(parent, geoBudget)
	defer cancel()
	return p.geo.locate(ctx, geoQuery{Host: rawURL, FallbackSelf: local}).Country
}

// labelNodes fills in the country every node sits in, so the console never has
// to say "this host" to tell the built-in node apart: the card carries the same
// kind of country label the instances get. An agent is located by the address
// it connects from; the built-in node falls back to this machine's own exit
// address when its public URL is a LAN name.
func (p *Panel) labelNodes(parent context.Context, nodes []api.Node) {
	if len(nodes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, geoBudget)
	defer cancel()
	regions := make([]region, len(nodes))
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := geoQuery{Host: nodes[i].RemoteAddr, FallbackSelf: nodes[i].Local}
			if q.Host == "" && !q.FallbackSelf {
				// An agent that has never connected leaves no address to
				// look up; the card simply goes without a country.
				return
			}
			regions[i] = p.geo.locate(ctx, q)
		}(i)
	}
	wg.Wait()
	for i := range nodes {
		nodes[i].Region = regions[i].Country
	}
}

func (p *Panel) tunnels(w http.ResponseWriter, r *http.Request, s *session) {
	sel := r.URL.Query().Get("server")
	d := tunnelsData{
		base:      p.newBase(r, s, "tunnels", "隧道", "tunnels"),
		PortRange: p.srv.Settings().PortRange,
	}
	if sel == "" {
		// Federated instances are an advanced feature: with none configured the
		// tunnel page opens straight on this instance's tunnels instead of
		// asking which server to manage.
		p.refreshRemotes(r.Context())
		if len(p.srv.Remotes()) == 0 {
			http.Redirect(w, r, "/nfr/tunnels?server=local", http.StatusSeeOther)
			return
		}
		d.Cards = p.cards(r.Context())
		p.render(w, r, "tunnels", d)
		return
	}
	d.Current = sel
	d.IsLocal = sel == "local"
	if d.IsLocal {
		d.CurName = p.srv.Info().Name
		d.CurURL = p.srv.Info().PanelURL
		d.Tunnels = p.srv.Tunnels()
		d.Codes = p.joinCodes(d.Tunnels)
		d.Nodes, d.LocalTarget = splitNodes(p.srv.Nodes())
		d.OnlineNodes = countOnline(d.Nodes)
		d.LocalNodeOn = d.LocalTarget != nil
		p.labelNodes(r.Context(), d.Nodes)
		if d.LocalTarget != nil {
			p.labelNodes(r.Context(), []api.Node{*d.LocalTarget})
		}
		d.CurRegion = p.region(r.Context(), d.CurURL, true)
	} else {
		c, rec, err := p.srv.RemoteClient(sel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d.CurName = rec.Name
		d.CurURL = rec.URL
		d.CurRegion = p.region(r.Context(), rec.URL, false)
		ctx := r.Context()
		tunnels, err := c.Tunnels(ctx)
		if err != nil {
			d.RemoteErr = err.Error()
		} else {
			d.Tunnels = tunnels
			d.Codes = p.joinCodes(tunnels)
			if nodes, err := c.Nodes(ctx); err == nil {
				d.Nodes, d.LocalTarget = splitNodes(nodes)
				d.OnlineNodes = countOnline(d.Nodes)
				d.LocalNodeOn = d.LocalTarget != nil
				p.labelNodes(r.Context(), d.Nodes)
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
	title := "新建隧道"
	if existing != nil {
		title = "编辑隧道"
	}
	d := tunnelsData{
		base:      p.newBase(r, s, "tunnels", title, "tunnel_form"),
		Current:   sel,
		IsLocal:   sel == "local",
		PortRange: p.srv.Settings().PortRange,
		Edit:      existing,
	}
	if existing != nil {
		d.LocalHost, d.LocalPort = splitLocal(existing.Local)
	}
	if d.IsLocal {
		d.CurName = p.srv.Info().Name
		d.Nodes, d.LocalTarget = splitNodes(p.srv.Nodes())
		d.OnlineNodes = countOnline(d.Nodes)
		d.LocalNodeOn = d.LocalTarget != nil
		p.labelNodes(r.Context(), d.Nodes)
		if d.LocalTarget != nil {
			p.labelNodes(r.Context(), []api.Node{*d.LocalTarget})
		}
		d.CurRegion = p.region(r.Context(), p.srv.Info().PanelURL, true)
	} else {
		c, rec, err := p.srv.RemoteClient(sel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d.CurName = rec.Name
		d.CurRegion = p.region(r.Context(), rec.URL, false)
		if nodes, err := c.Nodes(r.Context()); err == nil {
			d.Nodes, d.LocalTarget = splitNodes(nodes)
			d.OnlineNodes = countOnline(d.Nodes)
			d.LocalNodeOn = d.LocalTarget != nil
			p.labelNodes(r.Context(), d.Nodes)
		}
	}
	// Draft marks a remote new-form with nothing online: those save switched
	// off. The local form never drafts — the new-device flow must create an
	// enabled awaiting tunnel so the join command works the moment a device
	// redeems it.
	d.Draft = existing == nil && !d.IsLocal && d.OnlineNodes == 0
	// The node is picked outside the form: either it is a property of the
	// tunnel being edited, or it came in on the entry URL (?node=). When it
	// resolves, the form shows a locked confirmation instead of the picker.
	pick := func(id string) {
		if id == "" {
			return
		}
		if d.LocalTarget != nil && d.LocalTarget.ID == id {
			d.SelNode = d.LocalTarget
			return
		}
		for i := range d.Nodes {
			if d.Nodes[i].ID == id {
				d.SelNode = &d.Nodes[i]
				return
			}
		}
	}
	if existing != nil {
		pick(existing.NodeID)
	} else {
		pick(r.URL.Query().Get("node"))
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
		p.backErr(w, r, "/nfr/tunnels", err)
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

// joinCodes collects the live join code of every awaiting tunnel, keyed by
// tunnel id. The panel renders each awaiting card's command from it; joined
// tunnels get no entry and the template falls back to nothing.
func (p *Panel) joinCodes(tunnels []api.Tunnel) map[string]api.JoinCodeResponse {
	codes := map[string]api.JoinCodeResponse{}
	for _, t := range tunnels {
		if !t.Awaiting {
			continue
		}
		if jc, ok := p.srv.TunnelJoinCode(t.ID); ok {
			codes[t.ID] = jc
		}
	}
	return codes
}

func (p *Panel) tunnelSave(w http.ResponseWriter, r *http.Request, s *session) {
	if err := r.ParseForm(); err != nil {
		p.backErr(w, r, "/nfr/tunnels", err)
		return
	}
	sel := r.FormValue("server")
	if sel == "" {
		sel = "local"
	}
	port, _ := strconv.Atoi(r.FormValue("remote_port"))
	enabled := formSwitch(r.FormValue("enabled"))
	bw, _ := strconv.Atoi(r.FormValue("bandwidth_kb"))
	quota, _ := strconv.ParseInt(r.FormValue("quota_mb"), 10, 64)
	local := strings.TrimSpace(r.FormValue("local"))
	if local == "" {
		// The form posts two fields; join them for the backend contract.
		lh := strings.TrimSpace(r.FormValue("local_host"))
		lp := strings.TrimSpace(r.FormValue("local_port"))
		if lh != "" && lp != "" {
			local = lh + ":" + lp
		}
	}
	req := api.TunnelRequest{
		Name:        strings.TrimSpace(r.FormValue("name")),
		NodeID:      r.FormValue("node_id"),
		Type:        r.FormValue("type"),
		Local:       local,
		RemotePort:  port,
		Hosts:       splitHosts(r.FormValue("hosts")),
		Enabled:     &enabled,
		LeaseID:     r.FormValue("lease_id"),
		BandwidthKB: &bw,
		QuotaMB:     &quota,
	}
	id := r.FormValue("id")
	var err error
	if sel == "local" {
		if id == "" && req.NodeID == "new" {
			// New-device flow: the tunnel is created before any node exists
			// and the panel sends the operator back to its card, which shows
			// the bound join command.
			var created api.Tunnel
			created, _, err = p.srv.CreateTunnelAwaiting(req, 30*time.Minute)
			if err == nil {
				http.Redirect(w, r, "/nfr/tunnels?server=local#t-"+created.ID, http.StatusSeeOther)
				return
			}
		} else if id == "" {
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
		p.backErr(w, r, "/nfr/tunnels", err)
		return
	}
	if req.Type == "http" {
		p.ok(w, r, "/nfr/tunnels", "隧道已保存")
		return
	}
	p.ok(w, r, "/nfr/tunnels", "隧道已保存")
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
		p.backErr(w, r, "/nfr/tunnels", err)
		return
	}
	p.ok(w, r, "/nfr/tunnels", "隧道已删除")
}

// tunnelRegenCode replaces the join code bound to an awaiting tunnel: the old
// command stops working and the card shows a fresh one.
func (p *Panel) tunnelRegenCode(w http.ResponseWriter, r *http.Request, s *session) {
	_ = r.ParseForm()
	sel := r.FormValue("server")
	if sel == "" {
		sel = "local"
	}
	id := r.FormValue("id")
	if sel != "local" {
		// Regen is a local-server capability for now: the brief binds it to
		// this instance's awaiting tunnels.
		p.backErr(w, r, "/nfr/tunnels", errors.New("只有本服务器支持重新生成加入命令"))
		return
	}
	if _, err := p.srv.RegenerateTunnelCode(id); err != nil {
		p.backErr(w, r, "/nfr/tunnels", err)
		return
	}
	p.ok(w, r, "/nfr/tunnels", "加入命令已重新生成")
}

func (p *Panel) tunnelToggle(w http.ResponseWriter, r *http.Request, s *session) {
	_ = r.ParseForm()
	sel := r.FormValue("server")
	id := r.FormValue("id")
	on := formSwitch(r.FormValue("enabled"))
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
		p.backErr(w, r, "/nfr/tunnels", err)
		return
	}
	p.ok(w, r, "/nfr/tunnels", "隧道已更新")
}

// splitNodes separates real agents from the console host, which the server
// only offers as a target when the operator opted in.
func splitNodes(nodes []api.Node) (agents []api.Node, local *api.Node) {
	agents = make([]api.Node, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Local {
			v := nodes[i]
			local = &v
			continue
		}
		agents = append(agents, nodes[i])
	}
	return agents, local
}

// countOnline reports how many agents currently hold a control connection.
func countOnline(nodes []api.Node) int {
	n := 0
	for _, v := range nodes {
		if v.Online {
			n++
		}
	}
	return n
}

type nodesData struct {
	base
	Nodes       []api.Node
	LocalTarget *api.Node
	OnlineNodes int
	LocalNodeOn bool
	Code        string
	Command     string
	JoinWarning string
}

func (p *Panel) nodes(w http.ResponseWriter, r *http.Request, s *session) {
	d := nodesData{base: p.newBase(r, s, "nodes", "节点", "nodes")}
	d.Nodes, d.LocalTarget = splitNodes(p.srv.Nodes())
	d.OnlineNodes = countOnline(d.Nodes)
	d.LocalNodeOn = d.LocalTarget != nil
	p.labelNodes(r.Context(), d.Nodes)
	if d.LocalTarget != nil {
		p.labelNodes(r.Context(), []api.Node{*d.LocalTarget})
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		req := api.JoinCodeRequest{Name: strings.TrimSpace(r.FormValue("name"))}
		resp, err := p.srv.CreateJoinCode(req)
		if err != nil {
			d.Error = err.Error()
		} else {
			d.Code = resp.Code
			d.Command = resp.Command
			d.JoinWarning = resp.Warning
		}
	}
	p.render(w, r, "nodes", d)
}

func (p *Panel) nodeRename(w http.ResponseWriter, r *http.Request, s *session) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	name := r.FormValue("name")
	if err := p.srv.SetNodeName(id, name); err != nil {
		p.back(w, r, "/nfr/nodes", err)
		return
	}
	p.ok(w, r, "/nfr/nodes", "节点已改名")
}

func (p *Panel) nodeDelete(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.FormValue("id")
	if err := p.srv.RemoveNode(id); err != nil {
		p.back(w, r, "/nfr/nodes", err)
		return
	}
	p.ok(w, r, "/nfr/nodes", "节点已移除")
}

type serversData struct {
	base
	Cards []serverCard
	Pin   string
}

func (p *Panel) servers(w http.ResponseWriter, r *http.Request, s *session) {
	d := serversData{base: p.newBase(r, s, "servers", "服务器", "servers")}
	p.refreshRemotes(r.Context())
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		if r.FormValue("action") == "delete" {
			if err := p.srv.RemoveRemote(r.FormValue("id")); err != nil {
				d.Error = err.Error()
			} else {
				d.Notice = "服务器已移除"
			}
			d.Cards = p.cards(r.Context())[1:]
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
			d.Notice = "已添加 " + rec.Name
		}
	}
	d.Cards = p.cards(r.Context())[1:]
	d.Pin = p.srv.Pin()
	p.render(w, r, "servers", d)
}

type settingsData struct {
	base
	Name        string
	Domain      string
	PublicHost  string
	APIEnabled  bool
	PortLow     int
	PortHigh    int
	Email       string
	Pin         string
	PanelURL    string
	CertSource  string
	CertMode    string
	CertExpiry  string
	Keys        []api.APIKey
	NewKey      string
	LocalNode   bool
	JoinWarning string
}

func (p *Panel) settings(w http.ResponseWriter, r *http.Request, s *session) {
	d := settingsData{base: p.newBase(r, s, "settings", "设置", "settings")}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		action := r.FormValue("action")
		switch action {
		case "key":
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
				break
			}
			d.NewKey = resp.Key
			d.Notice = "密钥已创建"
		case "revoke":
			if err := p.srv.DeleteAPIKey(r.FormValue("id")); err != nil {
				d.Error = err.Error()
				break
			}
			d.Notice = "密钥已吊销"
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
			d.Notice = "密码已更新"
		default:
			low, _ := strconv.Atoi(r.FormValue("port_low"))
			high, _ := strconv.Atoi(r.FormValue("port_high"))
			err := p.srv.UpdateSettings(
				r.FormValue("domain"),
				r.FormValue("public_host"),
				formSwitch(r.FormValue("api_enabled")),
				[2]int{low, high},
				r.FormValue("email"),
			)
			if err != nil {
				d.Error = err.Error()
				break
			}
			if err := p.srv.SetLocalNode(formSwitch(r.FormValue("local_node"))); err != nil {
				d.Error = err.Error()
				break
			}
			if name := strings.TrimSpace(r.FormValue("name")); name != "" {
				if err := p.srv.SetInstanceName(name); err != nil {
					d.Error = err.Error()
					break
				}
			}
			d.Notice = "设置已保存"
		}
	}
	set := p.srv.Settings()
	info := p.srv.Info()
	d.Name = info.Name
	d.Domain = set.Domain
	d.PublicHost = set.PublicHost
	d.APIEnabled = set.APIEnabled
	d.PortLow, d.PortHigh = set.PortRange[0], set.PortRange[1]
	d.Email = set.HTTPSEmail
	d.LocalNode = set.LocalNode
	d.Pin = info.Pinned
	d.PanelURL = info.PanelURL
	cert := p.srv.CertSummary()
	d.Keys = p.srv.APIKeys()
	d.CertMode = cert.Mode
	d.CertSource = certSourceLabel(cert.Source)
	if cert.NotAfter != nil {
		d.CertExpiry = cert.NotAfter.Local().Format("2006-01-02")
	}
	p.render(w, r, "settings", d)
}

// certSourceLabel turns the internal certificate source key into something a
// person can read.
func certSourceLabel(source string) string {
	switch source {
	case "self-signed":
		return "自签，本机自动生成"
	case "acme":
		return "ACME（Let's Encrypt 或指定 CA）"
	case "file":
		return "自有证书文件"
	case "":
		return "未知"
	}
	return source
}

// allocate answers the "get a port" button without leaving the page. The
// tunnel type from the form picks the port space: udp tunnels reserve UDP.
func (p *Panel) allocate(w http.ResponseWriter, r *http.Request, s *session) {
	network := "tcp"
	if r.FormValue("type") == "udp" {
		network = "udp"
	}
	sel := r.URL.Query().Get("server")
	if sel == "" || sel == "local" {
		resp, err := p.srv.AllocatePort(r.FormValue("tunnel_id"), network)
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
	resp, err := c.Allocate(r.Context(), r.FormValue("tunnel_id"), network)
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

// formSwitch reads a checkbox-like value. A browser sends "1" (or "on") when
// ticked and nothing when not, but scripts post "true"/"false" just as often;
// treating "false" as truthy because it is a non-empty string would silently
// enable a tunnel nobody asked to enable.
func formSwitch(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
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

// splitLocal breaks "host:port" apart for the two-field form. It splits on
// the last colon so bracketed IPv6 like [::1]:8080 keeps working; a value
// without a port comes back with an empty port part.
func splitLocal(local string) (host, port string) {
	local = strings.TrimSpace(local)
	i := strings.LastIndex(local, ":")
	if i < 0 {
		return local, ""
	}
	return strings.TrimSpace(local[:i]), strings.TrimSpace(local[i+1:])
}
