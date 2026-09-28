package plan

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"mtha/internal/diff"
	"mtha/internal/model"
)

func withID(e model.Entry, id string) model.Entry {
	out := model.Entry{".id": id}
	for k, v := range e {
		out[k] = v
	}
	return out
}

var (
	allowHost  = model.Entry{"chain": "forward", "action": "accept", "src-address": "192.0.2.10", "comment": "allow-host"}
	dropSubnet = model.Entry{"chain": "forward", "action": "drop", "src-address": "192.0.2.0/24", "comment": "drop-subnet"}
)

// A selected order finding plans RouterOS moves on the target, by ".id",
// each naming a destination read from the target (docs/design-questions.md
// §2). A moved rule with no rule after it to anchor on goes before the last
// rule in order, which then goes before it: never a move without a
// destination. Moves come before creates, and a finding resolved since it
// was selected is skipped.
func TestBuildPlansRuleOrder(t *testing.T) {
	a := []model.Entry{withID(allowHost, "*1"), withID(dropSubnet, "*2"), {".id": "*3", "chain": "input", "action": "drop", "comment": "drop-in"}}
	b := []model.Entry{withID(dropSubnet, "*A"), withID(allowHost, "*B")}

	cases := []struct {
		name    string
		b       []model.Entry
		choices map[HunkRef]Direction
		ops     []string // excluding the backup
		skip    string
	}{
		{
			name:    "A to B",
			b:       b,
			choices: map[HunkRef]Direction{{Chain: "forward"}: AtoB},
			ops:     []string{`POST /ip/firewall/filter/move {"destination":"*A","numbers":"*B"}`},
		},
		{
			name:    "B to A, moved rule last",
			b:       b,
			choices: map[HunkRef]Direction{{Chain: "forward"}: BtoA},
			ops: []string{
				`POST /ip/firewall/filter/move {"destination":"*2","numbers":"*1"}`,
				`POST /ip/firewall/filter/move {"destination":"*1","numbers":"*2"}`,
			},
		},
		{
			name:    "beside a create",
			b:       b,
			choices: map[HunkRef]Direction{{Chain: "forward"}: AtoB, {Identity: "drop-in"}: AtoB},
			ops: []string{
				`POST /ip/firewall/filter/move {"destination":"*A","numbers":"*B"}`,
				`PUT /ip/firewall/filter {"action":"drop","chain":"input","comment":"drop-in"}`,
			},
		},
		{
			name:    "since resolved",
			b:       []model.Entry{withID(allowHost, "*B"), withID(dropSubnet, "*A")},
			choices: map[HunkRef]Direction{{Chain: "forward"}: BtoA},
			skip:    "no longer differs (resolved, or changed since it was selected)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Build([]SectionInput{{Section: "ip/firewall/filter", A: a, B: tc.b, Choices: tc.choices}}, Options{})
			var got []string
			for _, op := range p.Ops {
				if op.Section != "" {
					got = append(got, op.String())
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.ops, "\n") {
				t.Errorf("ops:\n%s\nwant:\n%s\n%s", strings.Join(got, "\n"), strings.Join(tc.ops, "\n"), p.Render())
			}
			switch {
			case tc.skip == "" && len(p.Skipped) != 0:
				t.Errorf("Skipped = %+v, want none", p.Skipped)
			case tc.skip != "" && (len(p.Skipped) != 1 || p.Skipped[0].Reason != tc.skip):
				t.Errorf("Skipped = %+v, want %q", p.Skipped, tc.skip)
			}
		})
	}
}

// The dry run of a reorder: uncommented rules travel with the commented
// rule they follow, other chains are left where they are, and a create
// lands after the moves.
func TestBuildRendersRuleOrder(t *testing.T) {
	a := []model.Entry{
		{".id": "*1", "chain": "forward", "action": "accept", "connection-state": "established,related"},
		{".id": "*2", "chain": "forward", "action": "accept", "src-address": "192.0.2.10", "comment": "allow-host"},
		{".id": "*3", "chain": "forward", "action": "log", "src-address": "192.0.2.10"},
		{".id": "*4", "chain": "input", "action": "accept", "comment": "allow-ssh"},
		{".id": "*5", "chain": "forward", "action": "drop", "src-address": "192.0.2.0/24", "comment": "drop-subnet"},
		{".id": "*6", "chain": "forward", "action": "drop", "comment": "drop-rest"},
		{".id": "*7", "chain": "forward", "action": "accept", "comment": "new-rule"},
		{".id": "*8", "chain": "forward", "action": "drop", "comment": "last"},
	}
	b := []model.Entry{
		{".id": "*A", "chain": "forward", "action": "accept", "connection-state": "established,related"},
		{".id": "*B", "chain": "forward", "action": "drop", "src-address": "192.0.2.0/24", "comment": "drop-subnet"},
		{".id": "*C", "chain": "input", "action": "accept", "comment": "allow-ssh"},
		{".id": "*D", "chain": "forward", "action": "drop", "comment": "last"},
		{".id": "*E", "chain": "forward", "action": "accept", "src-address": "192.0.2.10", "comment": "allow-host"},
		{".id": "*F", "chain": "forward", "action": "log", "src-address": "192.0.2.10"},
		{".id": "*10", "chain": "forward", "action": "drop", "comment": "drop-rest"},
	}
	p := Build([]SectionInput{{Section: "ip/firewall/filter", A: a, B: b,
		Choices: map[HunkRef]Direction{{Chain: "forward"}: AtoB, {Identity: "new-rule"}: AtoB}}}, Options{BackupName: "bk"})
	assertGolden(t, "firewall_move", p.Render())
	requireReordered(t, "ip/firewall/filter", a, b, p, "b")
}

// Random permutations of commented blocks (a commented rule and its
// uncommented followers), with another chain interleaved and a rule on
// the target only: in either direction, the planned moves, replayed on the
// target, leave no order finding.
func TestBuildRuleOrderRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for round := 0; round < 300; round++ {
		n := 2 + rng.Intn(7)
		var blocks [][]model.Entry
		for i := 0; i < n; i++ {
			block := []model.Entry{{"chain": "forward", "action": "accept", "comment": fmt.Sprintf("rule-%d", i)}}
			for j := 0; j < rng.Intn(3); j++ {
				block = append(block, model.Entry{"chain": "forward", "action": "log", "log-prefix": fmt.Sprintf("r%d-%d", i, j)})
			}
			blocks = append(blocks, block)
		}
		input := []model.Entry{
			{"chain": "input", "action": "accept", "comment": "in-1"},
			{"chain": "input", "action": "drop", "comment": "in-2"},
		}
		// lay puts the blocks in order; -1 is a block on this router only.
		lay := func(order []int, prefix string) []model.Entry {
			var fwd []model.Entry
			for _, i := range order {
				if i < 0 {
					fwd = append(fwd, model.Entry{"chain": "forward", "action": "drop", "comment": "target-only"})
					continue
				}
				fwd = append(fwd, blocks[i]...)
			}
			var out []model.Entry
			in := 0
			for _, e := range fwd {
				if in < len(input) && rng.Intn(3) == 0 {
					out = append(out, input[in])
					in++
				}
				out = append(out, e)
			}
			out = append(out, input[in:]...)
			for i := range out {
				out[i] = withID(out[i], fmt.Sprintf("*%s%X", prefix, i+1))
			}
			return out
		}
		identity := make([]int, n)
		for i := range identity {
			identity[i] = i
		}
		a := lay(identity, "")
		bOrder := rng.Perm(n)
		if rng.Intn(2) == 0 {
			at := rng.Intn(n + 1)
			bOrder = append(bOrder[:at], append([]int{-1}, bOrder[at:]...)...)
		}
		b := lay(bOrder, "F")

		dir := Direction(rng.Intn(2))
		sd := diff.Compare("ip/firewall/filter", a, b, nil)
		if len(sd.Hunks) > 1 {
			t.Fatalf("round %d: hunks %+v", round, sd.Hunks)
		}
		if len(sd.Order) == 0 {
			continue
		}
		p := Build([]SectionInput{{Section: "ip/firewall/filter", A: a, B: b, Choices: map[HunkRef]Direction{OrderRef(sd.Order[0]): dir}}}, Options{})
		if len(p.Skipped) != 0 {
			t.Fatalf("round %d %s: skipped %+v", round, dir, p.Skipped)
		}
		requireReordered(t, "ip/firewall/filter", a, b, p, dir.Target())
	}
}

// requireReordered replays p's moves on the target's entries, exactly as
// RouterOS would, and fails unless the result has no order finding against
// the source and every move named a destination on the target.
func requireReordered(t *testing.T, section string, a, b []model.Entry, p Plan, target string) {
	t.Helper()
	tgt := b
	if target == "a" {
		tgt = a
	}
	byID := map[string]model.Entry{}
	order := make([]string, len(tgt))
	for i, e := range tgt {
		order[i] = stringOf(e[".id"])
		byID[order[i]] = e
	}
	for _, op := range p.Ops {
		if !strings.HasSuffix(op.Path, "/move") {
			continue
		}
		if op.Router != target {
			t.Fatalf("move on router %s, want %s: %s", op.Router, target, op)
		}
		if _, ok := byID[op.Body["destination"]]; !ok {
			t.Fatalf("move to a destination not on the target: %s\n%s", op, p.Render())
		}
		order = without(order, op.Body["numbers"])
		at := indexOf(order, op.Body["destination"])
		order = append(order[:at], append([]string{op.Body["numbers"]}, order[at:]...)...)
	}
	after := make([]model.Entry, len(order))
	for i, id := range order {
		after[i] = byID[id]
	}
	var sd diff.SectionDiff
	if target == "a" {
		sd = diff.Compare(section, after, b, nil)
	} else {
		sd = diff.Compare(section, a, after, nil)
	}
	if len(sd.Order) != 0 {
		t.Fatalf("after the moves, order still differs: %+v\n%s", sd.Order, p.Render())
	}
}

// The stale-anchor hazard: RouterOS moves a rule to the end of the table,
// silently, when its destination is unknown. A chain whose anchor can't be
// verified on the target is skipped whole, with no move planned.
func TestBuildRefusesUnverifiableMoveAnchor(t *testing.T) {
	a := []model.Entry{withID(allowHost, "*1"), withID(dropSubnet, "*2")}
	for _, anchorID := range []string{"", "0", "3", "lab: drop"} {
		t.Run(fmt.Sprintf("anchor .id %q", anchorID), func(t *testing.T) {
			b := []model.Entry{withID(dropSubnet, anchorID), withID(allowHost, "*B")}
			p := Build([]SectionInput{{Section: "ip/firewall/filter", A: a, B: b, Choices: map[HunkRef]Direction{{Chain: "forward"}: AtoB}}}, Options{})
			if !p.Empty() {
				t.Fatalf("planned writes with an unusable anchor:\n%s", p.Render())
			}
			want := fmt.Sprintf("1 of 2 rule(s) out of order (allow-host); not moved: the rule to move allow-host before (drop-subnet) has no usable .id (%q) on router b. Reorder this chain by hand", anchorID)
			if len(p.Skipped) != 1 || p.Skipped[0].Ref.Chain != "forward" || p.Skipped[0].Reason != want {
				t.Fatalf("Skipped = %+v\nwant one forward skip: %q", p.Skipped, want)
			}
		})
	}
}

// The check behind it: a destination not on the target's table is refused,
// never replayed as RouterOS would (an append).
func TestReplayMovesRefusesUnknownDestination(t *testing.T) {
	order := []string{"*1", "*2", "*3"}
	ok := idMove{rule: "*3", before: "*1", ruleRef: HunkRef{Identity: "c"}, beforeRef: HunkRef{Identity: "a"}}
	if got, err := replayMoves(order, []idMove{ok}); err != nil || strings.Join(got, ",") != "*3,*1,*2" {
		t.Fatalf("replay = %v, %v", got, err)
	}

	stale := idMove{rule: "*1", before: "*9", ruleRef: HunkRef{Identity: "a"}, beforeRef: HunkRef{Identity: "gone"}}
	_, err := replayMoves(order, []idMove{ok, stale})
	if err == nil || !strings.Contains(err.Error(), "(gone, .id *9) is not there; RouterOS would silently move the rule to the end of the table") {
		t.Fatalf("err = %v, want the unknown destination refused", err)
	}

	twice := idMove{rule: "*1", before: "*2", ruleRef: HunkRef{Identity: "a"}, beforeRef: HunkRef{Identity: "b"}}
	if _, err := replayMoves([]string{"*1", "*2", "*2"}, []idMove{twice}); err == nil || !strings.Contains(err.Error(), "read 2 times") {
		t.Fatalf("err = %v, want a duplicated .id refused", err)
	}

	self := idMove{rule: "*1", before: "*1", ruleRef: HunkRef{Identity: "a"}, beforeRef: HunkRef{Identity: "a"}}
	if _, err := replayMoves(order, []idMove{self}); err == nil {
		t.Fatal("a move before itself was replayed")
	}
}

// Execute won't send a move RouterOS could read as "to the end" or as a
// position, whatever built it.
func TestExecuteRefusesMoveWithoutIDs(t *testing.T) {
	for _, body := range []map[string]string{
		{"numbers": "*1"},
		{"numbers": "*1", "destination": ""},
		{"numbers": "*1", "destination": "0"},
		{"numbers": "lab: allow ssh", "destination": "*2"},
	} {
		w := &fakeWriter{}
		err := Execute(context.Background(), w, Op{Router: "b", Method: MethodCommand, Path: "/ip/firewall/filter/move", Body: body})
		if err == nil || len(w.calls) != 0 {
			t.Errorf("%v: err = %v, calls = %+v; want refused before the router", body, err, w.calls)
		}
	}
	w := &fakeWriter{}
	if err := Execute(context.Background(), w, Op{Router: "b", Method: MethodCommand, Path: "/ip/firewall/filter/move", Body: map[string]string{"numbers": "*1A", "destination": "*2"}}); err != nil || len(w.calls) != 1 {
		t.Errorf("a valid move: err = %v, calls = %+v", err, w.calls)
	}
}

func TestIsRuleID(t *testing.T) {
	for s, want := range map[string]bool{"*1": true, "*1A": true, "*ff": true, "*": false, "": false, "0": false, "1A": false, "*G": false, "*1,*2": false} {
		if got := isRuleID(s); got != want {
			t.Errorf("isRuleID(%q) = %v, want %v", s, got, want)
		}
	}
}
