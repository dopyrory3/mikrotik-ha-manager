//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"mtha/internal/labtest"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

// The apply's safety behaviour (issue #13, project.md §7.3), against the
// lab. Where a test expects nothing to be written it proves it on the
// device: the target's configuration and files are compared before and
// after, not just the screen.

// syncedSections are the sections a state comparison covers: every section
// the lab pair syncs.
func syncedSections(lab *labtest.Lab) []string { return lab.Pair.Sync.Sections }

// addRuleOnA gives a a commented forward rule b lacks: one hunk, a create
// on b when synced a→b.
func addRuleOnA(t *testing.T, lab *labtest.Lab, comment string) {
	t.Helper()
	add(t, lab.A, filter, map[string]string{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "3389", "comment": comment})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), comment+" [a]")
}

// Without -write the plan renders in full, y is refused, and the target is
// provably untouched.
func TestLabApplyReadOnlyWritesNothing(t *testing.T) {
	lab := labtest.New(t)
	addRuleOnA(t, lab, "lab: read-only")
	before := deviceState(t, lab, lab.B, syncedSections(lab))

	s := startTUI(t, lab, tuiOptions{write: false})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[0].Path != "/system/backup/save" || ops[1].Method != "PUT" || ops[1].Body["comment"] != "lab: read-only" {
		t.Fatalf("the read-only session should still show the full plan, got %v:\n%s", ops, v)
	}
	if !strings.Contains(v, "read-only — restart with -write to apply this plan") {
		t.Errorf("the dry run does not say the session is read-only:\n%s", v)
	}
	s.keys("y")
	if v := s.view(); !strings.Contains(v, "read-only — restart with -write") || finished.MatchString(v) {
		t.Fatalf("y in a read-only session was not refused:\n%s", v)
	}
	s.keys("Y") // not a way round it either
	// Give any write that had been set off time to land: several poll
	// rounds.
	time.Sleep(3 * time.Second)
	s.until("still on the dry run", 5*time.Second, func(v string) bool { return strings.Contains(v, "Dry run:") })

	if after := deviceState(t, lab, lab.B, syncedSections(lab)); after != before {
		t.Fatalf("router b changed in a read-only session:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := preApplyBackups(t, lab.B); len(got) != 0 {
		t.Errorf("a read-only session left pre-apply backups on b: %v", got)
	}
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: read-only [a]")
}

// A plan writing to the VRRP master needs a second, different key: y at the
// master prompt does nothing; only Y writes.
func TestLabApplyMasterNeedsSecondConfirmation(t *testing.T) {
	lab := labtest.New(t)
	add(t, lab.B, filter, map[string]string{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "3389", "comment": "lab: to master"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: to master [b]")
	before := deviceState(t, lab, lab.A, syncedSections(lab))

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.BtoA)
	v := s.plan()
	if !strings.Contains(v, "router A is the current VRRP master (or its state is unknown): a second confirmation will be required") {
		t.Fatalf("the dry run for a write to the master does not warn of the second confirmation:\n%s", v)
	}
	requireMasterRefusesY(t, s, lab, lab.A, before)
	s.keys("Y")
	requireApplied(t, s.waitDone())
	requireSamePosition(t, lab, filter, "forward", byComment("lab: to master"))
	requireClean(t, lab, filter)
}

// requireMasterRefusesY presses y at the review (which must ask for the
// master confirmation), then y again at that prompt, and asserts that
// neither wrote anything to c.
func requireMasterRefusesY(t *testing.T, s *tui, lab *labtest.Lab, c *routeros.Client, before string) {
	t.Helper()
	s.keys("y")
	v := s.view()
	if !strings.Contains(v, "press Y (shift+y) to write to it anyway") {
		t.Fatalf("y did not stop at the master confirmation:\n%s", v)
	}
	s.keys("y")
	time.Sleep(3 * time.Second)
	v = s.until("still at the master confirmation", 5*time.Second, func(v string) bool {
		return strings.Contains(v, "press Y (shift+y) to write to it anyway")
	})
	if finished.MatchString(v) || strings.Contains(v, "applying") {
		t.Fatalf("a second y ran the plan:\n%s", v)
	}
	if after := deviceState(t, lab, c, syncedSections(lab)); after != before {
		t.Fatalf("y, y at the master prompt wrote to the router:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A target whose VRRP role is unknown is treated as a possible master: the
// second confirmation is demanded just the same.
func TestLabApplyUnknownRoleNeedsSecondConfirmation(t *testing.T) {
	// b's VRRP instance disabled: the device reports no role flags at all
	// for it (TestLabDisabledVRRPHasNoRoleFlags), so its role cannot be
	// decoded.
	t.Run("undecodable role", func(t *testing.T) {
		lab := labtest.New(t)
		addRuleOnA(t, lab, "lab: to unknown")
		patch(t, lab.B, "interface/vrrp", onlyVRRP(t, lab.B).ID, map[string]string{"disabled": "true"})
		before := deviceState(t, lab, lab.B, syncedSections(lab))

		s := startTUI(t, lab, tuiOptions{write: true, ready: func(v string) bool {
			return !strings.Contains(v, "waiting for first poll") && !strings.Contains(v, "unreachable") &&
				vrrpMaster.MatchString(v) && !vrrpBackup.MatchString(v) && strings.Contains(v, "unknown")
		}})
		s.drift()
		s.selectSection(filter, plan.AtoB)
		v := s.plan()
		if !strings.Contains(v, "router B is the current VRRP master (or its state is unknown)") {
			t.Fatalf("the dry run does not warn that b needs the second confirmation:\n%s", v)
		}
		requireMasterRefusesY(t, s, lab, lab.B, before)
		s.keys("Y")
		requireApplied(t, s.waitDone())
		requireClean(t, lab, filter)
	})

	// b unreachable when last polled (its container frozen), though it
	// answers again by the time the plan is built. The poll interval is
	// long, so that failed poll stays b's last.
	t.Run("unreachable at last poll", func(t *testing.T) {
		lab := labtest.New(t)
		addRuleOnA(t, lab, "lab: to unreachable")
		container := lab.Container("b")
		dockerRun(t, "pause", container)
		paused := true
		t.Cleanup(func() {
			if paused {
				_ = exec.Command("docker", "unpause", container).Run()
			}
		})

		s := startTUI(t, lab, tuiOptions{write: true, pollers: lab.Pollers(time.Hour), ready: func(v string) bool {
			return !strings.Contains(v, "waiting for first poll") && strings.Contains(v, "unreachable") && vrrpMaster.MatchString(v)
		}})
		dockerRun(t, "unpause", container)
		paused = false
		eventually(t, "router b to answer again", 60*time.Second, func() error {
			_, err := lab.Pollers(time.Second)["b"].Client.Identity(context.Background())
			return err
		})
		if v := s.view(); !strings.Contains(v, "unreachable") {
			t.Fatalf("b's last poll no longer reads unreachable, so this would not test it:\n%s", v)
		}
		before := deviceState(t, lab, lab.B, syncedSections(lab))

		s.drift()
		s.selectSection(filter, plan.AtoB)
		v := s.plan()
		if !strings.Contains(v, "router B is the current VRRP master (or its state is unknown)") {
			t.Fatalf("the dry run does not warn that b needs the second confirmation:\n%s", v)
		}
		requireMasterRefusesY(t, s, lab, lab.B, before)
		s.keys("Y")
		requireApplied(t, s.waitDone())
		requireClean(t, lab, filter)
	})
}

func dockerRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// The pre-run recheck: the target changes between the dry run and the
// confirmation, so the plan rebuilt just before running differs from the
// one confirmed. Nothing may be written — not even the backup — and the new
// plan is shown for review.
func TestLabApplyRecheckRefusesChangedPlan(t *testing.T) {
	lab := labtest.New(t)
	ssh := byComment("lab: allow ssh")
	patch(t, lab.A, filter, only(t, lab.A, filter, ssh)[".id"], map[string]string{"dst-port": "2222"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: allow ssh [both: dst-port]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	v := s.plan()
	if ops := shownOps(t, v); len(ops) != 2 || len(ops[1].Body) != 1 || ops[1].Body["dst-port"] != "2222" {
		t.Fatalf("want the backup and a PATCH of dst-port, got %v:\n%s", ops, v)
	}

	// Someone changes the same rule on b meanwhile.
	patch(t, lab.B, filter, only(t, lab.B, filter, ssh)[".id"], map[string]string{"protocol": "udp"})
	before := deviceState(t, lab, lab.B, syncedSections(lab))

	s.keys("y")
	v = s.until("the recheck", time.Minute, func(v string) bool {
		return finished.MatchString(v) || strings.Contains(v, "router state changed since the plan was shown")
	})
	if !strings.Contains(v, "router state changed since the plan was shown — nothing was written; review the updated plan") {
		t.Fatalf("the changed plan was not refused:\n%s", v)
	}
	if after := deviceState(t, lab, lab.B, syncedSections(lab)); after != before {
		t.Fatalf("the refused plan wrote to b:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := preApplyBackups(t, lab.B); len(got) != 0 {
		t.Errorf("the refused plan still took a backup on b: %v", got)
	}
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[1].Body["dst-port"] != "2222" || ops[1].Body["protocol"] != "tcp" {
		t.Fatalf("the updated plan shown should now set dst-port and protocol, got %v:\n%s", ops, v)
	}

	// Confirming the updated plan applies it.
	requireApplied(t, s.confirm(false))
	requireClean(t, lab, filter)
}

// Stop on first failure: a plan whose fifth op b rejects. The ops before it
// persist, the ones after it (in that section and the next) are never
// sent, the error is shown, and the post-apply drift reports what is left.
func TestLabApplyStopsOnFirstFailure(t *testing.T) {
	lab := labtest.New(t)
	const nat = "ip/firewall/nat"

	// a has an interface b lacks; a rule naming it is rejected on b
	// ("input does not match any value of interface").
	add(t, lab.A, "interface/bridge", map[string]string{"name": "lab-only-a"})

	ssh := byComment("lab: allow ssh")
	remove(t, lab.A, filter, only(t, lab.A, filter, ssh)[".id"])                                                                     // op 2: DELETE
	patch(t, lab.A, filter, only(t, lab.A, filter, byComment("lab: drop alt telnet"))[".id"], map[string]string{"dst-port": "2324"}) // op 3: PATCH
	for _, r := range []map[string]string{
		{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "7001", "comment": "lab: apply first"}, // op 4
		{"chain": "forward", "action": "drop", "in-interface": "lab-only-a", "comment": "lab: apply rejected"},       // op 5, rejected
		{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "7003", "comment": "lab: apply after"}, // op 6
	} {
		add(t, lab.A, filter, r)
	}
	add(t, lab.A, nat, map[string]string{"chain": "dstnat", "action": "dst-nat", "protocol": "tcp", "dst-port": "8081",
		"to-addresses": "192.168.88.11", "to-ports": "80", "comment": "lab: apply nat"}) // op 7
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		"lab: allow ssh [b]", "lab: apply after [a]", "lab: apply first [a]", "lab: apply rejected [a]", "lab: drop alt telnet [both: dst-port]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	s.selectSection(nat, plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)
	var shape []string
	for _, op := range ops {
		shape = append(shape, op.Method+" "+op.Body["comment"])
	}
	want := []string{"POST ", "DELETE ", "PATCH ", "PUT lab: apply first", "PUT lab: apply rejected", "PUT lab: apply after", "PUT lab: apply nat"}
	if strings.Join(shape, "|") != strings.Join(want, "|") {
		t.Fatalf("plan shape %q, want %q:\n%s", shape, want, v)
	}
	// The planner saw it coming and says so, but still runs the op.
	if !regexp.MustCompile(`(?s)Warnings.*lab: apply rejected.*lab-only-a`).MatchString(v) {
		t.Errorf("the dry run does not warn that b has no interface lab-only-a:\n%s", v)
	}

	v = s.confirm(false)
	t.Logf("result:\n%s", resultLines(v))
	if !strings.Contains(v, "apply stopped after 4/7 op(s)") {
		t.Fatalf("want the run stopped after 4 of 7 ops:\n%s", v)
	}
	if !strings.Contains(v, "input does not match any value of interface") {
		t.Errorf("the device's error is not shown:\n%s", v)
	}

	// Over REST: 1-4 landed, 5-7 did not.
	if pos := chainPos(t, lab.B, filter, "input", ssh); pos != -1 {
		t.Errorf("op 2 (DELETE \"lab: allow ssh\") did not persist on b")
	}
	if got := only(t, lab.B, filter, byComment("lab: drop alt telnet"))["dst-port"]; got != "2324" {
		t.Errorf("op 3 (PATCH dst-port) did not persist on b: dst-port %q", got)
	}
	requireSamePosition(t, lab, filter, "forward", byComment("lab: apply first"))
	for _, c := range []string{"lab: apply rejected", "lab: apply after"} {
		if pos := chainPos(t, lab.B, filter, "forward", byComment(c)); pos != -1 {
			t.Errorf("%q is on b at position %d: an op after the failure ran", c, pos)
		}
	}
	if pos := chainPos(t, lab.B, nat, "dstnat", byComment("lab: apply nat")); pos != -1 {
		t.Errorf("the nat create, in a later section, ran after the failure")
	}
	if got := preApplyBackups(t, lab.B); len(got) != 1 {
		t.Errorf("op 1 (the backup) should have left one file on b, got %v", got)
	}

	// The residuals, as the screen reports them and as a fresh diff does.
	for _, re := range []string{
		`ip/firewall/filter\s+2 residual lab: apply (rejected|after), lab: apply (rejected|after)`,
		`ip/firewall/nat\s+1 residual lab: apply nat`,
		`residual differences remain`,
	} {
		if !regexp.MustCompile(re).MatchString(v) {
			t.Errorf("the result does not report %q:\n%s", re, resultLines(v))
		}
	}
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: apply after [a]", "lab: apply rejected [a]")
	requireHunks(t, compare(t, lab, nat, lab.Pair.Sync.Exempt), "lab: apply nat [a]")
}

// The pre-apply backup: every target's writes start with
// /system/backup/save, and the file really exists on that router, under
// the name the plan shows. A plan writing both ways backs up both.
func TestLabApplyPreApplyBackup(t *testing.T) {
	lab := labtest.New(t)
	// Different chains: two rules each only on one router, appended to the
	// same chain, have no order relative to each other to preserve.
	add(t, lab.A, filter, map[string]string{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "7001", "comment": "lab: only on a"})
	add(t, lab.B, filter, map[string]string{"chain": "input", "action": "drop", "protocol": "tcp", "dst-port": "7002", "comment": "lab: only on b"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: only on a [a]", "lab: only on b [b]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectHunk(filter, hunkIndex(t, lab, filter, "lab: only on a"), plan.AtoB)
	s.selectHunk(filter, hunkIndex(t, lab, filter, "lab: only on b"), plan.BtoA)
	v := s.plan()
	ops := shownOps(t, v)
	nameA, nameB := backupOf(t, ops, "a"), backupOf(t, ops, "b")
	if !strings.HasPrefix(nameA, plan.DefaultBackupName) {
		t.Errorf("backup name %q", nameA)
	}
	requireApplied(t, s.confirm(true)) // a is master

	ctx := context.Background()
	for _, c := range []struct {
		key, name string
		client    *routeros.Client
	}{{"a", nameA, lab.A}, {"b", nameB, lab.B}} {
		var files []map[string]string
		eventually(t, "the backup file on router "+c.key, 30*time.Second, func() error {
			if err := c.client.Get(ctx, "/file?name="+c.name+".backup", &files); err != nil {
				return err
			}
			if len(files) != 1 {
				return fmt.Errorf("no %s.backup; pre-apply files: %v", c.name, preApplyBackups(t, c.client))
			}
			return nil
		})
		f := files[0]
		t.Logf("router %s: %s (type %s, size %s)", c.key, f["name"], f["type"], f["size"])
		if f["type"] != "backup" {
			t.Errorf("router %s: %s is a %q, not a backup", c.key, f["name"], f["type"])
		}
		if got := preApplyBackups(t, c.client); len(got) != 1 {
			t.Errorf("router %s: want exactly the one pre-apply backup, got %v", c.key, got)
		}
	}
	requireClean(t, lab, filter)
}

// Idempotency: after an apply the pair is clean, and planning the very same
// selection again — every section it touched, from fresh reads — produces
// no operations at all.
func TestLabApplyIsIdempotent(t *testing.T) {
	lab := labtest.New(t)
	const addrList = "ip/firewall/address-list"
	patch(t, lab.A, filter, only(t, lab.A, filter, byComment("lab: allow ssh"))[".id"], map[string]string{"dst-port": "2222"})
	add(t, lab.A, filter, map[string]string{"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "7001", "comment": "lab: idempotent"})
	add(t, lab.A, addrList, map[string]string{"list": "lab-apply", "address": "10.20.30.40"})

	choices := map[string]map[plan.HunkRef]plan.Direction{}
	for _, sec := range []string{filter, addrList} {
		choices[sec] = map[plan.HunkRef]plan.Direction{}
		for _, h := range compare(t, lab, sec, lab.Pair.Sync.Exempt).Hunks {
			choices[sec][plan.RefOf(h)] = plan.AtoB
		}
	}

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	s.selectSection(addrList, plan.AtoB)
	s.plan()
	requireApplied(t, s.confirm(false))

	// The whole pair is drift-free now...
	for _, sec := range syncedSections(lab) {
		requireClean(t, lab, sec)
	}
	// ...the same selection plans nothing...
	for sec, c := range choices {
		p := buildPlan(t, lab, sec, c)
		if !p.Empty() {
			t.Errorf("%s: re-planning the applied selection gives ops:%s", sec, describePlan(p))
		}
		for _, sk := range p.Skipped {
			if !strings.Contains(sk.Reason, "no longer differs") {
				t.Errorf("%s: unexpected skip %s: %s", sec, sk.Where(), sk.Reason)
			}
		}
	}
	// ...and the Apply screen, re-planned, has nothing selected to write.
	v := s.drift()
	if !strings.Contains(v, "0 selected") {
		t.Errorf("the applied hunks are still selected:\n%s", v)
	}
	s.keys("4", "r")
	if v := s.view(); !strings.Contains(v, "no hunks selected") || len(shownOps(t, v)) != 0 {
		t.Errorf("re-planning after the apply should find nothing to write:\n%s", v)
	}
}

// One write at a time: while a sync is running, a Runtime deploy is
// refused; while a deploy is running, the Apply screen stays on it and no
// sync can start.
func TestLabApplyOneWriteAtATime(t *testing.T) {
	lab := labtest.New(t)
	addRuleOnA(t, lab, "lab: one at a time")

	s := startTUI(t, lab, tuiOptions{write: true})
	// Runtime status first, so d is not ignored as "still fetching" later.
	s.keys("3")
	s.until("runtime status", 45*time.Second, func(v string) bool {
		return strings.Contains(v, "Router A") && !strings.Contains(v, "working...")
	})

	s.drift()
	s.selectSection(filter, plan.AtoB)
	s.plan()
	s.keys("y")
	// The model only moves on when the driver lets it, so once it shows
	// the run in progress it stays there until the next wait.
	s.until("the sync running", 30*time.Second, func(v string) bool { return strings.Contains(v, "applying ") })
	s.keys("3", "d")
	if v := s.view(); !strings.Contains(v, "a write is already running on the Apply screen (4)") {
		t.Fatalf("d during a running sync was not refused:\n%s", v)
	}
	s.keys("4")
	if v := s.view(); !strings.Contains(v, "— apply") || strings.Contains(v, "runtime deploy") {
		t.Fatalf("the Apply screen no longer shows the running sync:\n%s", v)
	}
	requireApplied(t, s.waitDone())
	requireRuntimeUntouched(t, lab)

	// The other way round: a second sync hunk is selected, then a deploy
	// is started.
	addRuleOnA(t, lab, "lab: after deploy")
	s.drift()
	s.selectSection(filter, plan.AtoB)
	s.keys("3", "d")
	v := s.until("the deploy's dry run", 45*time.Second, func(v string) bool {
		return strings.Contains(v, "runtime deploy") && strings.Contains(v, "Dry run:")
	})
	s.keys("y")
	if strings.Contains(s.view(), "press Y (shift+y)") {
		s.keys("Y")
	}
	s.until("the deploy running", 30*time.Second, func(v string) bool { return strings.Contains(v, "applying ") })
	s.keys("2", "4")
	v = s.view()
	if !strings.Contains(v, "runtime deploy") {
		t.Fatalf("entering the Apply screen during a deploy replaced it:\n%s", v)
	}
	s.keys("y", "Y")
	if v := s.view(); !strings.Contains(v, "runtime deploy") {
		t.Fatalf("y during a deploy started something else:\n%s", v)
	}
	t.Logf("deploy: %s", resultLines(s.waitDone()))
	if pos := chainPos(t, lab.B, filter, "forward", byComment("lab: after deploy")); pos != -1 {
		t.Errorf("the selected sync ran during the deploy: its rule is on b")
	}
}

// requireRuntimeUntouched asserts neither router carries anything a
// Runtime deploy creates (every such object is tagged "mtha:").
func requireRuntimeUntouched(t *testing.T, lab *labtest.Lab) {
	t.Helper()
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		for _, section := range []string{"tool/netwatch", "system/script", "system/scheduler", "interface/vrrp", "ip/address"} {
			var entries []map[string]string
			if err := c.Get(context.Background(), "/"+section, &entries); err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e["comment"], "mtha:") || strings.HasPrefix(e["name"], "mtha") {
					t.Errorf("%s has a runtime object: %v", section, e)
				}
			}
		}
	}
}
