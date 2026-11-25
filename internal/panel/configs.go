package panel

import (
	"bytes"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/config"
	"gopkg.in/yaml.v3"
)

type nodeConfigBundle struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	TOML   string `json:"toml"`
	YAML   string `json:"yaml"`
}

type configsPayload struct {
	ServerAddr  string             `json:"server_addr"`
	AuthToken   string             `json:"auth_token"`
	Pin         string             `json:"pin"`
	CA          bool               `json:"ca"`
	Domain      string             `json:"domain"`
	PublicHost  string             `json:"public_host"`
	PanelHTTPS  bool               `json:"panel_https"`
	ServerTOML  string             `json:"server_toml"`
	ServerYAML  string             `json:"server_yaml"`
	Template    nodeConfigBundle   `json:"template"`
	NodeConfigs []nodeConfigBundle `json:"nodes"`
}

func (p *Panel) buildConfigsPayload() configsPayload {
	eff := p.srv.EffectiveConfig()
	set := p.srv.Settings()
	cert := p.srv.CertSummary()
	useCA := cert.Source == "acme"
	serverAddr := p.srv.JoinAddr()
	token := p.srv.AuthToken()
	pin := ""
	if !useCA {
		pin = p.srv.Pin()
	}

	serverTOML, serverYAML := encodeServerConfigs(eff)

	tmplAgent := config.Agent{
		ServerAddr: serverAddr,
		AuthToken:  token,
		Name:       "my-device",
		Pin:        pin,
		CA:         useCA,
		Proxies: []config.ProxyConfig{
			{
				Name:       "ssh",
				Type:       "tcp",
				Local:      "127.0.0.1:22",
				RemotePort: set.PortRange[0],
			},
		},
	}
	tmplTOML, tmplYAML := encodeAgentConfigs(tmplAgent)

	nodes := p.srv.Nodes()
	tunnels := p.srv.Tunnels()
	byNode := make(map[string][]api.Tunnel, len(nodes))
	for _, t := range tunnels {
		if t.NodeID != "" {
			byNode[t.NodeID] = append(byNode[t.NodeID], t)
		}
	}

	bundles := make([]nodeConfigBundle, 0, len(nodes))
	for _, n := range nodes {
		proxies := make([]config.ProxyConfig, 0, len(byNode[n.ID]))
		for _, t := range byNode[n.ID] {
			proxies = append(proxies, config.ProxyConfig{
				Name:        t.Name,
				Type:        t.Type,
				Local:       t.Local,
				RemotePort:  t.RemotePort,
				Hosts:       t.Hosts,
				BandwidthKB: t.BandwidthKB,
				QuotaMB:     t.QuotaMB,
			})
		}
		ag := config.Agent{
			ServerAddr: serverAddr,
			AuthToken:  token,
			Name:       n.Name,
			Pin:        pin,
			CA:         useCA,
			Proxies:    proxies,
		}
		ct, cy := encodeAgentConfigs(ag)
		bundles = append(bundles, nodeConfigBundle{
			NodeID: n.ID,
			Name:   n.Name,
			Online: n.Online,
			TOML:   ct,
			YAML:   cy,
		})
	}

	return configsPayload{
		ServerAddr:  serverAddr,
		AuthToken:   token,
		Pin:         pin,
		CA:          useCA,
		Domain:      set.Domain,
		PublicHost:  set.PublicHost,
		PanelHTTPS:  eff.Panel.HTTPS,
		ServerTOML:  serverTOML,
		ServerYAML:  serverYAML,
		Template:    nodeConfigBundle{Name: "my-device", TOML: tmplTOML, YAML: tmplYAML},
		NodeConfigs: bundles,
	}
}

type tunnelConfigPayload struct {
	TunnelID     string `json:"tunnel_id"`
	Name         string `json:"name"`
	FilenameTOML string `json:"filename_toml"`
	FilenameYAML string `json:"filename_yaml"`
	TOML         string `json:"toml"`
	YAML         string `json:"yaml"`
	RunHint      string `json:"run_hint"`
}

func (p *Panel) buildTunnelConfigPayload(t api.Tunnel) tunnelConfigPayload {
	cert := p.srv.CertSummary()
	useCA := cert.Source == "acme"
	serverAddr := p.srv.JoinAddr()
	token := p.srv.AuthToken()
	pin := ""
	if !useCA {
		pin = p.srv.Pin()
	}

	ag := config.Agent{
		ServerAddr: serverAddr,
		AuthToken:  token,
		Name:       "t-" + t.ID,
		Pin:        pin,
		CA:         useCA,
		Proxies: []config.ProxyConfig{
			{
				Name:        t.Name,
				Type:        t.Type,
				Local:       t.Local,
				RemotePort:  t.RemotePort,
				Hosts:       t.Hosts,
				BandwidthKB: t.BandwidthKB,
				QuotaMB:     t.QuotaMB,
			},
		},
	}
	tomlStr, yamlStr := encodeAgentConfigs(ag)
	safeName := sanitizeFilenamePart(t.Name)
	if safeName == "" {
		safeName = t.ID
	}
	tomlFile := "nfrc-" + safeName + ".toml"
	yamlFile := "nfrc-" + safeName + ".yaml"
	return tunnelConfigPayload{
		TunnelID:     t.ID,
		Name:         t.Name,
		FilenameTOML: tomlFile,
		FilenameYAML: yamlFile,
		TOML:         tomlStr,
		YAML:         yamlStr,
		RunHint:      "nfrc -c " + tomlFile,
	}
}

func sanitizeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func encodeServerConfigs(cfg config.Server) (tomlStr, yamlStr string) {
	var tb bytes.Buffer
	if err := toml.NewEncoder(&tb).Encode(cfg); err == nil {
		tomlStr = strings.TrimSpace(tb.String()) + "\n"
	}
	var yb bytes.Buffer
	ye := yaml.NewEncoder(&yb)
	ye.SetIndent(2)
	if err := ye.Encode(cfg); err == nil {
		_ = ye.Close()
		yamlStr = strings.TrimSpace(yb.String()) + "\n"
	}
	return tomlStr, yamlStr
}

func encodeAgentConfigs(cfg config.Agent) (tomlStr, yamlStr string) {
	var tb bytes.Buffer
	if err := toml.NewEncoder(&tb).Encode(cfg); err == nil {
		tomlStr = strings.TrimSpace(tb.String()) + "\n"
	}
	var yb bytes.Buffer
	ye := yaml.NewEncoder(&yb)
	ye.SetIndent(2)
	if err := ye.Encode(cfg); err == nil {
		_ = ye.Close()
		yamlStr = strings.TrimSpace(yb.String()) + "\n"
	}
	return tomlStr, yamlStr
}
