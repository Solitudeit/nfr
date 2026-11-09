// Package federation talks to another NFR instance over its HTTP API. It is
// deliberately a plain client: the console decides when to call it.
package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/transport"
)

// Client manages one remote instance.
type Client struct {
	BaseURL string
	Key     string
	Pin     string
	HTTP    *http.Client
}

// New builds a client pinned to the remote certificate fingerprint.
func New(baseURL, key, pin string) *Client {
	return newClient(baseURL, key, pin, false)
}

// NewTrusted manages an instance whose certificate is signed by a public CA.
func NewTrusted(baseURL, key string) *Client {
	return newClient(baseURL, key, "", true)
}

func newClient(baseURL, key, pin string, ca bool) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	host := hostOf(baseURL)
	tlsCfg := transport.WebClientConfig(host, pin)
	if ca {
		tlsCfg = transport.PublicClientConfig(host)
	}
	return &Client{
		BaseURL: baseURL,
		Key:     key,
		Pin:     pin,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConnsPerHost: 2,
			},
		},
	}
}

func hostOf(baseURL string) string {
	raw := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	if h, _, err := net.SplitHostPort(raw); err == nil {
		return h
	}
	return strings.TrimSuffix(raw, "/")
}

// Do performs an authenticated request and decodes the result into out.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e api.Error
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("remote: %s", e.Error)
		}
		return fmt.Errorf("remote: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Info fetches the remote identity.
func (c *Client) Info(ctx context.Context) (api.Info, error) {
	var info api.Info
	err := c.Do(ctx, http.MethodGet, "/api/v1/info", nil, &info)
	return info, err
}

// Tunnels lists remote tunnels.
func (c *Client) Tunnels(ctx context.Context) ([]api.Tunnel, error) {
	var out []api.Tunnel
	err := c.Do(ctx, http.MethodGet, "/api/v1/tunnels", nil, &out)
	return out, err
}

// CreateTunnel creates a tunnel on the remote instance.
func (c *Client) CreateTunnel(ctx context.Context, req api.TunnelRequest) (api.Tunnel, error) {
	var out api.Tunnel
	err := c.Do(ctx, http.MethodPost, "/api/v1/tunnels", req, &out)
	return out, err
}

// UpdateTunnel edits a remote tunnel.
func (c *Client) UpdateTunnel(ctx context.Context, id string, req api.TunnelRequest) (api.Tunnel, error) {
	var out api.Tunnel
	err := c.Do(ctx, http.MethodPatch, "/api/v1/tunnels/"+id, req, &out)
	return out, err
}

// DeleteTunnel removes a remote tunnel.
func (c *Client) DeleteTunnel(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/tunnels/"+id, nil, nil)
}

// Nodes lists remote agents.
func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var out []api.Node
	err := c.Do(ctx, http.MethodGet, "/api/v1/nodes", nil, &out)
	return out, err
}

// JoinCode mints a join code on the remote instance.
func (c *Client) JoinCode(ctx context.Context, req api.JoinCodeRequest) (api.JoinCodeResponse, error) {
	var out api.JoinCodeResponse
	err := c.Do(ctx, http.MethodPost, "/api/v1/nodes/join-codes", req, &out)
	return out, err
}

// Allocate asks the remote for a free port in the given port space
// ("tcp" default, "udp" for UDP tunnels).
func (c *Client) Allocate(ctx context.Context, tunnelID, network string) (api.AllocateResponse, error) {
	var out api.AllocateResponse
	err := c.Do(ctx, http.MethodPost, "/api/v1/ports/allocate", api.AllocateRequest{TunnelID: tunnelID, Network: network}, &out)
	return out, err
}

// Probe confirms the remote is reachable and the key works.
func (c *Client) Probe(ctx context.Context) (api.Info, error) {
	return c.Info(ctx)
}

// PinFor discovers a remote certificate fingerprint without trusting it.
// VerifyMode asks an instance how peers should verify its certificate. It
// returns "pin" plus the fingerprint for self-signed or operator supplied
// certificates, or "ca" with an empty pin when the instance uses ACME.
func VerifyMode(ctx context.Context, baseURL string) (string, string, error) {
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	tr := &http.Transport{TLSClientConfig: transport.WebClientConfig(hostOf(baseURL), "")}
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/.well-known/nfr", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", "", err
	}
	var wk struct {
		Pin    string `json:"pin"`
		Verify string `json:"verify"`
	}
	if err := json.Unmarshal(raw, &wk); err != nil {
		return "", "", fmt.Errorf("unexpected discovery response: %w", err)
	}
	if resp.TLS != nil && wk.Verify != "ca" {
		return "pin", transport.PinOf(*resp.TLS), nil
	}
	if wk.Verify == "ca" {
		return "ca", "", nil
	}
	return "pin", wk.Pin, nil
}

func PinFor(ctx context.Context, baseURL string) (string, error) {
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/info", nil)
	if err != nil {
		return "", err
	}
	tr := &http.Transport{TLSClientConfig: transport.WebClientConfig("", "")}
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.TLS == nil {
		return "", fmt.Errorf("not a TLS endpoint")
	}
	return transport.PinOf(*resp.TLS), nil
}
