package runtime

import (
	"fmt"
	"strings"

	"mtha/internal/config"
)

// Every generated script's first line is a stable "# mtha:..." marker so
// deploy.go can tell a foreign (hand-written) on-master/on-backup value
// apart from one it manages, and refuse to overwrite it.

func onMasterMarker(name string) string   { return "# mtha:on-master:" + name }
func onBackupMarker(name string) string   { return "# mtha:on-backup:" + name }
func netwatchMarker(target string) string { return "# mtha:netwatch:" + target }

// onMasterScript/onBackupScript log the transition and, when toggles are
// configured for this instance, switch them (project.md §5.5: "optionally
// enable/disable DHCP server, adjust routes") — the same find-and-set shape
// the netwatch scripts use for priority, so a name that matches nothing is a
// no-op rather than a script error. On master the routes come up before
// DHCP starts offering leases; on backup DHCP stops first. Pass a zero
// TogglesConfig for a log-only script.
func onMasterScript(name string, toggles config.TogglesConfig) string {
	lines := []string{
		onMasterMarker(name),
		fmt.Sprintf(":log info \"mtha: %s transitioned to master\"", name),
	}
	lines = append(lines, toggleLines("/ip/route", "comment", toggles.Routes, false)...)
	lines = append(lines, toggleLines("/ip/dhcp-server", "name", toggles.DHCPServers, false)...)
	return strings.Join(lines, "\n")
}

func onBackupScript(name string, toggles config.TogglesConfig) string {
	lines := []string{
		onBackupMarker(name),
		fmt.Sprintf(":log info \"mtha: %s transitioned to backup\"", name),
	}
	lines = append(lines, toggleLines("/ip/dhcp-server", "name", toggles.DHCPServers, true)...)
	lines = append(lines, toggleLines("/ip/route", "comment", toggles.Routes, true)...)
	return strings.Join(lines, "\n")
}

// toggleLines renders one enable/disable line per value, each matching
// section entries where field equals that value.
func toggleLines(section, field string, values []string, disabled bool) []string {
	state := "no"
	if disabled {
		state = "yes"
	}
	lines := make([]string, 0, len(values))
	for _, v := range values {
		lines = append(lines, fmt.Sprintf(":foreach i in=[%s find where %s=%s] do={%s set $i disabled=%s}",
			section, field, quote(v), section, state))
	}
	return lines
}

// quote renders v as a RouterOS script string literal, escaping the
// characters that would otherwise end the string or expand a variable.
func quote(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)
	return `"` + r.Replace(v) + `"`
}

// Netwatch scripts only touch VRRP interfaces mtha manages (comment
// "mtha:vrrp:<name>", see vrrpTag), never hand-made ones. project.md §5.5:
// "raise/lower VRRP priority (priority_master <-> priority_degraded)", per
// router — the standby is restored to priority_backup, not promoted.
//
// netwatchDownAny filters on status alone. A disabled entry is never
// probed and reports "unknown" (disabling one that is down resets it), so
// status=down already means enabled-and-down. It must not also say
// disabled=no: on RouterOS 7.23.7 an enabled netwatch entry has no disabled
// property, so that matches nothing and the count is always 0 (issue #26).
const (
	managedVRRP     = `/interface/vrrp find where comment~"^mtha:vrrp:"`
	netwatchDownAny = `/tool/netwatch print count-only where comment~"^mtha:netwatch:" status=down`
)

// netwatchDownScript lowers the router's managed VRRP interfaces to the
// degraded priority as soon as any one target goes down.
func netwatchDownScript(target string, degraded int) string {
	return fmt.Sprintf("%s\n:foreach i in=[%s] do={/interface/vrrp set $i priority=%d}",
		netwatchMarker(target), managedVRRP, degraded)
}

// netwatchUpScript restores the router's base priority, but only when no
// mtha netwatch entry is still down: each target's up-script runs
// independently, and must not undo another target's down-script.
func netwatchUpScript(target string, base int) string {
	return fmt.Sprintf("%s\n:if ([%s] = 0) do={:foreach i in=[%s] do={/interface/vrrp set $i priority=%d}}",
		netwatchMarker(target), netwatchDownAny, managedVRRP, base)
}
