//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/plan"
	"mtha/internal/routeros"
	"mtha/internal/runtime"
)

// Runtime deployment against the real routers (project.md §5.5): what a
// deploy puts on each router, that verify reads it back as ok, that it is
// idempotent, that it never overwrites a hand-written transition script, and
// that remove takes it all away again.
//
// The baseline's own vrrp-lan was made by provision.sh, not mtha, so it
// carries no mtha: tag and a deploy of it would be (rightly) refused as a
// conflict. These tests deploy a second instance beside it instead,
// vrrp-mtha, on the same ether2 with its own VRID and VIP. vrrp-lan is left
// alone throughout, which the netwatch tests also rely on to show the
// scripts only touch what mtha manages.
const (
	rtVRRP = "vrrp-mtha"
	rtVRID = 2
	rtVIP  = "192.168.88.9/24"

	// The routers' ether2 addresses: each answers from both routers.
	rtTargetA = "192.168.88.2"
	rtTargetB = "192.168.88.3"
	// rtUnreachable is on the lab LAN but nothing holds it (it is the
	// fixture's "lab: nobody" netwatch host).
	rtUnreachable = "192.168.88.254"

	rtBackupName = "mtha-lab-runtime"
)

// The four sections a deploy writes to, and the tag every object it makes
// there carries.
var rtSections = []string{"interface/vrrp", "ip/address", "tool/netwatch", "system/scheduler"}

// runtimePair is the lab pair with vrrp-mtha as its only VRRP instance, the
// given netwatch targets, and toggles (zero for none) on vrrp-mtha.
func runtimePair(lab *labtest.Lab, targets []string, toggles config.TogglesConfig) *config.Pair {
	p := *lab.Pair
	p.VRRP = []config.VRRPInstance{{Interface: rtVRRP, On: "ether2", VRID: rtVRID, Addresses: []string{rtVIP}}}
	p.Runtime.NetwatchTargets = targets
	if toggles.Enabled() {
		toggles.VRRP = rtVRRP
	}
	p.Runtime.Toggles = toggles
	return &p
}

func buildRuntime(t *testing.T, pair *config.Pair) map[string]runtime.Plan {
	t.Helper()
	plans, err := runtime.BuildPlan(pair)
	if err != nil {
		t.Fatalf("build runtime plan: %v", err)
	}
	return plans
}

// runtimeWrites plans action from fresh reads, as the Runtime screen does.
func runtimeWrites(t *testing.T, lab *labtest.Lab, plans map[string]runtime.Plan, action runtime.Action) plan.Plan {
	t.Helper()
	p, err := runtime.Writes(context.Background(), lab.A, lab.B, plans, action, rtBackupName)
	if err != nil {
		t.Fatalf("plan runtime %s: %v", action, err)
	}
	return p
}

// deployRuntime plans and runs a deploy and requires verify to read every
// object back as ok.
func deployRuntime(t *testing.T, lab *labtest.Lab, plans map[string]runtime.Plan) plan.Plan {
	t.Helper()
	p := runtimeWrites(t, lab, plans, runtime.ActionDeploy)
	execute(t, lab, p)
	requireRuntimeStates(t, lab, plans, runtime.StateOK)
	return p
}

// runtimeStatus is verify's report, failing on a read error.
func runtimeStatus(t *testing.T, lab *labtest.Lab, plans map[string]runtime.Plan) runtime.Status {
	t.Helper()
	status, err := runtime.Verify(context.Background(), lab.A, lab.B, plans)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return status
}

// requireRuntimeStates requires every item on both routers to be want.
func requireRuntimeStates(t *testing.T, lab *labtest.Lab, plans map[string]runtime.Plan, want runtime.State) {
	t.Helper()
	status := runtimeStatus(t, lab, plans)
	for _, router := range []string{"a", "b"} {
		if len(status[router]) != len(plans[router].Ops) {
			t.Errorf("router %s: verify reported %d items for %d ops", router, len(status[router]), len(plans[router].Ops))
		}
		for _, it := range status[router] {
			if it.State != want {
				t.Errorf("router %s: %s is %s (%s), want %s", router, it.Label, it.State, it.Note, want)
			}
		}
	}
}

// tagged is every static entry in section whose comment starts "mtha:".
func tagged(t *testing.T, c *routeros.Client, section string) []map[string]string {
	t.Helper()
	var entries []map[string]string
	if err := c.Get(context.Background(), "/"+section, &entries); err != nil {
		t.Fatalf("%s: %v", section, err)
	}
	var out []map[string]string
	for _, e := range entries {
		if e["dynamic"] != "true" && strings.HasPrefix(e["comment"], "mtha:") {
			out = append(out, e)
		}
	}
	return out
}

// managedVRRP is the router's vrrp-mtha, read fresh.
func managedVRRP(t *testing.T, c *routeros.Client) map[string]string {
	t.Helper()
	return only(t, c, "interface/vrrp", byName(rtVRRP))
}

// After a deploy each router holds exactly the four kinds of object, all
// tagged: the VRRP interface carrying both transition scripts, its VIP, one
// netwatch entry per target, and the snapshot scheduler — and nothing else
// tagged in those sections. Verify reads them all back as ok, and a second
// deploy finds nothing to do.
func TestLabRuntimeDeployCreatesTaggedObjects(t *testing.T) {
	lab := labtest.New(t)
	targets := []string{rtTargetA, rtTargetB}
	plans := buildRuntime(t, runtimePair(lab, targets, config.TogglesConfig{}))

	requireRuntimeStates(t, lab, plans, runtime.StateMissing)
	p := deployRuntime(t, lab, plans)
	if targets := p.Targets(); !slices.Equal(targets, []string{"a", "b"}) {
		t.Errorf("deploy wrote to %v, want both routers:%s", targets, describePlan(p))
	}

	rt := lab.Pair.Runtime
	for _, r := range []struct {
		key      string
		c        *routeros.Client
		priority int
	}{{"a", lab.A, rt.PriorityMaster}, {"b", lab.B, rt.PriorityBackup}} {
		t.Run("router "+r.key, func(t *testing.T) {
			vrrp := tagged(t, r.c, "interface/vrrp")
			if len(vrrp) != 1 {
				t.Fatalf("%d mtha-tagged VRRP interfaces, want 1: %v", len(vrrp), vrrp)
			}
			v := vrrp[0]
			for field, want := range map[string]string{
				"name": rtVRRP, "comment": "mtha:vrrp:" + rtVRRP, "interface": "ether2",
				"vrid": strconv.Itoa(rtVRID), "priority": strconv.Itoa(r.priority),
			} {
				if v[field] != want {
					t.Errorf("VRRP interface %s = %q, want %q", field, v[field], want)
				}
			}
			// The transition scripts are fields of the interface, not
			// /system/script entries.
			for field, marker := range map[string]string{"on-master": "# mtha:on-master:", "on-backup": "# mtha:on-backup:"} {
				if !strings.HasPrefix(v[field], marker+rtVRRP) {
					t.Errorf("VRRP interface %s = %q, want mtha's script (%s%s)", field, v[field], marker, rtVRRP)
				}
			}
			for _, s := range tagged(t, r.c, "system/script") {
				t.Errorf("a /system/script entry is mtha-tagged: %v", s)
			}

			addrs := tagged(t, r.c, "ip/address")
			if len(addrs) != 1 || addrs[0]["address"] != rtVIP || addrs[0]["interface"] != rtVRRP ||
				addrs[0]["comment"] != "mtha:vrrp:"+rtVRRP+":"+rtVIP {
				t.Errorf("mtha-tagged addresses %v, want just %s on %s", addrs, rtVIP, rtVRRP)
			}

			var hosts []string
			for _, n := range tagged(t, r.c, "tool/netwatch") {
				hosts = append(hosts, n["host"])
				if n["comment"] != "mtha:netwatch:"+n["host"] {
					t.Errorf("netwatch %s is tagged %q", n["host"], n["comment"])
				}
				for _, f := range []string{"up-script", "down-script"} {
					if !strings.HasPrefix(n[f], "# mtha:netwatch:"+n["host"]) {
						t.Errorf("netwatch %s %s = %q, want mtha's script", n["host"], f, n[f])
					}
				}
			}
			slices.Sort(hosts)
			if !slices.Equal(hosts, targets) {
				t.Errorf("mtha-tagged netwatch hosts %v, want one per target %v", hosts, targets)
			}

			sched := tagged(t, r.c, "system/scheduler")
			if len(sched) != 1 || sched[0]["name"] != "mtha-snapshot" || sched[0]["comment"] != "mtha:scheduler:snapshot" {
				t.Errorf("mtha-tagged schedulers %v, want just mtha-snapshot", sched)
			}
		})
	}

	// The deploy took a pre-apply backup on each router before writing.
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		var files []map[string]string
		if err := c.Get(context.Background(), "/file?name="+rtBackupName+".backup", &files); err != nil || len(files) != 1 {
			t.Errorf("pre-apply backup %s.backup: %v, %v", rtBackupName, files, err)
		}
	}

	// Idempotent: a second deploy, planned from fresh reads, writes nothing
	// and skips nothing, and everything still verifies.
	if again := runtimeWrites(t, lab, plans, runtime.ActionDeploy); !again.Empty() || len(again.Skipped) > 0 {
		t.Errorf("second deploy is not a no-op:%s", describePlan(again))
	}
	requireRuntimeStates(t, lab, plans, runtime.StateOK)
}

// The snapshot scheduler is in a synced section, so what a deploy creates
// must be the same entry on both routers however far apart the two creates
// land: a field the device fills in from its clock at creation (start-date,
// start-time) would differ between them for good, and that drift is mtha's
// own doing and keeps a healthy pair from ever being Ready. Router b's
// create is held back here until the clocks have moved on a second.
func TestLabRuntimeDeployLeavesSchedulerClean(t *testing.T) {
	lab := labtest.New(t)
	plans := buildRuntime(t, runtimePair(lab, []string{rtTargetA}, config.TogglesConfig{}))

	p := runtimeWrites(t, lab, plans, runtime.ActionDeploy)
	created := map[string]bool{}
	for _, op := range p.Ops {
		if op.Section == "system/scheduler" {
			if op.Method != plan.MethodCreate {
				t.Fatalf("deploy plans %s for the scheduler on router %s, want a create:%s", op.Method, op.Router, describePlan(p))
			}
			if op.Router == "b" {
				if !created["a"] {
					t.Fatalf("router b's scheduler create comes before router a's:%s", describePlan(p))
				}
				time.Sleep(1500 * time.Millisecond)
			}
			created[op.Router] = true
		}
		if err := plan.Execute(context.Background(), lab.Client(op.Router), op); err != nil {
			t.Fatalf("%s %s %v: %v", op.Method, op.Path, op.Body, err)
		}
	}
	if !created["a"] || !created["b"] {
		t.Fatalf("deploy did not create the scheduler on both routers:%s", describePlan(p))
	}
	requireRuntimeStates(t, lab, plans, runtime.StateOK)

	a := only(t, lab.A, "system/scheduler", byName("mtha-snapshot"))
	b := only(t, lab.B, "system/scheduler", byName("mtha-snapshot"))
	for _, field := range []string{"start-date", "start-time", "interval"} {
		if a[field] == "" || a[field] != b[field] {
			t.Errorf("mtha-snapshot %s is %q on router a and %q on router b, want one value on both", field, a[field], b[field])
		}
	}
	requireClean(t, lab, "system/scheduler")
}

// A hand-written on-master on the managed interface is someone's script:
// verify reports the interface as a conflict on that router only, and a
// deploy leaves the script alone (saying so) rather than restoring mtha's.
func TestLabRuntimeHandWrittenOnMasterIsAConflict(t *testing.T) {
	lab := labtest.New(t)
	plans := buildRuntime(t, runtimePair(lab, []string{rtTargetA}, config.TogglesConfig{}))
	deployRuntime(t, lab, plans)

	const handWritten = `:log info "operator's own on-master"`
	patch(t, lab.A, "interface/vrrp", managedVRRP(t, lab.A)[".id"], map[string]string{"on-master": handWritten})

	status := runtimeStatus(t, lab, plans)
	for _, router := range []string{"a", "b"} {
		for _, it := range status[router] {
			want := runtime.StateOK
			if router == "a" && it.Label == "vrrp interface "+rtVRRP {
				want = runtime.StateConflict
			}
			if it.State != want {
				t.Errorf("router %s: %s is %s, want %s", router, it.Label, it.State, want)
			}
		}
	}

	p := runtimeWrites(t, lab, plans, runtime.ActionDeploy)
	for _, op := range p.Ops {
		if op.Section == "interface/vrrp" {
			t.Errorf("deploy writes the VRRP interface despite the hand-written script:%s", describePlan(p))
		}
	}
	skipped := false
	for _, s := range p.Skipped {
		if s.Router == "a" && s.Section == "interface/vrrp" && strings.Contains(s.Reason, "on-master") {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("deploy does not report leaving router a's on-master alone:%s", describePlan(p))
	}
	execute(t, lab, p)

	if got := managedVRRP(t, lab.A)["on-master"]; got != handWritten {
		t.Errorf("router a's on-master is now %q, want the hand-written %q kept", got, handWritten)
	}
	if got := managedVRRP(t, lab.B)["on-master"]; !strings.HasPrefix(got, "# mtha:on-master:") {
		t.Errorf("router b's on-master is %q, want mtha's untouched", got)
	}
}

// Remove leaves no mtha-tagged object in any of the four sections, deletes
// each router's VIP before the VRRP interface it sits on, and takes nothing
// that was already there: the pair is back at its baseline shape.
func TestLabRuntimeRemoveLeavesNothingBehind(t *testing.T) {
	lab := labtest.New(t)
	plans := buildRuntime(t, runtimePair(lab, []string{rtTargetA, rtTargetB}, config.TogglesConfig{}))

	before := map[string]map[string]int{}
	for _, r := range []struct {
		key string
		c   *routeros.Client
	}{{"a", lab.A}, {"b", lab.B}} {
		before[r.key] = sectionCounts(t, r.c)
	}

	deployRuntime(t, lab, plans)

	p := runtimeWrites(t, lab, plans, runtime.ActionRemove)
	for _, router := range []string{"a", "b"} {
		vip, vrrp := -1, -1
		deletes := 0
		for i, op := range p.Ops {
			if op.Router != router || op.Method != plan.MethodDelete {
				continue
			}
			deletes++
			switch op.Section {
			case "ip/address":
				vip = i
			case "interface/vrrp":
				vrrp = i
			}
		}
		if want := len(plans[router].Ops); deletes != want {
			t.Errorf("router %s: remove deletes %d objects, want all %d:%s", router, deletes, want, describePlan(p))
		}
		if vip < 0 || vrrp < 0 || vip > vrrp {
			t.Errorf("router %s: remove does not delete the VIP before its VRRP interface:%s", router, describePlan(p))
		}
	}
	execute(t, lab, p)

	requireRuntimeStates(t, lab, plans, runtime.StateMissing)
	for _, r := range []struct {
		key string
		c   *routeros.Client
	}{{"a", lab.A}, {"b", lab.B}} {
		for _, section := range rtSections {
			if left := tagged(t, r.c, section); len(left) > 0 {
				t.Errorf("router %s: %s still has mtha-tagged entries %v", r.key, section, left)
			}
		}
		if after := sectionCounts(t, r.c); !mapsEqual(after, before[r.key]) {
			t.Errorf("router %s: entry counts after remove %v, before deploy %v", r.key, after, before[r.key])
		}
	}
	if again := runtimeWrites(t, lab, plans, runtime.ActionRemove); !again.Empty() {
		t.Errorf("second remove is not a no-op:%s", describePlan(again))
	}
}

// sectionCounts is how many static entries each deploy section holds.
func sectionCounts(t *testing.T, c *routeros.Client) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, section := range rtSections {
		var entries []map[string]string
		if err := c.Get(context.Background(), "/"+section, &entries); err != nil {
			t.Fatalf("%s: %v", section, err)
		}
		for _, e := range entries {
			if e["dynamic"] != "true" {
				counts[section]++
			}
		}
	}
	return counts
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// waitVRRP waits until router c's vrrp-mtha has priority and role.
func waitVRRP(t *testing.T, key string, c *routeros.Client, priority int, role routeros.VRRPRole, timeout time.Duration) {
	t.Helper()
	eventually(t, fmt.Sprintf("router %s's %s at priority %d as %s", key, rtVRRP, priority, role), timeout, func() error {
		v, err := vrrpNamed(c, rtVRRP)
		if err != nil {
			return err
		}
		if v.Priority != strconv.Itoa(priority) || v.Role() != role {
			return fmt.Errorf("priority %s, %s", v.Priority, v.Role())
		}
		return nil
	})
}

func vrrpNamed(c *routeros.Client, name string) (routeros.VRRPInstance, error) {
	all, err := c.VRRP(context.Background())
	if err != nil {
		return routeros.VRRPInstance{}, err
	}
	for _, v := range all {
		if v.Name == name {
			return v, nil
		}
	}
	return routeros.VRRPInstance{}, fmt.Errorf("no VRRP interface %s", name)
}
