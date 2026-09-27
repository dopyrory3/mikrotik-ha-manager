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

// SectionDiff is the full set of differences for one config section.
type SectionDiff struct {
	Section string
	Hunks   []Hunk
}

// Clean reports whether the section has no differences.
func (d SectionDiff) Clean() bool {
	return len(d.Hunks) == 0
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

	return SectionDiff{Section: section, Hunks: hunks}
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
