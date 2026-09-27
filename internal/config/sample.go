package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// SampleYAML is a commented starter pair file matching the shape documented
// in project.md §5.1. Credentials are deliberately absent: they are resolved
// from MTHA_<PAIR>_<ROUTER>_PASSWORD, never from this file.
const SampleYAML = `# mtha pair file. Default location: ~/.config/mtha/pairs.yaml
#
# Credentials are never stored here. Set one environment variable per router,
# named MTHA_<PAIR>_<ROUTER>_PASSWORD with both parts upper-cased, e.g. for
# pair "core" below:
#
#   export MTHA_CORE_A_PASSWORD=...
#   export MTHA_CORE_B_PASSWORD=...

pairs:
  - name: core
    routers:
      a: { host: 10.0.0.2, user: mtha, insecure_tls: false }
      # port is optional; set it if a router's REST API (www-ssl service)
      # has been moved off the standard HTTPS port 443. This is unrelated
      # to the legacy binary API service (ports 8728/8729), which mtha
      # does not use.
      b: { host: 10.0.0.3, port: 8443, user: mtha }
    vrrp:
      - interface: vrrp-lan
        on: ether2
        vrid: 1
        addresses: [10.0.0.1/24]
      - interface: vrrp-wan
        on: ether1
        vrid: 2
        addresses: [203.0.113.1/29]
    sync:
      sections:
        - ip/firewall/filter
        - ip/firewall/nat
        - ip/firewall/address-list
        - ip/dhcp-server
        - ip/dhcp-server/network
        - ip/dhcp-server/lease   # static only
        - ip/dns/static
        - ip/route               # excluding per-router routes
        - ip/service
        - user
        - system/script
        - system/scheduler
      exempt:
        - system/identity
        - interface/vrrp.priority
        - ip/address             # per-router interface addresses
        - ip/service.certificate # each router's own self-signed cert
        - user.last-logged-in    # updates independently on every login
        # runtime.toggles below enables these on the master and disables
        # them on the standby, so "disabled" differs by design:
        - ip/dhcp-server.disabled
        - ip/route.disabled
    runtime:
      netwatch_targets: [1.1.1.1, 8.8.8.8]
      priority_master: 200
      priority_backup: 100
      priority_degraded: 50
      # Optional: what the VRRP on-master/on-backup scripts switch on
      # cutover. Omit it to keep those scripts log-only. The standby's DHCP
      # server must be disabled at rest; DHCP leases are not synced, so
      # clients re-DISCOVER against the new master after a failover.
      toggles:
        vrrp: vrrp-lan             # the instance whose transitions drive this
        dhcp_servers: [dhcp-lan]   # /ip/dhcp-server names
        routes: [mtha-default]     # /ip/route comments
`

// WriteSample writes the sample pair file to path, creating parent
// directories as needed. It refuses to overwrite an existing file.
func WriteSample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(SampleYAML), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
