package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

// State is one Op's verified status on one router.
type State int

const (
	StateMissing State = iota
	StateMismatched
	StateOK
	// StateConflict means a guarded field (on-master/on-backup) already
	// holds a non-empty, non-mtha value, or an untagged entry already has
	// the Op's unique name; deploy and remove leave it untouched.
	StateConflict
)

// foreignNote explains a StateConflict caused by an untagged same-named
// entry.
const foreignNote = "exists, not managed by mtha"

func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateMismatched:
		return "mismatched"
	case StateConflict:
		return "conflict"
	default:
		return "missing"
	}
}

// ItemStatus is one Op's verified state on one router.
type ItemStatus struct {
	Label string
	State State
	Note  string // why, when State came from an error or an unmanaged entry
}

// Status is the verified state of every Op in a Plan, keyed by router.
type Status map[string][]ItemStatus

// Clean reports whether every item on every router is State OK.
func (s Status) Clean() bool {
	for _, router := range routers {
		for _, it := range s[router] {
			if it.State != StateOK {
				return false
			}
		}
	}
	return true
}

// FirstIssue describes the first non-OK item, for a readiness note. It
// returns "" when Status is clean (or empty).
func (s Status) FirstIssue() string {
	for _, router := range routers {
		for _, it := range s[router] {
			if it.State != StateOK {
				return fmt.Sprintf("router %s: %s %s", router, it.Label, it.State)
			}
		}
	}
	return ""
}

func clientsByRouter(clientA, clientB *routeros.Client) map[string]*routeros.Client {
	return map[string]*routeros.Client{"a": clientA, "b": clientB}
}

// Verify reads both routers and reports how their current state compares to
// plans, without writing anything. It is also the post-write verification
// of a Runtime deploy or remove.
func Verify(ctx context.Context, clientA, clientB *routeros.Client, plans map[string]Plan) (Status, error) {
	clients := clientsByRouter(clientA, clientB)
	status := Status{}
	var firstErr error

	for _, router := range routers {
		client := clients[router]
		items := make([]ItemStatus, 0, len(plans[router].Ops))
		for _, op := range plans[router].Ops {
			state, note, err := check(ctx, client, op)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("verify %s on router %s: %w", op.Label, router, err)
			}
			items = append(items, ItemStatus{Label: op.Label, State: state, Note: note})
		}
		status[router] = items
	}

	return status, firstErr
}

// Action is which of the Runtime screen's two write actions to plan.
type Action int

const (
	ActionDeploy Action = iota
	ActionRemove
)

func (a Action) String() string {
	if a == ActionRemove {
		return "remove"
	}
	return "deploy"
}

// Writes reads both routers fresh and plans action as REST operations, so
// Runtime writes go through the same dry run → confirm → recheck → execute
// → verify pipeline as config sync (project.md §7.3: "every write goes
// through plan and is shown before execution"). Each router the plan writes
// to starts with a pre-apply backup. Deploy creates what's missing and
// patches mismatched fields, in plan order (a VRRP interface before its
// addresses); remove deletes what's there, in reverse. Anything left alone
// on purpose — an untagged object with mtha's name, a hand-written
// on-master/on-backup script, a priority outside the accepted set — is
// listed in Plan.Skipped with the reason.
func Writes(ctx context.Context, clientA, clientB *routeros.Client, plans map[string]Plan, action Action, backupName string) (plan.Plan, error) {
	clients := clientsByRouter(clientA, clientB)
	reads := map[string]map[string][]model.Entry{}
	for _, router := range routers {
		reads[router] = map[string][]model.Entry{}
		for _, op := range plans[router].Ops {
			if _, ok := reads[router][op.Section]; ok {
				continue
			}
			entries, err := clients[router].GetSection(ctx, op.Section)
			if err != nil {
				return plan.Plan{}, fmt.Errorf("read %s on router %s: %w", op.Section, router, err)
			}
			reads[router][op.Section] = entries
		}
	}
	return buildWrites(plans, reads, action, backupName), nil
}

// buildWrites is Writes without the I/O: reads holds each router's current
// entries per section.
func buildWrites(plans map[string]Plan, reads map[string]map[string][]model.Entry, action Action, backupName string) plan.Plan {
	var p plan.Plan
	for _, router := range routers {
		ops := plans[router].Ops
		if action == ActionRemove {
			ops = make([]Op, len(plans[router].Ops))
			for i, op := range plans[router].Ops {
				ops[len(ops)-1-i] = op
			}
		}

		var writes []plan.Op
		for _, op := range ops {
			entries := reads[router][op.Section]
			var w []plan.Op
			var skips []string
			if action == ActionRemove {
				w, skips = removeWrites(router, op, entries)
			} else {
				w, skips = deployWrites(router, op, entries)
			}
			writes = append(writes, w...)
			for _, reason := range skips {
				p.Skipped = append(p.Skipped, plan.Skip{
					Section: op.Section, Ref: plan.HunkRef{Identity: op.Label}, Router: router, Reason: reason,
				})
			}
		}
		if len(writes) > 0 {
			p.Ops = append(p.Ops, plan.BackupOp(router, backupName))
			p.Ops = append(p.Ops, writes...)
		}
	}
	return p
}

// check is Verify's per-Op, per-router read: no writes.
func check(ctx context.Context, client *routeros.Client, op Op) (state State, note string, err error) {
	entries, err := client.GetSection(ctx, op.Section)
	if err != nil {
		return StateMissing, err.Error(), err
	}
	current, found, foreign := locate(entries, op)
	if foreign {
		return StateConflict, foreignNote, nil
	}
	if !found {
		return StateMissing, "", nil
	}
	state, _ = classify(op, current)
	return state, "", nil
}

// deployWrites is the create or patch that makes op true on router, given
// the router's current entries of op.Section, plus why anything is left
// alone: guarded and Mutable fields are never overwritten on an existing
// entry, and an untagged same-named entry is never touched.
func deployWrites(router string, op Op, entries []model.Entry) ([]plan.Op, []string) {
	current, found, foreign := locate(entries, op)
	if foreign {
		return nil, []string{foreignNote + "; left untouched"}
	}
	if !found {
		return []plan.Op{{
			Router: router, Method: plan.MethodCreate, Path: "/" + op.Section, Body: copyFields(op.Fields),
			Section: op.Section, Identity: op.Label, Note: "create " + op.Label,
		}}, nil
	}

	_, conflicts := classify(op, current)
	sort.Strings(conflicts)
	var skips []string
	for _, field := range conflicts {
		skips = append(skips, field+" holds a script mtha didn't write; left untouched")
	}
	for _, field := range sortedKeys(op.Mutable) {
		accepted := op.Mutable[field]
		if actual := stringField(current, field); !contains(accepted, actual) {
			skips = append(skips, fmt.Sprintf("%s is %s, not %s; deploy doesn't change it on an existing entry",
				field, actual, strings.Join(accepted, " or ")))
		}
	}

	conflictSet := toSet(conflicts)
	patch := map[string]string{}
	for k, v := range op.Fields {
		if _, mutable := op.Mutable[k]; mutable || conflictSet[k] || stringField(current, k) == v {
			continue
		}
		patch[k] = v
	}
	if len(patch) == 0 {
		return nil, skips
	}
	return []plan.Op{{
		Router: router, Method: plan.MethodUpdate, Path: "/" + op.Section + "/" + stringField(current, ".id"), Body: patch,
		Section: op.Section, Identity: op.Label, Note: "update " + op.Label,
	}}, skips
}

// removeWrites is the delete of op's entry on router, if it is there and
// is mtha's.
func removeWrites(router string, op Op, entries []model.Entry) ([]plan.Op, []string) {
	current, found, foreign := locate(entries, op)
	switch {
	case foreign:
		return nil, []string{foreignNote + "; not removed"}
	case !found:
		return nil, nil
	}
	return []plan.Op{{
		Router: router, Method: plan.MethodDelete, Path: "/" + op.Section + "/" + stringField(current, ".id"),
		Section: op.Section, Identity: op.Label, Note: "remove " + op.Label,
	}}, nil
}

// classify compares current against op.Fields, honoring op.Guarded: a
// guarded field whose current value is non-empty, isn't already op's own
// value, and doesn't start with the required marker is excluded from the
// comparison (and, in ensure, from patching) and reported as StateConflict
// instead of silently overwritten. A Mutable field is compared against its
// accepted values instead of the single Fields value.
func classify(op Op, current model.Entry) (State, []string) {
	fields := make(map[string]string, len(op.Fields))
	for k, v := range op.Fields {
		if _, mutable := op.Mutable[k]; !mutable {
			fields[k] = v
		}
	}

	var conflicts []string
	for field, marker := range op.Guarded {
		actual := stringField(current, field)
		if actual == fields[field] {
			continue
		}
		if actual != "" && !strings.HasPrefix(actual, marker) {
			conflicts = append(conflicts, field)
			delete(fields, field)
		}
	}

	if len(conflicts) > 0 {
		return StateConflict, conflicts
	}
	if !fieldsMatch(fields, current) || !mutableMatch(op, current) {
		return StateMismatched, nil
	}
	return StateOK, nil
}

// mutableMatch reports whether every Mutable field of op holds one of its
// accepted values on current.
func mutableMatch(op Op, current model.Entry) bool {
	for field, accepted := range op.Mutable {
		if !contains(accepted, stringField(current, field)) {
			return false
		}
	}
	return true
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func copyFields(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func stringField(e model.Entry, key string) string {
	v, ok := e[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// locate finds op's entry by its tag. When there is none, foreign reports
// whether an untagged entry already holds op's Unique value: that entry is
// someone else's, so callers must neither create over it nor patch or
// delete it.
func locate(entries []model.Entry, op Op) (current model.Entry, found, foreign bool) {
	if current, found = find(entries, op.MatchField, op.MatchValue); found {
		return current, true, false
	}
	if op.Unique != "" {
		_, foreign = find(entries, op.Unique, op.Fields[op.Unique])
	}
	return nil, false, foreign
}

func find(entries []model.Entry, field, value string) (model.Entry, bool) {
	for _, e := range entries {
		if stringField(e, field) == value {
			return e, true
		}
	}
	return nil, false
}

func fieldsMatch(fields map[string]string, actual model.Entry) bool {
	for k, v := range fields {
		if stringField(actual, k) != v {
			return false
		}
	}
	return true
}

func toSet(keys []string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}
