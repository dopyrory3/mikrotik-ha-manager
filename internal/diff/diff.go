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
	OnA      bool
	OnB      bool
	Changes  []model.FieldChange // only set when OnA && OnB
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

	aByID := make(map[string]model.Entry, len(aNorm))
	for i, id := range aIDs {
		aByID[id] = aNorm[i]
	}
	bByID := make(map[string]model.Entry, len(bNorm))
	for i, id := range bIDs {
		bByID[id] = bNorm[i]
	}

	seen := make(map[string]bool, len(aIDs)+len(bIDs))
	var hunks []Hunk

	for _, id := range aIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		be, onB := bByID[id]
		if !onB {
			hunks = append(hunks, Hunk{Identity: id, OnA: true, OnB: false})
			continue
		}

		equal, changes := model.EntriesEqual(aByID[id], be)
		if equal {
			continue
		}
		hunks = append(hunks, Hunk{Identity: id, OnA: true, OnB: true, Changes: changes})
	}

	for _, id := range bIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		hunks = append(hunks, Hunk{Identity: id, OnA: false, OnB: true})
	}

	return SectionDiff{Section: section, Hunks: hunks}
}
