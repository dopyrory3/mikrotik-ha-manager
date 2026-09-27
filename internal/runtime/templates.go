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
// netwatchScript uses for priority, so a name that matches nothing is a
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

// netwatchScript raises or lowers every VRRP instance's priority on this
// router in one shot (project.md §5.5: "raise/lower VRRP priority
// (priority_master <-> priority_degraded)"); the same script (parameterized
// only by which priority to set) serves both up-script and down-script.
func netwatchScript(target string, priority int) string {
	return fmt.Sprintf("%s\n:foreach i in=[/interface/vrrp find] do={/interface/vrrp set $i priority=%d}",
		netwatchMarker(target), priority)
}
