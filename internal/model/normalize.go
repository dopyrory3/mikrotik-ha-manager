package model

import "strings"

// SectionExempt reports whether a whole section is exempt from drift/sync,
// i.e. exempt contains the bare section path (not a "<section>.<field>"
// entry).
func SectionExempt(section string, exempt []string) bool {
	for _, ex := range exempt {
		if ex == section {
			return true
		}
	}
	return false
}

// Normalize drops fields and entries that must never be compared between
// routers: the router-assigned ".id", dynamic entries, and any field listed
// as exempt for this section in the pair's sync.exempt list (project.md
// §5.1, §5.3). Field-level exemptions are written "<section>.<field>";
// which form applies to a given exempt entry is one of the spec's open
// questions (project.md §10), so both a bare section path and a dotted
// field path are accepted here.
func Normalize(section string, raw []Entry, exempt []string) []Entry {
	fieldExempt := exemptFieldsFor(section, exempt)

	out := make([]Entry, 0, len(raw))
	for _, e := range raw {
		if isDynamic(e) {
			continue
		}
		out = append(out, normalizeEntry(e, fieldExempt))
	}
	return out
}

func exemptFieldsFor(section string, exempt []string) map[string]bool {
	fields := map[string]bool{}
	for _, ex := range exempt {
		sec, field, ok := strings.Cut(ex, ".")
		if ok && sec == section {
			fields[field] = true
		}
	}
	return fields
}

func isDynamic(e Entry) bool {
	v, ok := stringField(e, "dynamic")
	return ok && v == "true"
}

func normalizeEntry(e Entry, fieldExempt map[string]bool) Entry {
	out := make(Entry, len(e))
	for k, v := range e {
		if k == ".id" || fieldExempt[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// FieldChange is one differing field between two matched entries.
type FieldChange struct {
	Field string
	A     string
	B     string
}

// defaultLikeValues are RouterOS field values indistinguishable from a
// field being absent (project.md §5.3: "treat absent and default-valued
// fields as equal"), since routers omit fields left at default rather than
// writing them explicitly.
var defaultLikeValues = map[string]bool{
	"":      true,
	"false": true,
	"no":    true,
	"none":  true,
	"0":     true,
}

// EntriesEqual compares two matched, already-normalized entries and returns
// the fields that differ.
func EntriesEqual(a, b Entry) (bool, []FieldChange) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}

	var changes []FieldChange
	for k := range keys {
		av, aok := a[k]
		bv, bok := b[k]
		as, bs := toComparable(av), toComparable(bv)

		switch {
		case aok && bok:
			if as != bs {
				changes = append(changes, FieldChange{Field: k, A: as, B: bs})
			}
		case aok && !bok:
			if !defaultLikeValues[as] {
				changes = append(changes, FieldChange{Field: k, A: as, B: ""})
			}
		case !aok && bok:
			if !defaultLikeValues[bs] {
				changes = append(changes, FieldChange{Field: k, A: "", B: bs})
			}
		}
	}

	return len(changes) == 0, changes
}
