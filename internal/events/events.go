// Package events builds the Events screen's merged timeline (project.md
// §5.7): both routers' filtered logs plus the tool's own actions, in one
// chronological list. Everything here is held in memory for the session
// only; nothing is written to disk.
package events

import (
	"sort"
	"strings"
	"sync"
	"time"

	"mtha/internal/routeros"
)

// Source says where a timeline event came from.
type Source string

const (
	SourceA    Source = "a"
	SourceB    Source = "b"
	SourceTool Source = "tool"
)

// Event is one row of the merged timeline.
type Event struct {
	At     time.Time
	Source Source
	// Kind is the routeros.LogKind for router entries ("vrrp", "netwatch",
	// "mtha") and the ActionKind for tool actions ("sync", "failover", ...).
	Kind    string
	Message string
	// Failed marks a tool action that returned an error. Always false for
	// router log entries: RouterOS severity lives in their topics instead.
	Failed bool
}

// ActionKind classifies a tool action on the timeline.
type ActionKind string

const (
	// ActionSync is a sync/apply push (project.md §5.4, milestone 3).
	ActionSync ActionKind = "sync"
	// ActionFailover is a planned failover or failback (§5.6, milestone 5).
	ActionFailover ActionKind = "failover"
	// ActionRuntime is a Runtime screen deploy or remove (§5.5).
	ActionRuntime ActionKind = "runtime"
)

// Action is one of the tool's own write actions, as reported to the
// timeline by whichever screen performed it.
type Action struct {
	// At is when the action finished. Recorder implementations fill it in
	// with the current time when it is left zero.
	At   time.Time
	Kind ActionKind
	// Target is the router the action wrote to ("a", "b"), or empty when
	// it touched both or neither.
	Target string
	// Summary is a one-line, human-readable description, e.g.
	// "apply ip/firewall/filter A→B: 3 ops".
	Summary string
	// Err is the action's outcome; nil means it succeeded.
	Err error
}

// Recorder accepts the tool's own actions for the Events timeline
// (project.md §5.7: "append the tool's own sync/failover actions").
//
// This is the only hook later milestones need: Apply (milestone 3) and
// Failover (milestone 5) should call Record once per completed action,
// success or failure, with a Summary suitable for a single timeline row.
// Record must be safe to call from any goroutine, including from inside a
// tea.Cmd, and must not block on I/O.
type Recorder interface {
	Record(Action)
}

// Journal is the session's in-memory Recorder. The zero value is not ready
// for use; construct one with NewJournal.
type Journal struct {
	mu      sync.Mutex
	actions []Action
	now     func() time.Time
}

// NewJournal returns an empty Journal stamping actions with the wall clock.
func NewJournal() *Journal {
	return &Journal{now: time.Now}
}

// Record appends a, stamping At with the current time if it is zero.
func (j *Journal) Record(a Action) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a.At.IsZero() {
		a.At = j.now()
	}
	j.actions = append(j.actions, a)
}

// Actions returns a copy of every recorded action, in recording order.
func (j *Journal) Actions() []Action {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Action(nil), j.actions...)
}

// FromLog converts one router's filtered log into timeline events.
func FromLog(src Source, log []routeros.LogEvent) []Event {
	out := make([]Event, len(log))
	for i, e := range log {
		out[i] = Event{At: e.At, Source: src, Kind: string(e.Kind), Message: e.Message}
	}
	return out
}

// FromActions converts recorded tool actions into timeline events. A failed
// action's message carries its error so the timeline shows why.
func FromActions(actions []Action) []Event {
	out := make([]Event, len(actions))
	for i, a := range actions {
		msg := a.Summary
		if a.Target != "" {
			msg = "→ " + strings.ToUpper(a.Target) + ": " + msg
		}
		if a.Err != nil {
			msg += ": " + a.Err.Error()
		}
		out[i] = Event{At: a.At, Source: SourceTool, Kind: string(a.Kind), Message: msg, Failed: a.Err != nil}
	}
	return out
}

// Merge combines event streams into one list ordered oldest first. The sort
// is stable, so events sharing a timestamp keep their order within a stream
// (a router's log order is authoritative at 1s resolution) and across
// streams follow argument order — pass router A, router B, then tool actions.
func Merge(streams ...[]Event) []Event {
	n := 0
	for _, s := range streams {
		n += len(s)
	}
	out := make([]Event, 0, n)
	for _, s := range streams {
		out = append(out, s...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
