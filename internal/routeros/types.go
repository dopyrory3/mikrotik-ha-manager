package routeros

import (
	"context"

	"mtha/internal/model"
)

// SystemResource is the response of GET /rest/system/resource.
type SystemResource struct {
	Version     string `json:"version"`
	BoardName   string `json:"board-name"`
	Uptime      string `json:"uptime"`
	CPULoad     string `json:"cpu-load"`
	FreeMemory  string `json:"free-memory"`
	TotalMemory string `json:"total-memory"`
}

// Identity is the response of GET /rest/system/identity.
type Identity struct {
	Name string `json:"name"`
}

// VRRPInstance is one entry of GET /rest/interface/vrrp.
type VRRPInstance struct {
	ID        string `json:".id"`
	Name      string `json:"name"`
	Interface string `json:"interface"`
	Priority  string `json:"priority"`
	State     string `json:"vrrp-state"`
	Disabled  string `json:"disabled"`
}

// NetwatchEntry is one entry of GET /rest/tool/netwatch.
type NetwatchEntry struct {
	ID       string `json:".id"`
	Host     string `json:"host"`
	Status   string `json:"status"`
	Comment  string `json:"comment"`
	Disabled string `json:"disabled"`
}

// SystemResource fetches the router's system resource summary.
func (c *Client) SystemResource(ctx context.Context) (*SystemResource, error) {
	var r SystemResource
	if err := c.Get(ctx, "/system/resource", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Identity fetches the router's system identity (hostname).
func (c *Client) Identity(ctx context.Context) (*Identity, error) {
	var id Identity
	if err := c.Get(ctx, "/system/identity", &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// VRRP fetches all configured VRRP instances.
func (c *Client) VRRP(ctx context.Context) ([]VRRPInstance, error) {
	var vs []VRRPInstance
	if err := c.Get(ctx, "/interface/vrrp", &vs); err != nil {
		return nil, err
	}
	return vs, nil
}

// Netwatch fetches all configured netwatch entries.
func (c *Client) Netwatch(ctx context.Context) ([]NetwatchEntry, error) {
	var ns []NetwatchEntry
	if err := c.Get(ctx, "/tool/netwatch", &ns); err != nil {
		return nil, err
	}
	return ns, nil
}

// GetSection fetches a config section by its REST path (e.g.
// "ip/firewall/filter"), as used for drift detection (project.md §5.3).
func (c *Client) GetSection(ctx context.Context, section string) ([]model.Entry, error) {
	var raw []map[string]any
	if err := c.Get(ctx, "/"+section, &raw); err != nil {
		return nil, err
	}
	entries := make([]model.Entry, len(raw))
	for i, r := range raw {
		entries[i] = model.Entry(r)
	}
	return entries, nil
}
