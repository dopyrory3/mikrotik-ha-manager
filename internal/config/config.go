// Package config loads mtha pair definitions from the YAML pair file and
// resolves per-router credentials from the environment or OS keychain.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RouterConfig describes one router in a pair.
type RouterConfig struct {
	Host string `yaml:"host"`
	// Port overrides the REST API (www-ssl) port; omit it to use the
	// standard HTTPS port (443). This is for a relocated www-ssl service,
	// not the legacy binary API (ports 8728/8729), which mtha does not
	// speak — see project.md §6 (REST-only, no third-party client).
	Port        int    `yaml:"port,omitempty"`
	User        string `yaml:"user"`
	InsecureTLS bool   `yaml:"insecure_tls"`
}

// VRRPInstance is a VRRP interface the pair should track, and — once "on",
// "vrid" and "addresses" are set — that mtha's runtime deployment (milestone
// 4, project.md §5.5) provisions on both routers.
type VRRPInstance struct {
	Interface string `yaml:"interface"` // name mtha gives the created VRRP interface
	// On is the physical interface the VRRP interface rides on (same name
	// on both routers). Required only for runtime deploy/verify, not for
	// tracking an already-existing instance on the dashboard/drift screens.
	On   string `yaml:"on,omitempty"`
	VRID int    `yaml:"vrid,omitempty"`
	// Addresses are the VIP(s), in CIDR notation, assigned to the VRRP
	// interface on both routers.
	Addresses []string `yaml:"addresses,omitempty"`
}

// SyncConfig lists which config sections are kept in sync and which paths
// within them are exempt from comparison.
type SyncConfig struct {
	Sections []string `yaml:"sections"`
	Exempt   []string `yaml:"exempt"`
}

// RuntimeConfig parameterizes the netwatch/VRRP templates deployed to both
// routers.
type RuntimeConfig struct {
	NetwatchTargets  []string `yaml:"netwatch_targets"`
	PriorityMaster   int      `yaml:"priority_master"`
	PriorityBackup   int      `yaml:"priority_backup"`
	PriorityDegraded int      `yaml:"priority_degraded"`
	// Toggles is what the on-master/on-backup scripts switch on a VRRP
	// transition (project.md §5.5: "optionally enable/disable DHCP server,
	// adjust routes"). Zero value keeps the scripts log-only.
	Toggles TogglesConfig `yaml:"toggles,omitempty"`
}

// TogglesConfig names the router objects a VRRP transition switches: on
// master they're enabled, on backup disabled, so only the router holding
// the VIP serves DHCP and carries the listed routes.
type TogglesConfig struct {
	// VRRP is the one instance (by interface name) whose transitions drive
	// the toggles. Required when any toggle is set: with several instances,
	// splitting mastership across routers must not leave both serving DHCP.
	VRRP string `yaml:"vrrp,omitempty"`
	// DHCPServers are /ip/dhcp-server entries, matched by name.
	DHCPServers []string `yaml:"dhcp_servers,omitempty"`
	// Routes are /ip/route entries, matched by comment — typically the
	// default route out of the uplink only the master should use.
	Routes []string `yaml:"routes,omitempty"`
}

// Enabled reports whether any toggle is configured.
func (t TogglesConfig) Enabled() bool {
	return len(t.DHCPServers) > 0 || len(t.Routes) > 0
}

// Pair is one managed HA pair: two routers, the VRRP instances linking them,
// and the sync/runtime settings that apply to both.
type Pair struct {
	Name    string                  `yaml:"name"`
	Routers map[string]RouterConfig `yaml:"routers"`
	VRRP    []VRRPInstance          `yaml:"vrrp"`
	Sync    SyncConfig              `yaml:"sync"`
	Runtime RuntimeConfig           `yaml:"runtime"`
}

// File is the top-level shape of the pair file.
type File struct {
	Pairs []Pair `yaml:"pairs"`
	// Warnings are problems Load found that don't stop the file being
	// used, such as SectionOrderWarnings.
	Warnings []string `yaml:"-"`
}

// DefaultPath returns the default pair file location, ~/.config/mtha/pairs.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mtha", "pairs.yaml"), nil
}

// Load reads and parses the pair file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pair file %s: %w", path, err)
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse pair file %s: %w", path, err)
	}

	for _, p := range f.Pairs {
		f.Warnings = append(f.Warnings, p.SectionOrderWarnings()...)
		if _, ok := p.Routers["a"]; !ok {
			return nil, fmt.Errorf("pair %q: missing router \"a\"", p.Name)
		}
		if _, ok := p.Routers["b"]; !ok {
			return nil, fmt.Errorf("pair %q: missing router \"b\"", p.Name)
		}
		for key, router := range p.Routers {
			if router.Port < 0 || router.Port > 65535 {
				return nil, fmt.Errorf("pair %q router %q: invalid port %d", p.Name, key, router.Port)
			}
		}
	}

	return &f, nil
}

// Pair returns the named pair, or an error if it isn't defined.
func (f *File) Pair(name string) (*Pair, error) {
	for i := range f.Pairs {
		if f.Pairs[i].Name == name {
			return &f.Pairs[i], nil
		}
	}
	return nil, fmt.Errorf("pair %q not found", name)
}

// sectionReferents maps a sync section to the sync sections its entries name
// objects in (docs/design-questions.md §3): a lease names its DHCP server, a
// scheduler its script, a firewall rule its address lists. It is the synced
// half of internal/plan's reference table, which a plan test keeps it in
// step with; referents mtha never syncs (ip/pool, user/group, interfaces)
// have no order to check.
var sectionReferents = map[string][]string{
	"ip/firewall/filter":   {"ip/firewall/address-list"},
	"ip/firewall/nat":      {"ip/firewall/address-list"},
	"ip/firewall/mangle":   {"ip/firewall/address-list"},
	"ip/firewall/raw":      {"ip/firewall/address-list"},
	"ip/dhcp-server/lease": {"ip/dhcp-server"},
	"system/scheduler":     {"system/script"},
}

// SectionReferents returns the sync sections whose objects entries of
// section can name.
func SectionReferents(section string) []string {
	return sectionReferents[section]
}

// SectionOrderWarnings reports each synced referent listed after a section
// that refers to it (docs/design-questions.md §3, option D). An apply runs
// sections in the configured order (project.md §5.4), so a referrer listed
// first is created before the object it names: a lease before its DHCP
// server, a scheduler before its script, a firewall rule before the address
// list it matches, which for a drop rule on a blocklist fails open until
// the list is written. It is a warning, not an error: RouterOS accepts some
// of these, and the apply's dry run checks each reference again.
func (p Pair) SectionOrderWarnings() []string {
	pos := make(map[string]int, len(p.Sync.Sections))
	for i, s := range p.Sync.Sections {
		if _, dup := pos[s]; !dup {
			pos[s] = i
		}
	}
	var out []string
	for i, s := range p.Sync.Sections {
		if pos[s] != i {
			continue
		}
		for _, ref := range sectionReferents[s] {
			if j, ok := pos[ref]; ok && j > i {
				out = append(out, fmt.Sprintf("pair %q: sync section %s is listed after %s, which refers to it; list %s first so its entries exist before anything that names them is created", p.Name, ref, s, ref))
			}
		}
	}
	return out
}
