//go:build lab

package labtest_test

import (
	"context"
	"testing"

	"mtha/internal/labtest"
	"mtha/internal/plan"
)

// Syncing a field back to its default. plan.updateOps turns a field the
// source leaves at default into POST /<section>/unset, but RouterOS 7.23.7
// refuses unset for a firewall rule's disabled and log ("input does not
// match any value of value-name", 400), where PATCHing the default
// ("disabled": "false") is accepted. So re-enabling a rule, or turning its
// logging off, to match the other router fails mid-apply.
//
// Found while writing the identity tests (issue #12). It fails today, on
// purpose: it is a planner bug, not an identity one, and is left for a fix.
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
