package routeros

import (
	"context"
	"encoding/json"
	"strconv"

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
//
// The role is not decoded from one agreed field: no response captured from a
// real RouterOS 7 device backs either shape. RouterOS REST reports print
// flags as properties ("master":"true", "backup":"false"), and some
// payloads may carry a "vrrp-state" string instead, so both are decoded and
// Role reconciles them. Anything it can't positively place is RoleUnknown,
// which callers must treat as "possibly master" (project.md §10.1).
type VRRPInstance struct {
	ID        string `json:".id"`
	Name      string `json:"name"`
	Interface string `json:"interface"`
	Priority  string `json:"priority"`
	Master    Flag   `json:"master"`
	Backup    Flag   `json:"backup"`
	// State is the raw "vrrp-state" value, if the router reports one. Use
	// Role rather than reading it directly.
	State    string `json:"vrrp-state"`
	Disabled string `json:"disabled"`
}

// VRRPRole is an instance's VRRP role as far as it can be determined.
type VRRPRole int

const (
	// RoleUnknown means the payload doesn't positively say master or
	// backup: no flags or state, contradictory ones, or another state
	// such as init.
	RoleUnknown VRRPRole = iota
	RoleMaster
	RoleBackup
)

func (r VRRPRole) String() string {
	switch r {
	case RoleMaster:
		return "master"
	case RoleBackup:
		return "backup"
	default:
		return "unknown"
	}
}

// Role reconciles the master/backup flags with vrrp-state. The flags decide
// when exactly one of them is set; vrrp-state decides when it is "master" or
// "backup". If both sources decide and disagree, the role is unknown.
func (v VRRPInstance) Role() VRRPRole {
	fromFlags := RoleUnknown
	switch master, backup := v.Master.True(), v.Backup.True(); {
	case master && !backup:
		fromFlags = RoleMaster
	case backup && !master:
		fromFlags = RoleBackup
	}

	fromState := RoleUnknown
	switch v.State {
	case "master":
		fromState = RoleMaster
	case "backup":
		fromState = RoleBackup
	}

	switch {
	case fromFlags == RoleUnknown:
		return fromState
	case fromState == RoleUnknown || fromState == fromFlags:
		return fromFlags
	default:
		return RoleUnknown
	}
}

// Flag is a RouterOS boolean property. REST encodes booleans as the strings
// "true"/"false"; a JSON boolean is accepted too, so a payload that uses one
// still decodes rather than failing the whole read.
type Flag string

// UnmarshalJSON accepts a JSON string or boolean.
func (f *Flag) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*f = Flag(strconv.FormatBool(b))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*f = Flag(s)
	return nil
}

// True reports whether the flag is set ("true" or "yes").
func (f Flag) True() bool {
	return f == "true" || f == "yes"
}

// NetwatchEntry is one entry of GET /rest/tool/netwatch.
type NetwatchEntry struct {
	ID       string `json:".id"`
	Host     string `json:"host"`
	Status   string `json:"status"`
	Comment  string `json:"comment"`
	Disabled string `json:"disabled"`
}

// getInto GETs path into a zero-valued T, returning the zero value alongside
// the error on failure so callers can pass it straight through (or turn it
// into a nil pointer, for the pointer-returning readers below).
func getInto[T any](ctx context.Context, c *Client, path string) (T, error) {
	var v T
	err := c.Get(ctx, path, &v)
	return v, err
}

// SystemResource fetches the router's system resource summary.
func (c *Client) SystemResource(ctx context.Context) (*SystemResource, error) {
	r, err := getInto[SystemResource](ctx, c, "/system/resource")
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Identity fetches the router's system identity (hostname).
func (c *Client) Identity(ctx context.Context) (*Identity, error) {
	id, err := getInto[Identity](ctx, c, "/system/identity")
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// VRRP fetches all configured VRRP instances.
func (c *Client) VRRP(ctx context.Context) ([]VRRPInstance, error) {
	return getInto[[]VRRPInstance](ctx, c, "/interface/vrrp")
}

// Netwatch fetches all configured netwatch entries.
func (c *Client) Netwatch(ctx context.Context) ([]NetwatchEntry, error) {
	return getInto[[]NetwatchEntry](ctx, c, "/tool/netwatch")
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
