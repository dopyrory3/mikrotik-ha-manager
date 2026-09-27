//go:build lab

package ui

import (
	"context"
	"testing"
	"time"

	"mtha/internal/labtest"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

const labFilter = "ip/firewall/filter"

// The end-to-end smoke test for the model-driven harness: real pollers and
// REST clients against the lab, the real Model driven by key presses, and
// the outcome asserted on the routers themselves.
//
// Router a gains a firewall rule router b lacks. The drift screen must find
// exactly that hunk (and nothing else: the lab pair, fixture included, is
// drift-free at baseline), and syncing it a→b from the Apply screen must take a
// pre-apply backup on b, add the rule there, and leave a untouched.
func TestLabSyncFirewallRuleAToB(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	// The baseline fixture already has filter rules on both routers.
	var baseline []map[string]string
	if err := lab.B.Get(ctx, "/"+labFilter, &baseline); err != nil {
		t.Fatal(err)
	}

	rule := map[string]string{
		"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53",
		"comment": "mtha-lab-allow-dns",
	}
	if err := lab.A.Post(ctx, "/"+labFilter, rule, nil); err != nil {
		t.Fatal(err)
	}

	d := labtest.Drive(t, New(lab.Pair, true, lab.Pollers(time.Second)))

	// Plan only once b is polled as backup, so a single y is the whole
	// confirmation (a master target would need Y as well).
	d.Until("both routers polled, b as VRRP backup", 30*time.Second, func(m Model) bool {
		b := m.snapshots["b"]
		return m.have("a") && m.have("b") && len(b.VRRP) == 1 && b.VRRP[0].Role() == routeros.RoleBackup
	})

	d.Send(labtest.Key("2"))
	m := d.Until("drift fetched", 30*time.Second, func(m Model) bool { return !m.driftFetching && m.driftData != nil })
	if m.driftErr != nil {
		t.Fatalf("drift fetch: %v", m.driftErr)
	}
	for section, sd := range m.driftData {
		if section != labFilter && !sd.Clean() {
			t.Errorf("%s drifts at baseline: %+v", section, sd.Hunks)
		}
	}
	hunks := m.driftData[labFilter].Hunks
	if len(hunks) != 1 || !hunks[0].OnA || hunks[0].OnB {
		t.Fatalf("%s hunks = %+v, want the one rule only on a", labFilter, hunks)
	}
	if m.driftSections[m.driftSection] != labFilter {
		t.Fatalf("drift cursor on %s, want %s first (see testlab/pairs.yaml)", m.driftSections[m.driftSection], labFilter)
	}

	d.Send(labtest.Key("enter"), labtest.Key(" "))
	if dir, ok := d.Model().driftSelected[labFilter][plan.HunkRef{Identity: hunks[0].Identity}]; !ok || dir != plan.AtoB {
		t.Fatalf("selection = %v, want the hunk a→b", d.Model().driftSelected)
	}

	d.Send(labtest.Key("4"))
	m = d.Until("dry run built", 30*time.Second, func(m Model) bool { return m.apply.stage == applyReview })
	if targets := m.apply.plan.Targets(); len(targets) != 1 || targets[0] != "b" {
		t.Fatalf("plan targets %v, want only b:\n%s", targets, m.apply.plan.Render())
	}

	d.Send(labtest.Key("y"))
	m = d.Until("apply finished and verified", time.Minute, func(m Model) bool { return m.apply.stage == applyDone })
	if m.apply.err != nil {
		t.Fatalf("apply: %v\n%s", m.apply.err, m.View())
	}
	if sd := m.apply.residual[labFilter]; !sd.Clean() {
		t.Errorf("post-apply verification still reports drift: %+v", sd.Hunks)
	}

	// Device state, over REST.
	var onB []map[string]string
	if err := lab.B.Get(ctx, "/"+labFilter, &onB); err != nil {
		t.Fatal(err)
	}
	if len(onB) != len(baseline)+1 {
		t.Fatalf("router b has %d filter rules, want its %d baseline rules plus the synced one: %v", len(onB), len(baseline), onB)
	}
	added := onB[len(onB)-1] // appended, as on a
	for k, want := range rule {
		if added[k] != want {
			t.Errorf("router b rule %s = %q, want %q", k, added[k], want)
		}
	}
	var backups []map[string]string
	if err := lab.B.Get(ctx, "/file?name="+m.apply.backupName+".backup", &backups); err != nil || len(backups) != 1 {
		t.Errorf("pre-apply backup %s.backup on router b: %v, %v", m.apply.backupName, backups, err)
	}
	var onA []map[string]string
	if err := lab.A.Get(ctx, "/file?name="+m.apply.backupName+".backup", &onA); err != nil || len(onA) != 0 {
		t.Errorf("router a (the source) was backed up, so it was a write target: %v, %v", onA, err)
	}
}
