//go:build lab

package labtest_test

import (
	"context"
	"testing"

	"mtha/internal/labtest"
	"mtha/internal/plan"
)

// Syncing a yes/no field back to its default. RouterOS 7.23.7 refuses both
// the unset command and "" for a firewall rule's disabled and log ("input
// does not match any value of value-name", "must be either yes or no",
// 400); PATCHing the default ("disabled": "false") is accepted, so that is
// what plan.updateOps sends to re-enable a rule or turn its logging off
// (issue #25).
func TestLabSyncFieldBackToDefault(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	requireClean(t, lab, filter)

	// Two rules on b move off their default, one field each.
	patch(t, lab.B, filter, only(t, lab.B, filter, byComment("lab: allow ssh"))[".id"], map[string]string{"disabled": "true"})
	patch(t, lab.B, filter, only(t, lab.B, filter, byComment("lab: trusted out"))[".id"], map[string]string{"log": "yes"})
	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	requireHunks(t, d, "lab: allow ssh [both: disabled]", "lab: trusted out [both: log]")

	choices := map[plan.HunkRef]plan.Direction{}
	for _, h := range d.Hunks {
		choices[plan.RefOf(h)] = plan.AtoB
	}
	p := buildPlan(t, lab, filter, choices)
	t.Logf("plan:%s", describePlan(p))
	// Every op, not stopping at the first failure as an apply does, so each
	// refused field is reported.
	for _, op := range p.Ops {
		if err := plan.Execute(ctx, lab.Client(op.Router), op); err != nil {
			t.Errorf("%s %s %v: %v", op.Method, op.Path, op.Body, err)
		}
	}
	requireClean(t, lab, filter)
}
