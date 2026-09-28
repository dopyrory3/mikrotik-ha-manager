package plan

import (
	"fmt"
	"strings"

	"mtha/internal/diff"
)

// Planning a chain's order finding (docs/design-questions.md §2). RouterOS
// reorders with one command, POST /<table>/move {"numbers": <.id>,
// "destination": <.id>}, which puts numbers immediately before destination
// and keeps the moved rule's ".id" (lab-rest-contract.md, write probe 4).
// Its sharp edge is the destination: an unknown ".id" is not an error, it
// silently moves the rule to the end of the whole table, as does leaving
// destination out. So every move planned here names a destination, and
// every destination is checked against the target's fresh read — and the
// whole sequence replayed on that read — before any op is emitted.

// move is one planned move: rule goes immediately before before.
type move struct {
	rule, before row
}

// moveOps plans the moves that put the target's copy of chain o.Chain into
// the source's order, or returns why it won't (and then plans nothing).
//
// The rules o.Moved leaves out are already in the same relative order on
// both routers (a longest common ordering), so they stay put and serve as
// anchors: walking the chain in source order, each moved rule goes before
// the next rule that stays. Moved rules sharing an anchor are moved in
// source order, so they line up before it in that order. Moved rules after
// the last rule that stays have no rule to go before: they go before that
// last rule, which then goes before the first of them. That moves one rule
// more than o.Moved names, but never leaves a move without a destination.
//
// Every rule is tracked by the ".id" read from the target for this plan,
// which RouterOS keeps across a move, so a later move can anchor on a rule
// an earlier one has already moved.
func moveOps(section string, o diff.OrderHunk, dir Direction, src, tgt side) ([]Op, string) {
	chain := o.Chain
	target := dir.Target()

	// The rules the finding compares — in the chain on both routers — as
	// the target's rows, in the source's order.
	var shared []row
	for _, r := range src.rows {
		if stringOf(r.norm["chain"]) != chain {
			continue
		}
		if t, _, ok := tgt.get(r.ref); ok && stringOf(t.norm["chain"]) == chain {
			shared = append(shared, t)
		}
	}
	inShared := map[HunkRef]bool{}
	for _, r := range shared {
		inShared[r.ref] = true
	}
	moving := map[HunkRef]bool{}
	for _, m := range o.Moved {
		ref := HunkRef{Identity: m.Identity, Occurrence: m.Occurrence}
		if !inShared[ref] {
			return nil, fmt.Sprintf("%s is not in chain %s on both routers", ref, chain)
		}
		moving[ref] = true
	}

	var moves []move
	var pending []row
	var last *row
	for i, r := range shared {
		if moving[r.ref] {
			pending = append(pending, r)
			continue
		}
		for _, p := range pending {
			moves = append(moves, move{rule: p, before: r})
		}
		pending = nil
		last = &shared[i]
	}
	if len(pending) > 0 {
		if last == nil {
			return nil, fmt.Sprintf("no rule in chain %s is in the same place on both routers to move the others around", chain)
		}
		for _, p := range pending {
			moves = append(moves, move{rule: p, before: *last})
		}
		moves = append(moves, move{rule: *last, before: pending[0]})
	}

	if reason := verifyMoves(tgt, target, chain, moves, shared); reason != "" {
		return nil, reason
	}

	ops := make([]Op, len(moves))
	for i, m := range moves {
		note := fmt.Sprintf("move %s before %s, to match chain %s's order on router %s", m.rule.ref, m.before.ref, chain, dir.Source())
		if i == 0 && len(moves) > 1 {
			note += fmt.Sprintf("; chain %s is in neither router's order until the last of these %d moves", chain, len(moves))
		}
		ops[i] = Op{
			Router: target, Method: MethodCommand, Path: "/" + section + "/move",
			Body:    map[string]string{"numbers": idOf(m.rule.raw), "destination": idOf(m.before.raw)},
			Section: section, Identity: m.rule.ref.Identity, Note: note,
		}
	}
	return ops, ""
}

// verifyMoves replays moves on the target's read of the table and returns
// why they are unsafe, or "": every ".id" must be one RouterOS reads as an
// ID and be on the target exactly once — above all the destination, since
// an unknown one would silently send the rule to the end of the table — and
// the replay must leave the chain's shared rules (want, in source order) in
// exactly that order.
func verifyMoves(tgt side, target, chain string, moves []move, want []row) string {
	order := make([]string, len(tgt.rows))
	for i, r := range tgt.rows {
		order[i] = idOf(r.raw)
	}
	steps := make([]idMove, len(moves))
	for i, m := range moves {
		steps[i] = idMove{rule: idOf(m.rule.raw), before: idOf(m.before.raw), ruleRef: m.rule.ref, beforeRef: m.before.ref}
	}
	got, err := replayMoves(order, steps)
	if err != nil {
		return fmt.Sprintf("%v on router %s", err, target)
	}

	pos := map[string]int{}
	for i, id := range got {
		pos[id] = i
	}
	for i := 1; i < len(want); i++ {
		if pos[idOf(want[i-1].raw)] > pos[idOf(want[i].raw)] {
			return fmt.Sprintf("the planned moves would leave %s after %s on router %s", want[i-1].ref, want[i].ref, target)
		}
	}
	return ""
}

// idMove is a move by ".id", with the rules' names for errors.
type idMove struct {
	rule, before       string
	ruleRef, beforeRef HunkRef
}

// replayMoves applies moves to order (a table's ".id"s, in table order) the
// way RouterOS's move command does, except that it refuses where RouterOS
// would guess: an ".id" that isn't on the table (RouterOS would move the
// rule to the end for an unknown destination), that is on it twice, or that
// isn't an ".id" at all (RouterOS reads a bare number as a position).
func replayMoves(order []string, moves []idMove) ([]string, error) {
	out := append([]string(nil), order...)
	for _, m := range moves {
		if err := checkID(out, m.rule, m.ruleRef, "the rule to move", ""); err != nil {
			return nil, err
		}
		if err := checkID(out, m.before, m.beforeRef, fmt.Sprintf("the rule to move %s before", m.ruleRef),
			"; RouterOS would silently move the rule to the end of the table"); err != nil {
			return nil, err
		}
		if m.rule == m.before {
			return nil, fmt.Errorf("%s would be moved before itself", m.ruleRef)
		}
		out = without(out, m.rule)
		at := indexOf(out, m.before)
		out = append(out[:at], append([]string{m.rule}, out[at:]...)...)
	}
	return out, nil
}

// checkID fails unless id is an ".id" found exactly once in order; missing
// is appended to the error when id isn't there at all.
func checkID(order []string, id string, ref HunkRef, what, missing string) error {
	if !isRuleID(id) {
		return fmt.Errorf("%s (%s) has no usable .id (%q)", what, ref, id)
	}
	n := 0
	for _, x := range order {
		if x == id {
			n++
		}
	}
	switch n {
	case 0:
		return fmt.Errorf("%s (%s, .id %s) is not there%s", what, ref, id, missing)
	case 1:
		return nil
	default:
		return fmt.Errorf("%s (%s, .id %s) is read %d times", what, ref, id, n)
	}
}

// isRuleID reports whether s is a RouterOS ".id" ("*" and hex digits), not
// a bare number (a position) or a comment, which move also accepts.
func isRuleID(s string) bool {
	if len(s) < 2 || s[0] != '*' {
		return false
	}
	return strings.Trim(strings.ToUpper(s[1:]), "0123456789ABCDEF") == ""
}

func without(ids []string, id string) []string {
	out := make([]string, 0, len(ids))
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func indexOf(ids []string, id string) int {
	for i, x := range ids {
		if x == id {
			return i
		}
	}
	return -1
}
