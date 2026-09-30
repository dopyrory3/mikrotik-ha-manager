// Package diff compares normalised config between two routers and produces
// per-section, hunk-level diffs (project.md §5.3). It reports presence and
// field-level differences only; which side is the source of truth for a
// given hunk (A→B or B→A) is chosen by the caller at apply time, not by the
// diff engine.
package diff

import "mtha/internal/model"

// Hunk is one identity-matched difference between router A and router B
// within a section.
type Hunk struct {
	Identity string
	// Occurrence is the 0-based position of the matched entry among the
	// entries sharing Identity on the same router. It is almost always 0
	// (comment dedupe makes identities unique for commented entries); it
	// matters only when a per-section fallback key collides, e.g. two
	// address-list entries with the same list and address. Identity plus
	// Occurrence is what the planner uses to find the exact entry to write.
	Occurrence int
	OnA        bool
	OnB        bool
	Changes    []model.FieldChange // only set when OnA && OnB
}

// RuleRef names one entry by identity and occurrence, the same pair a Hunk
// carries (and plan.HunkRef mirrors).
type RuleRef struct {
	Identity   string
	Occurrence int
}

// OrderHunk is order drift in one chain of a firewall section: rules present
// on both routers, in the same chain on both, whose relative order differs
// (docs/design-questions.md §2). RouterOS evaluates a chain in list order,
// so this is a policy difference that no identity-matched Hunk shows: every
// rule matches its counterpart, only the sequence differs.
//
// It is kept apart from Hunks rather than given a made-up identity, so it
// can never collide with a comment.
type OrderHunk struct {
	Chain string
	// Moved is a smallest set of rules that, moved, would put router B's
	// chain in router A's order (every other shared rule is already in
	// order relative to the rest), listed in router A's order. The same
	// number of rules has to move in the other direction.
	Moved []RuleRef
	// Rules is how many rules the chain has on both routers.
	Rules int
}

// SectionDiff is the full set of differences for one config section.
type SectionDiff struct {
	Section string
	Hunks   []Hunk
	// Order is per-chain order drift; only firewall sections have any.
	Order []OrderHunk
}

// Clean reports whether the section has no differences: no hunk, and no
// chain whose rules are in a different order.
func (d SectionDiff) Clean() bool {
	return len(d.Hunks) == 0 && len(d.Order) == 0
}

// Count is the number of findings: hunks plus chains with order drift.
func (d SectionDiff) Count() int {
	return len(d.Hunks) + len(d.Order)
}

// Compare normalises both routers' raw entries for a section and matches
// them by identity, returning one hunk per identity that differs (added,
// removed, or changed). Identities with no difference are omitted.
func Compare(section string, aRaw, bRaw []model.Entry, exempt []string) SectionDiff {
	aNorm := model.Normalize(section, aRaw, exempt)
	bNorm := model.Normalize(section, bRaw, exempt)

	aIDs := model.BuildIdentities(section, aNorm)
	bIDs := model.BuildIdentities(section, bNorm)

	aByID := groupByIdentity(aIDs, aNorm)
	bByID := groupByIdentity(bIDs, bNorm)

	var hunks []Hunk
	for _, id := range orderedIdentities(aIDs, bIDs) {
		aGroup := aByID[id]
		bGroup := bByID[id]

		// Two entries on the same router can legitimately share an
		// identity (e.g. a missing per-section key falling back to a
		// shared ordinal); pair them up by encounter order rather than
		// letting one silently overwrite the other, so every entry is
		// still diffed against something.
		n := len(aGroup)
		if len(bGroup) > n {
			n = len(bGroup)
		}
		for i := 0; i < n; i++ {
			switch {
			case i >= len(bGroup):
				hunks = append(hunks, Hunk{Identity: id, Occurrence: i, OnA: true, OnB: false})
			case i >= len(aGroup):
				hunks = append(hunks, Hunk{Identity: id, Occurrence: i, OnA: false, OnB: true})
			default:
				equal, changes := model.EntriesEqual(aGroup[i], bGroup[i])
				if !equal {
					hunks = append(hunks, Hunk{Identity: id, Occurrence: i, OnA: true, OnB: true, Changes: changes})
				}
			}
		}
	}

	sd := SectionDiff{Section: section, Hunks: hunks}
	if model.IsFirewallSection(section) {
		sd.Order = compareOrder(aIDs, aNorm, bIDs, bNorm)
	}
	return sd
}

// orderKey is one entry's position-independent key within a section: its
// identity plus occurrence, exactly what the planner resolves hunks by.
type orderKey struct {
	ref   RuleRef
	chain string
}

func orderKeys(ids []string, entries []model.Entry) []orderKey {
	keys := make([]orderKey, len(ids))
	seen := map[string]int{}
	for i, id := range ids {
		keys[i] = orderKey{ref: RuleRef{Identity: id, Occurrence: seen[id]}, chain: chainOf(entries[i])}
		seen[id]++
	}
	return keys
}

// compareOrder finds order drift per chain (docs/design-questions.md §2):
// over the rules on both routers and in the same chain on both, the rules
// outside a longest common ordering must move. Chains are compared on their
// own because only order within a chain matters; interleaving of chains in
// the list is cosmetic. Rules on one router only, or whose chain differs,
// are already hunks and are left out.
func compareOrder(aIDs []string, aNorm []model.Entry, bIDs []string, bNorm []model.Entry) []OrderHunk {
	aKeys := orderKeys(aIDs, aNorm)
	bKeys := orderKeys(bIDs, bNorm)

	// Position of each rule within its chain on router B.
	bPos := map[orderKey]int{}
	perChain := map[string]int{}
	for _, k := range bKeys {
		bPos[k] = perChain[k.chain]
		perChain[k.chain]++
	}

	// Router A's order, per chain, as B positions.
	var chains []string
	refs := map[string][]RuleRef{}
	positions := map[string][]int{}
	for _, k := range aKeys {
		p, ok := bPos[k]
		if !ok {
			continue
		}
		if _, ok := positions[k.chain]; !ok {
			chains = append(chains, k.chain)
		}
		refs[k.chain] = append(refs[k.chain], k.ref)
		positions[k.chain] = append(positions[k.chain], p)
	}

	var out []OrderHunk
	for _, chain := range chains {
		inOrder := longestIncreasing(positions[chain])
		var moved []RuleRef
		for i, ref := range refs[chain] {
			if !inOrder[i] {
				moved = append(moved, ref)
			}
		}
		if len(moved) > 0 {
			out = append(out, OrderHunk{Chain: chain, Moved: moved, Rules: len(refs[chain])})
		}
	}
	return out
}

// longestIncreasing marks the members of one longest strictly increasing
// subsequence of seq (patience sorting, O(n log n)). seq holds distinct
// values, so strictly increasing is the right comparison.
func longestIncreasing(seq []int) []bool {
	// tails[l] is the index in seq of the smallest tail of an increasing
	// subsequence of length l+1; prev links each index to its predecessor.
	var tails []int
	prev := make([]int, len(seq))
	for i, v := range seq {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if seq[tails[mid]] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[i] = -1
		if lo > 0 {
			prev[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	member := make([]bool, len(seq))
	if len(tails) == 0 {
		return member
	}
	for i := tails[len(tails)-1]; i >= 0; i = prev[i] {
		member[i] = true
	}
	return member
}

// chainOf is the rule's chain, "_" when it has none, as model's anchored
// identities use.
func chainOf(e model.Entry) string {
	if c, ok := e["chain"].(string); ok && c != "" {
		return c
	}
	return "_"
}

func groupByIdentity(ids []string, entries []model.Entry) map[string][]model.Entry {
	groups := make(map[string][]model.Entry, len(ids))
	for i, id := range ids {
		groups[id] = append(groups[id], entries[i])
	}
	return groups
}

// orderedIdentities returns the union of aIDs and bIDs, each id appearing
// once, in first-seen order (A's order, then any B-only ids in B's order)
// so hunk output stays deterministic.
func orderedIdentities(aIDs, bIDs []string) []string {
	seen := make(map[string]bool, len(aIDs)+len(bIDs))
	order := make([]string, 0, len(aIDs)+len(bIDs))
	for _, id := range aIDs {
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}
	for _, id := range bIDs {
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}
	return order
}
