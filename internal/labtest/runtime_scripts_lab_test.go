//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/routeros"
)

// The deployed scripts, run by the routers themselves. Deploying the right
// text proves little: these tests make RouterOS run it — netwatch seeing a
// target go down or come back, VRRP electing a new master — and assert over
// REST on what the scripts did.

// netwatchTimeout covers a netwatch entry noticing a change of its host
// (it probes every 10s) and its script running.
const netwatchTimeout = 60 * time.Second

// electionTimeout covers VRRP re-electing after a priority change: well
// under a second from backup to master, up to ~9s the other way (README.md).
const electionTimeout = 30 * time.Second

type labRouter struct {
	key string
	c   *routeros.Client
}

func labRouters(lab *labtest.Lab) []labRouter {
	return []labRouter{{"a", lab.A}, {"b", lab.B}}
}

// pointNetwatch re-points the mtha netwatch entry for target at host.
func pointNetwatch(t *testing.T, c *routeros.Client, target, host string) {
	t.Helper()
	e := only(t, c, "tool/netwatch", byComment("mtha:netwatch:"+target))
	patch(t, c, "tool/netwatch", e[".id"], map[string]string{"host": host})
}

// skipStartupDelay lets the mtha netwatch entry for target probe at once.
// RouterOS holds every netwatch probe for startup-delay (default 5m) after
// boot, and each test starts just after the reset's reboot, so without this
// the entries would sit at "unknown" for the first five minutes. mtha does
// not set the field, so this changes nothing verify compares.
func skipStartupDelay(t *testing.T, c *routeros.Client, target string) {
	t.Helper()
	e := only(t, c, "tool/netwatch", byComment("mtha:netwatch:"+target))
	patch(t, c, "tool/netwatch", e[".id"], map[string]string{"startup-delay": "0s"})
}

// waitNetwatch waits until the mtha netwatch entry for target reports status.
func waitNetwatch(t *testing.T, r labRouter, target, status string) {
	t.Helper()
	eventually(t, fmt.Sprintf("router %s's netwatch %s to be %s", r.key, target, status), netwatchTimeout, func() error {
		e := only(t, r.c, "tool/netwatch", byComment("mtha:netwatch:"+target))
		if e["status"] != status {
			return fmt.Errorf("status %q (host %s)", e["status"], e["host"])
		}
		return nil
	})
}

// waitPriority waits until router r's vrrp-mtha has priority.
func waitPriority(t *testing.T, r labRouter, priority int) {
	t.Helper()
	eventually(t, fmt.Sprintf("router %s's %s at priority %d", r.key, rtVRRP, priority), netwatchTimeout, func() error {
		v, err := vrrpNamed(r.c, rtVRRP)
		if err != nil {
			return err
		}
		if v.Priority != fmt.Sprint(priority) {
			return fmt.Errorf("priority %s", v.Priority)
		}
		return nil
	})
}

// requirePriority asserts router r's vrrp-mtha holds priority now.
func requirePriority(t *testing.T, r labRouter, priority int, why string) {
	t.Helper()
	v, err := vrrpNamed(r.c, rtVRRP)
	if err != nil {
		t.Fatal(err)
	}
	if v.Priority != fmt.Sprint(priority) {
		t.Errorf("router %s's %s is at priority %s, want %d: %s", r.key, rtVRRP, v.Priority, priority, why)
	}
}

// requireUnmanagedVRRPUntouched checks the baseline's own vrrp-lan still has
// provision.sh's priority: the netwatch scripts only move mtha's.
func requireUnmanagedVRRPUntouched(t *testing.T, lab *labtest.Lab) {
	t.Helper()
	for _, r := range []struct {
		labRouter
		priority string
	}{{labRouter{"a", lab.A}, "200"}, {labRouter{"b", lab.B}, "100"}} {
		v, err := vrrpNamed(r.c, labtest.VRRPName)
		if err != nil {
			t.Fatal(err)
		}
		if v.Priority != r.priority {
			t.Errorf("router %s: the unmanaged %s moved to priority %s (want %s)", r.key, labtest.VRRPName, v.Priority, r.priority)
		}
	}
}

// deployAtBase deploys plans for pair and waits until the pair has settled
// with a master of vrrp-mtha at priority_master and b its backup at
// priority_backup, and every mtha netwatch entry up.
func deployAtBase(t *testing.T, lab *labtest.Lab, pair *config.Pair) {
	t.Helper()
	deployRuntime(t, lab, buildRuntime(t, pair))
	for _, r := range labRouters(lab) {
		for _, target := range pair.Runtime.NetwatchTargets {
			skipStartupDelay(t, r.c, target)
			waitNetwatch(t, r, target, "up")
		}
	}
	rt := pair.Runtime
	waitVRRP(t, "a", lab.A, rt.PriorityMaster, routeros.RoleMaster, electionTimeout)
	waitVRRP(t, "b", lab.B, rt.PriorityBackup, routeros.RoleBackup, electionTimeout)
}

// A target going down drops each router's managed VRRP priority to
// priority_degraded; coming back restores each router's own base — a to
// priority_master, b to priority_backup, not both to master's, which would
// let the standby hold on to mastership.
func TestLabRuntimeNetwatchDegradesAndRestores(t *testing.T) {
	lab := labtest.New(t)
	pair := runtimePair(lab, []string{rtTargetB}, config.TogglesConfig{})
	rt := pair.Runtime
	deployAtBase(t, lab, pair)
	logMarks := logMarks(t, lab)

	for _, r := range labRouters(lab) {
		pointNetwatch(t, r.c, rtTargetB, rtUnreachable)
	}
	for _, r := range labRouters(lab) {
		waitNetwatch(t, r, rtTargetB, "down")
		waitPriority(t, r, rt.PriorityDegraded)
	}
	requireUnmanagedVRRPUntouched(t, lab)

	for _, r := range labRouters(lab) {
		pointNetwatch(t, r.c, rtTargetB, rtTargetB)
	}
	for _, r := range labRouters(lab) {
		waitNetwatch(t, r, rtTargetB, "up")
	}
	waitVRRP(t, "a", lab.A, rt.PriorityMaster, routeros.RoleMaster, netwatchTimeout)
	waitVRRP(t, "b", lab.B, rt.PriorityBackup, routeros.RoleBackup, netwatchTimeout)
	requireUnmanagedVRRPUntouched(t, lab)
	requireNoScriptErrors(t, lab, logMarks)
}

// With two targets, either one down holds the router degraded, and it is
// restored only once both are up again: the first target's up-script must
// not undo the second's down-script.
//
// The up-script's count of entries still down is what this pins (issue
// #26): it once filtered on "disabled=no status=down", and on RouterOS
// 7.23.7 an enabled netwatch entry has no disabled property at all, so
// that matched nothing, the count was always 0, and the first target to
// come back restored the base priority while the other was still down.
func TestLabRuntimeNetwatchTwoTargets(t *testing.T) {
	lab := labtest.New(t)
	pair := runtimePair(lab, []string{rtTargetA, rtTargetB}, config.TogglesConfig{})
	rt := pair.Runtime
	deployAtBase(t, lab, pair)
	logMarks := logMarks(t, lab)
	routers := labRouters(lab)

	// One down, the other up: degraded.
	for _, r := range routers {
		pointNetwatch(t, r.c, rtTargetA, rtUnreachable)
	}
	for _, r := range routers {
		waitNetwatch(t, r, rtTargetA, "down")
		waitPriority(t, r, rt.PriorityDegraded)
		waitNetwatch(t, r, rtTargetB, "up")
	}

	// Both down, then the first back up: still degraded, since the
	// second is down.
	for _, r := range routers {
		pointNetwatch(t, r.c, rtTargetB, rtUnreachable)
	}
	for _, r := range routers {
		waitNetwatch(t, r, rtTargetB, "down")
	}
	for _, r := range routers {
		pointNetwatch(t, r.c, rtTargetA, rtTargetA)
	}
	for _, r := range routers {
		waitNetwatch(t, r, rtTargetA, "up")
	}
	// The up-script runs as the status changes; give it a moment to have
	// run (and wrongly restored, if it were going to) before looking.
	time.Sleep(3 * time.Second)
	for _, r := range routers {
		requirePriority(t, r, rt.PriorityDegraded, rtTargetA+" is up but "+rtTargetB+" is still down")
	}

	// Both up: restored, each to its own base.
	for _, r := range routers {
		pointNetwatch(t, r.c, rtTargetB, rtTargetB)
	}
	for _, r := range routers {
		waitNetwatch(t, r, rtTargetB, "up")
	}
	waitVRRP(t, "a", lab.A, rt.PriorityMaster, routeros.RoleMaster, netwatchTimeout)
	waitVRRP(t, "b", lab.B, rt.PriorityBackup, routeros.RoleBackup, netwatchTimeout)
	requireUnmanagedVRRPUntouched(t, lab)
	requireNoScriptErrors(t, lab, logMarks)
}

// The lab fixture's toggle targets: its DHCP server and the static route
// provision.sh tags "mtha:lab-route". Both start enabled on both routers.
const (
	toggleDHCP     = "dhcp-lab"
	toggleRoute    = "mtha:lab-route"
	toggleRouteDst = "10.99.0.0/16"
)

// toggleState is whether router c's DHCP server and route are enabled.
func toggleState(t *testing.T, c *routeros.Client) (dhcp, route bool) {
	t.Helper()
	d := only(t, c, "ip/dhcp-server", byName(toggleDHCP))
	r := only(t, c, "ip/route", byComment(toggleRoute))
	return d["disabled"] != "true", r["disabled"] != "true"
}

// waitToggles waits until router r serves DHCP and carries the route
// (enabled) or does neither.
func waitToggles(t *testing.T, r labRouter, enabled bool, why string) {
	t.Helper()
	eventually(t, fmt.Sprintf("router %s's %s and %s route enabled=%v (%s)", r.key, toggleDHCP, toggleRoute, enabled, why), electionTimeout, func() error {
		dhcp, route := toggleState(t, r.c)
		if dhcp != enabled || route != enabled {
			return fmt.Errorf("dhcp enabled=%v, route enabled=%v", dhcp, route)
		}
		return nil
	})
}

// failoverWithToggles deploys pair (whose toggles name the fixture's DHCP
// server and route), checks the toggles follow the initial election, then
// fails over for real — a's priority dropped below b's, so b preempts — and
// checks they follow it: on for the new master, off for the old. It returns
// each router's log from just before the failover.
func failoverWithToggles(t *testing.T, lab *labtest.Lab, pair *config.Pair) map[string][]routeros.LogEntry {
	t.Helper()
	deployAtBase(t, lab, pair)
	a, b := labRouter{"a", lab.A}, labRouter{"b", lab.B}
	waitToggles(t, a, true, "a is master")
	waitToggles(t, b, false, "b is backup")

	marks := logMarks(t, lab)
	patch(t, lab.A, "interface/vrrp", managedVRRP(t, lab.A)[".id"],
		map[string]string{"priority": fmt.Sprint(pair.Runtime.PriorityDegraded)})
	waitVRRP(t, "b", lab.B, pair.Runtime.PriorityBackup, routeros.RoleMaster, electionTimeout)
	waitVRRP(t, "a", lab.A, pair.Runtime.PriorityDegraded, routeros.RoleBackup, electionTimeout)
	waitToggles(t, b, true, "b is the new master")
	waitToggles(t, a, false, "a is the old master")
	return logsSince(t, lab, marks)
}

// On a real failover the new master brings the route up and then starts
// DHCP, and the old master stops DHCP and then drops the route — so the
// master never offers leases before it can route for them.
func TestLabRuntimeTogglesFollowFailover(t *testing.T) {
	lab := labtest.New(t)
	pair := runtimePair(lab, nil, config.TogglesConfig{DHCPServers: []string{toggleDHCP}, Routes: []string{toggleRoute}})
	logs := failoverWithToggles(t, lab, pair)

	requireNoErrorsIn(t, logs)
	for _, c := range []struct {
		key         string
		first, then string
	}{
		{"b", "route", "dhcp"}, // master: route up before DHCP
		{"a", "dhcp", "route"}, // backup: DHCP down before the route
	} {
		order := toggleOrder(logs[c.key])
		if len(order) != 2 || order[0] != c.first || order[1] != c.then {
			t.Errorf("router %s toggled %v, want %s then %s; log:\n%s", c.key, order, c.first, c.then, logText(logs[c.key]))
		}
	}
}

// A toggle naming something the router doesn't have is a no-op, not a
// script error: listed first, ahead of the real ones, it must not stop the
// script before them.
func TestLabRuntimeToggleOfMissingObjectIsNoOp(t *testing.T) {
	lab := labtest.New(t)
	pair := runtimePair(lab, nil, config.TogglesConfig{
		DHCPServers: []string{"mtha-lab-no-such-dhcp", toggleDHCP},
		Routes:      []string{"mtha:no-such-route", toggleRoute},
	})
	logs := failoverWithToggles(t, lab, pair)
	requireNoErrorsIn(t, logs)
}

// logMarks is how many entries each router's log holds now, so a later
// read can look at only what came after.
func logMarks(t *testing.T, lab *labtest.Lab) map[string]int {
	t.Helper()
	marks := map[string]int{}
	for _, r := range labRouters(lab) {
		entries, err := r.c.Log(context.Background())
		if err != nil {
			t.Fatalf("router %s log: %v", r.key, err)
		}
		marks[r.key] = len(entries)
	}
	return marks
}

func logsSince(t *testing.T, lab *labtest.Lab, marks map[string]int) map[string][]routeros.LogEntry {
	t.Helper()
	logs := map[string][]routeros.LogEntry{}
	for _, r := range labRouters(lab) {
		entries, err := r.c.Log(context.Background())
		if err != nil {
			t.Fatalf("router %s log: %v", r.key, err)
		}
		if marks[r.key] > len(entries) {
			t.Fatalf("router %s log shrank from %d to %d entries", r.key, marks[r.key], len(entries))
		}
		logs[r.key] = entries[marks[r.key]:]
	}
	return logs
}

func requireNoScriptErrors(t *testing.T, lab *labtest.Lab, marks map[string]int) {
	t.Helper()
	requireNoErrorsIn(t, logsSince(t, lab, marks))
}

// requireNoErrorsIn fails on any error-topic log entry, or any entry
// mentioning a script error.
func requireNoErrorsIn(t *testing.T, logs map[string][]routeros.LogEntry) {
	t.Helper()
	for key, entries := range logs {
		for _, e := range entries {
			if strings.Contains(e.Topics, "error") || strings.Contains(strings.ToLower(e.Message), "error") {
				t.Errorf("router %s logged an error: %s %s %s", key, e.Time, e.Topics, e.Message)
			}
		}
	}
}

// toggleOrder is which of the fixture's toggle targets log entries show
// being changed, in order: "route" and "dhcp".
func toggleOrder(entries []routeros.LogEntry) []string {
	var order []string
	for _, e := range entries {
		switch {
		case !strings.Contains(e.Message, "changed"):
		case strings.Contains(e.Message, "route "+toggleRouteDst):
			order = append(order, "route")
		case strings.Contains(e.Message, "dhcp server "+toggleDHCP):
			order = append(order, "dhcp")
		}
	}
	return order
}

func logText(entries []routeros.LogEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s %s %s\n", e.Time, e.Topics, e.Message)
	}
	return b.String()
}
