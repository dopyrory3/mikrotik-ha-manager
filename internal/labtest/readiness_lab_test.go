//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"mtha/internal/labtest"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// The readiness verdict (project.md §5.2) against a live pair. The golden
// path is the one only a live pair can reach: every check green at once.
// Each other test starts from that state, turns one condition red on the
// device, and asserts the verdict and exactly which checks fail and why —
// then, where the condition can be undone in place, that the verdict
// recovers.
//
// Some checks depend on others: versions, VRRP and netwatch can only pass
// with both routers reachable, so stopping a router fails all four. Drift
// and runtime are fetched on demand (screens 2 and 3), not polled, so they
// keep their last result until re-checked; the tests that flip them
// re-check them, as an operator would.

// Everything clean, runtime deployed, netwatch up: Ready.
func TestLabReadinessReady(t *testing.T) {
	s := readyPair(t)
	v := s.requireReady(pollWait)
	if !strings.Contains(v, "Readiness: Ready") {
		t.Fatalf("dashboard:\n%s", v)
	}
	// The state behind the verdict, on the devices themselves.
	ctx := context.Background()
	for _, c := range []struct {
		key  string
		c    *routeros.Client
		role routeros.VRRPRole
	}{{"a", s.lab.A, routeros.RoleMaster}, {"b", s.lab.B, routeros.RoleBackup}} {
		if got := onlyVRRP(t, c.c).Role(); got != c.role {
			t.Errorf("router %s: VRRP role %s, want %s", c.key, got, c.role)
		}
		nw, err := c.c.Netwatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(nw) != 2 {
			t.Errorf("router %s: %d netwatch entries, want mtha's two: %+v", c.key, len(nw), nw)
		}
		for _, e := range nw {
			if e.Status != "up" || !strings.HasPrefix(e.Comment, "mtha:netwatch:") {
				t.Errorf("router %s: netwatch %+v, want an mtha target, up", c.key, e)
			}
		}
	}
}

// A router that stops answering fails reachability, and with it every check
// that needs both routers' live state.
func TestLabReadinessRouterStopped(t *testing.T) {
	s := readyPair(t)
	container := s.lab.Container("b")

	docker(t, "stop", container)
	// Runs before the harness's reset, which needs router b answering to
	// load the golden backup.
	t.Cleanup(func() { startRouter(t, s.lab, "b") })

	s.requireReadiness(wantReadiness("Degraded", map[string]string{
		checkReachable: "router b unreachable",
		checkVersions:  "",
		checkMaster:    "",
		checkNetwatch:  "",
	}), pollWait)
	if v := s.screen(); !strings.Contains(v, "unreachable") {
		t.Errorf("router b's panel does not say unreachable:\n%s", v)
	}

	// Back up, it rejoins as backup and the verdict recovers without
	// re-fetching anything.
	startRouter(t, s.lab, "b")
	s.requireRoles("master", "backup", 2*time.Minute)
	s.requireReady(time.Minute)
}

// Drift in a synced section fails the drift check once drift is re-fetched,
// naming how many sections drift.
func TestLabReadinessDrift(t *testing.T) {
	s := readyPair(t)
	add(t, s.lab.A, "ip/firewall/filter", map[string]string{
		"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "5353", "comment": "mtha-lab-readiness-drift",
	})
	s.fetchDrift()
	s.requireReadiness(wantReadiness("Degraded", map[string]string{
		checkDrift: "1 section(s) have drift",
	}), pollWait)
}

// Two masters for one VRRP instance — split brain, forced by dropping VRRP
// adverts on router b so it no longer hears router a — fail the VRRP check.
// Letting the adverts through again hands mastership back to a.
//
// The lab's netwatch targets are the routers' own ether2 addresses, on the
// VIP's subnet. In split brain both routers hold the VIP, so each has two
// connected routes to 192.168.88.0/24, and a probe of the peer that leaves by
// vrrp-lan carries the VIP as its source — which the peer, holding it too,
// answers locally. About half the probes are lost that way, so whether a
// netwatch goes down in any one interval is chance; when it does, its
// down-script lowers the priority, which restarts VRRP and drops that router
// to backup, and the up-script's restore restarts it again. The pair then
// flaps under the test instead of holding two masters. Pinning each router's
// probe of its peer to ether2 keeps netwatch out of it, so this test turns
// only the VRRP check red, and the hold below proves the scripts stay quiet
// for a whole netwatch interval.
func TestLabReadinessTwoMasters(t *testing.T) {
	s := readyPair(t)
	pins := map[*routeros.Client]string{}
	for _, p := range []struct {
		c         *routeros.Client
		own, peer string
	}{{s.lab.A, "192.168.88.2", "192.168.88.3"}, {s.lab.B, "192.168.88.3", "192.168.88.2"}} {
		pins[p.c] = add(t, p.c, "ip/route", map[string]string{
			"dst-address": p.peer + "/32", "gateway": "ether2", "pref-src": p.own, "comment": "mtha-lab-pin-peer",
		})
	}
	id := add(t, s.lab.B, "ip/firewall/raw", map[string]string{
		"chain": "prerouting", "action": "drop", "protocol": "vrrp", "in-interface": "ether2", "comment": "mtha-lab-split-brain",
	})
	waitRole(t, s.lab.B, routeros.RoleMaster)
	s.requireRoles("master", "master", pollWait)
	s.requireReadiness(wantReadiness("Degraded", map[string]string{checkMaster: ""}), pollWait)
	holds(t, "both masters at base priority, netwatch up", netwatchInterval+2*time.Second, func() error {
		for _, r := range []struct {
			c        *routeros.Client
			priority int
		}{{s.lab.A, s.lab.Pair.Runtime.PriorityMaster}, {s.lab.B, s.lab.Pair.Runtime.PriorityBackup}} {
			v := onlyVRRP(t, r.c)
			if v.Role() != routeros.RoleMaster || v.Priority != strconv.Itoa(r.priority) {
				return fmt.Errorf("VRRP %+v, want master at priority %d", v, r.priority)
			}
			for _, host := range []string{"192.168.88.2", "192.168.88.3"} {
				if err := netwatchStatus(r.c, host, "up"); err != nil {
					return err
				}
			}
		}
		return nil
	})

	remove(t, s.lab.B, "ip/firewall/raw", id)
	s.requireRoles("master", "backup", pollWait)
	for c, pin := range pins {
		remove(t, c, "ip/route", pin)
	}
	s.requireReady(pollWait)
}

// netwatchInterval is the probe interval internal/runtime deploys on every
// netwatch entry.
const netwatchInterval = 10 * time.Second

// holds fails t unless fn succeeds on every check, twice a second, for d.
func holds(t *testing.T, what string, d time.Duration, fn func() error) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if err := fn(); err != nil {
			t.Fatalf("%s did not hold for %s: %v", what, d, err)
		}
	}
}

// Runtime logic missing from one router fails the runtime check once it is
// re-verified, naming the first missing object.
func TestLabReadinessRuntimeUndeployed(t *testing.T) {
	s := readyPair(t)
	sched := only(t, s.lab.B, "system/scheduler", byComment("mtha:scheduler:snapshot"))
	remove(t, s.lab.B, "system/scheduler", sched[".id"])
	s.verifyRuntime()
	s.requireReadiness(wantReadiness("Degraded", map[string]string{
		checkRuntime: "router b: scheduler mtha-snapshot missing",
	}), pollWait)
}

// A netwatch target the standby can no longer reach fails the netwatch
// check. mtha's down-script drops the standby to its degraded priority,
// which is still backup and still a priority the runtime check accepts, so
// nothing else turns red; once the target answers again the up-script
// restores its priority and the verdict recovers.
func TestLabReadinessStandbyNetwatchDown(t *testing.T) {
	s := readyPair(t)
	// Only b's own echo requests to a: dropping b's replies as well would
	// take a's netwatch of b down too, and a's down-script with it.
	id := add(t, s.lab.B, "ip/firewall/raw", map[string]string{
		"chain": "output", "action": "drop", "protocol": "icmp", "icmp-options": "8:0", "dst-address": "192.168.88.2",
		"comment": "mtha-lab-netwatch-down",
	})
	eventually(t, "router b's netwatch of 192.168.88.2 to go down", time.Minute, func() error {
		return netwatchStatus(s.lab.B, "192.168.88.2", "down")
	})
	s.requireReadiness(wantReadiness("Degraded", map[string]string{checkNetwatch: ""}), pollWait)

	eventually(t, "router b's down-script to lower its priority", pollWait, func() error {
		return priorityIs(s.lab.B, s.lab.Pair.Runtime.PriorityDegraded)
	})
	s.verifyRuntime()
	s.requireReadiness(wantReadiness("Degraded", map[string]string{checkNetwatch: ""}), pollWait)
	s.requireRoles("master", "backup", pollWait)

	remove(t, s.lab.B, "ip/firewall/raw", id)
	eventually(t, "router b's up-script to restore its priority", time.Minute, func() error {
		return priorityIs(s.lab.B, s.lab.Pair.Runtime.PriorityBackup)
	})
	s.requireReady(pollWait)
}

// VRRP disabled on both routers leaves no master at all: Degraded.
func TestLabReadinessZeroMasters(t *testing.T) {
	s := readyPair(t)
	setVRRPDisabled(t, s.lab.A, true)
	setVRRPDisabled(t, s.lab.B, true)
	s.requireRoles("unknown", "unknown", pollWait)
	s.requireReadiness(wantReadiness("Degraded", map[string]string{checkMaster: ""}), pollWait)
}

// A role the tool cannot decode fails closed. With router b's VRRP disabled
// it reports neither role flag nor a state: b is not known to be backup, so
// "exactly one master" cannot be claimed even though a is master.
func TestLabReadinessUnknownRoleFailsClosed(t *testing.T) {
	s := readyPair(t)
	setVRRPDisabled(t, s.lab.B, true)
	s.requireRoles("master", "unknown", pollWait)
	s.requireReadiness(wantReadiness("Degraded", map[string]string{checkMaster: ""}), pollWait)
}

func netwatchStatus(c *routeros.Client, host, want string) error {
	nw, err := c.Netwatch(context.Background())
	if err != nil {
		return err
	}
	for _, e := range nw {
		if e.Host == host {
			if e.Status != want {
				return fmt.Errorf("netwatch %s is %s", host, e.Status)
			}
			return nil
		}
	}
	return fmt.Errorf("no netwatch entry for %s", host)
}

func priorityIs(c *routeros.Client, want int) error {
	vrrp, err := c.VRRP(context.Background())
	if err != nil {
		return err
	}
	if len(vrrp) != 1 || vrrp[0].Priority != strconv.Itoa(want) {
		return fmt.Errorf("VRRP %+v, want priority %d", vrrp, want)
	}
	return nil
}

func docker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// startRouter starts router key's lab container, if it is not running, and
// waits until its guest answers REST again.
func startRouter(t *testing.T, lab *labtest.Lab, key string) {
	t.Helper()
	container := lab.Container(key)
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).Output()
	if err != nil {
		t.Errorf("docker inspect %s: %v", container, err)
		return
	}
	if strings.TrimSpace(string(out)) != "true" {
		docker(t, "start", container)
	}
	c := lab.Pollers(time.Second)[poll.RouterKey(key)].Client
	eventually(t, container+" answering REST", 3*time.Minute, func() error {
		_, err := c.SystemResource(context.Background())
		return err
	})
}
