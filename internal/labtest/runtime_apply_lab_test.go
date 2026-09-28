//go:build lab

package labtest_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/runtime"
	"mtha/internal/ui"
)

// A Runtime deploy from the TUI goes through the Apply pipeline like a
// sync: a dry run that includes each router's pre-apply backup, the y
// confirmation plus Y for a router holding VRRP master, nothing written
// until then, the backup really taken on each router, and the action
// recorded in the session's Events timeline.
func TestLabRuntimeDeployGoesThroughApply(t *testing.T) {
	lab := labtest.New(t)
	pair := runtimePair(lab, []string{rtTargetA}, config.TogglesConfig{})
	plans := buildRuntime(t, pair)

	d := labtest.Drive(t, ui.New(pair, true, lab.Pollers(time.Second)))
	screen := func(m ui.Model) string { return labtest.StripANSI(m.View()) }
	showing := func(s ...string) func(ui.Model) bool {
		return func(m ui.Model) bool {
			v := screen(m)
			for _, x := range s {
				if !strings.Contains(v, x) {
					return false
				}
			}
			return true
		}
	}

	// Let both routers be polled first, so the master check below is
	// decided by a's real role rather than by "not polled yet".
	d.Until("both routers polled", 30*time.Second, showing("master", "backup"))

	d.Send(labtest.Key("3"))
	d.Until("runtime status read", 30*time.Second, showing("Router A", "Router B", "missing  vrrp interface "+rtVRRP))

	d.Send(labtest.Key("d"))
	m := d.Until("deploy dry run", 30*time.Second, showing("runtime deploy", "Dry run:", "press y to apply"))
	dry := screen(m)
	for _, want := range []string{"pre-apply backup of router a", "pre-apply backup of router b", "create vrrp interface " + rtVRRP} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry run does not show %q:\n%s", want, dry)
		}
	}
	backup := regexp.MustCompile(`mtha-pre-apply-\d{8}-\d{6}`).FindString(dry)
	if backup == "" {
		t.Fatalf("dry run names no pre-apply backup:\n%s", dry)
	}

	// Router a is VRRP master: y alone is not enough.
	d.Send(labtest.Key("y"))
	d.Until("second confirmation for the master", 10*time.Second, showing("press Y (shift+y)"))
	requireRuntimeStates(t, lab, plans, runtime.StateMissing) // nothing written yet

	d.Send(labtest.Key("Y"))
	m = d.Until("deploy run and verified", 2*time.Minute, func(m ui.Model) bool {
		v := screen(m)
		return strings.Contains(v, "verified — see the Runtime screen") || strings.Contains(v, "not as intended") ||
			strings.Contains(v, "apply stopped") || strings.Contains(v, "verify error")
	})
	if v := screen(m); !strings.Contains(v, "verified — see the Runtime screen") {
		t.Fatalf("deploy did not finish verified:\n%s", v)
	}
	requireRuntimeStates(t, lab, plans, runtime.StateOK)

	for _, r := range labRouters(lab) {
		var files []map[string]string
		if err := r.c.Get(context.Background(), "/file?name="+backup+".backup", &files); err != nil || len(files) != 1 {
			t.Errorf("router %s: pre-apply backup %s.backup: %v, %v", r.key, backup, files, err)
		}
	}

	d.Send(labtest.Key("6"))
	m = d.Until("events timeline read", 30*time.Second, showing("deploy runtime logic"))
	if n := strings.Count(screen(m), "deploy runtime logic"); n != 2 {
		t.Errorf("events timeline has %d runtime deploy actions, want one per router:\n%s", n, screen(m))
	}
}
