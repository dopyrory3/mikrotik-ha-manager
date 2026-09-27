package model

import (
	"sort"
	"strings"
)

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

// TagPrefix is the comment prefix marking an object as mtha-managed or
// mtha-selected (project.md §5.5, §7.3).
const TagPrefix = "mtha:"

// Select returns the raw entries of a section that take part in drift and
// sync at all, in their original order and still carrying ".id":
//
//   - dynamic entries are dropped (router-generated, not config);
//   - ip/route is opt-in (project.md §10): only routes whose comment starts
//     with TagPrefix are selected, so per-router and untagged routes never
//     enter the diff or the plan.
//
// Normalize is Select followed by field stripping, and the two always agree
// on which entries survive and in what order — the planner relies on that to
// map a normalised entry (and its identity) back to the router's ".id".
func Select(section string, raw []Entry) []Entry {
	out := make([]Entry, 0, len(raw))
	for _, e := range raw {
		if isDynamic(e) {
			continue
		}
		if section == "ip/route" && !isTagged(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func isTagged(e Entry) bool {
	c, _ := stringField(e, "comment")
	return strings.HasPrefix(c, TagPrefix)
}

// Normalize drops entries and fields that must never be compared between
// routers: entries Select excludes, the router-assigned ".id", runtime state
// fields (see stateFields), and any field listed as exempt for this section
// in the pair's sync.exempt list (project.md §5.1, §5.3). Field-level
// exemptions are written "<section>.<field>"; a bare section path exempts
// the whole section (see SectionExempt) and is ignored here.
func Normalize(section string, raw []Entry, exempt []string) []Entry {
	drop := exemptFieldsFor(section, exempt)
	for _, f := range commonStateFields {
		drop[f] = true
	}
	for _, f := range sectionStateFields[section] {
		drop[f] = true
	}

	selected := Select(section, raw)
	out := make([]Entry, 0, len(selected))
	for _, e := range selected {
		out = append(out, normalizeEntry(e, drop))
	}
	return out
}

// commonStateFields are read-only fields RouterOS reports on many sections
// that describe runtime state rather than configuration. They are stripped
// before comparison (they'd otherwise show as permanent drift) and so never
// appear in a planned write body, where RouterOS would reject them.
var commonStateFields = []string{".id", ".nextid", ".about", "dynamic", "invalid", "running", "builtin"}

// sectionStateFields are the per-section read-only/runtime fields, in the
// same spirit as commonStateFields: counters, timestamps and derived flags.
// Extend this when a new synced section reports a read-only field — the
// symptom is drift that can never be resolved, or an apply that fails with
// a RouterOS "unknown parameter" error.
var sectionStateFields = map[string][]string{
	"ip/firewall/filter":       {"bytes", "packets"},
	"ip/firewall/nat":          {"bytes", "packets"},
	"ip/firewall/mangle":       {"bytes", "packets"},
	"ip/firewall/raw":          {"bytes", "packets"},
	"ip/firewall/address-list": {"creation-time"},
	"ip/dhcp-server/lease": {
		"status", "last-seen", "expires-after", "blocked", "radius", "host-name",
		"active-address", "active-mac-address", "active-client-id", "active-server",
	},
	"ip/route":         {"active", "inactive", "static", "connect", "ecmp", "hw-offloaded", "immediate-gw", "local-address"},
	"system/script":    {"owner", "last-started", "run-count"},
	"system/scheduler": {"owner", "next-run", "run-count"},
	"user":             {"last-logged-in", "expired"},
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

func normalizeEntry(e Entry, drop map[string]bool) Entry {
	out := make(Entry, len(e))
	for k, v := range e {
		if drop[k] {
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
// the fields that differ, sorted by field name so diff output (and the plans
// built from it) are deterministic.
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

	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	return len(changes) == 0, changes
}
