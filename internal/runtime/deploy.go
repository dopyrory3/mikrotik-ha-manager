package runtime

import (
	"context"
	"fmt"
	"strings"

	"mtha/internal/model"
	"mtha/internal/routeros"
)

// State is one Op's verified status on one router.
type State int

const (
	StateMissing State = iota
	StateMismatched
	StateOK
	// StateConflict means a guarded field (on-master/on-backup) already
	// holds a non-empty, non-mtha value; deploy leaves it untouched.
	StateConflict
)

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
	Note  string // set when State came from an error rather than a real read
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

// Result is the outcome of Deploy or Remove across both routers.
type Result struct {
	Status Status
	Err    error // first error encountered; other ops still ran (partial results in Status)
}

func clientsByRouter(clientA, clientB *routeros.Client) map[string]*routeros.Client {
	return map[string]*routeros.Client{"a": clientA, "b": clientB}
}

// Verify reads both routers and reports how their current state compares to
// plans, without writing anything.
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

// Deploy makes every Op in plans true on both routers: creates what's
// missing, patches mismatched fields, leaves conflicting guarded fields
// alone. One Op failing doesn't stop the rest (mirrors ui.fetchDrift's
// partial-result philosophy) — check Result.Status for what actually landed.
func Deploy(ctx context.Context, clientA, clientB *routeros.Client, plans map[string]Plan) Result {
	clients := clientsByRouter(clientA, clientB)
	status := Status{}
	var firstErr error

	for _, router := range routers {
		client := clients[router]
		items := make([]ItemStatus, 0, len(plans[router].Ops))
		for _, op := range plans[router].Ops {
			state, err := ensure(ctx, client, op)
			note := ""
			if err != nil {
				note = err.Error()
				if firstErr == nil {
					firstErr = fmt.Errorf("deploy %s on router %s: %w", op.Label, router, err)
				}
			}
			items = append(items, ItemStatus{Label: op.Label, State: state, Note: note})
		}
		status[router] = items
	}

	return Result{Status: status, Err: firstErr}
}

// Remove deletes every mtha-tagged entry plans describes, on both routers —
// including VRRP interfaces and their addresses. The caller (the Runtime
// screen) is responsible for confirming this with the operator first,
// especially for a router currently holding VRRP master.
func Remove(ctx context.Context, clientA, clientB *routeros.Client, plans map[string]Plan) Result {
	clients := clientsByRouter(clientA, clientB)
	status := Status{}
	var firstErr error

	for _, router := range routers {
		client := clients[router]
		items := make([]ItemStatus, 0, len(plans[router].Ops))
		for _, op := range plans[router].Ops {
			state, err := remove(ctx, client, op)
			note := ""
			if err != nil {
				note = err.Error()
				if firstErr == nil {
					firstErr = fmt.Errorf("remove %s on router %s: %w", op.Label, router, err)
				}
			}
			items = append(items, ItemStatus{Label: op.Label, State: state, Note: note})
		}
		status[router] = items
	}

	return Result{Status: status, Err: firstErr}
}

// check is Verify's per-Op, per-router read: no writes.
func check(ctx context.Context, client *routeros.Client, op Op) (state State, note string, err error) {
	entries, err := client.GetSection(ctx, op.Section)
	if err != nil {
		return StateMissing, err.Error(), err
	}
	current, found := find(entries, op.MatchField, op.MatchValue)
	if !found {
		return StateMissing, "", nil
	}
	state, _ = classify(op, current)
	return state, "", nil
}

// ensure is Deploy's per-Op, per-router step: create if missing, patch any
// mismatched non-conflicting fields if present.
func ensure(ctx context.Context, client *routeros.Client, op Op) (State, error) {
	entries, err := client.GetSection(ctx, op.Section)
	if err != nil {
		return StateMissing, err
	}

	current, found := find(entries, op.MatchField, op.MatchValue)
	if !found {
		if err := client.Post(ctx, "/"+op.Section, op.Fields, nil); err != nil {
			return StateMissing, err
		}
		return StateOK, nil
	}

	state, conflicts := classify(op, current)
	if state == StateOK {
		return StateOK, nil
	}

	conflictSet := toSet(conflicts)
	patch := map[string]string{}
	for k, v := range op.Fields {
		if _, mutable := op.Mutable[k]; mutable || conflictSet[k] || stringField(current, k) == v {
			continue
		}
		patch[k] = v
	}
	if len(patch) > 0 {
		id := stringField(current, ".id")
		if err := client.Patch(ctx, "/"+op.Section+"/"+id, patch, nil); err != nil {
			return state, err
		}
	}

	if len(conflicts) > 0 {
		return StateConflict, nil
	}
	if !mutableMatch(op, current) {
		return StateMismatched, nil
	}
	return StateOK, nil
}

// remove is Remove's per-Op, per-router step.
func remove(ctx context.Context, client *routeros.Client, op Op) (State, error) {
	entries, err := client.GetSection(ctx, op.Section)
	if err != nil {
		return StateMissing, err
	}
	current, found := find(entries, op.MatchField, op.MatchValue)
	if !found {
		return StateMissing, nil
	}
	id := stringField(current, ".id")
	if err := client.Delete(ctx, "/"+op.Section+"/"+id); err != nil {
		return StateMismatched, err
	}
	return StateMissing, nil
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
		actual := stringField(current, field)
		ok := false
		for _, v := range accepted {
			if actual == v {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
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
