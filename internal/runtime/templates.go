package runtime

import "fmt"

// Every generated script's first line is a stable "# mtha:..." marker so
// deploy.go can tell a foreign (hand-written) on-master/on-backup value
// apart from one it manages, and refuse to overwrite it.

func onMasterMarker(name string) string   { return "# mtha:on-master:" + name }
func onBackupMarker(name string) string   { return "# mtha:on-backup:" + name }
func netwatchMarker(target string) string { return "# mtha:netwatch:" + target }

// onMasterScript/onBackupScript are v1's on-master/on-backup content:
// logging only. project.md §5.5 mentions optionally toggling DHCP or
// adjusting routes; that's deliberately deferred — there's no config field
// for it yet.
func onMasterScript(name string) string {
	return fmt.Sprintf("%s\n:log info \"mtha: %s transitioned to master\"", onMasterMarker(name), name)
}

func onBackupScript(name string) string {
	return fmt.Sprintf("%s\n:log info \"mtha: %s transitioned to backup\"", onBackupMarker(name), name)
}

// netwatchScript raises or lowers every VRRP instance's priority on this
// router in one shot (project.md §5.5: "raise/lower VRRP priority
// (priority_master <-> priority_degraded)"); the same script (parameterized
// only by which priority to set) serves both up-script and down-script.
func netwatchScript(target string, priority int) string {
	return fmt.Sprintf("%s\n:foreach i in=[/interface/vrrp find] do={/interface/vrrp set $i priority=%d}",
		netwatchMarker(target), priority)
}
