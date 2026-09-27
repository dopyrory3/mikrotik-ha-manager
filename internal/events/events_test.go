package events

import (
	"errors"
	"sync"
	"testing"
	"time"

	"mtha/internal/routeros"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func TestMergeOrdersChronologicallyAcrossSources(t *testing.T) {
	a := FromLog(SourceA, []routeros.LogEvent{
		{LogEntry: routeros.LogEntry{Message: "a1"}, Kind: routeros.LogKindVRRP, At: at(1)},
		{LogEntry: routeros.LogEntry{Message: "a4"}, Kind: routeros.LogKindNetwatch, At: at(4)},
	})
	b := FromLog(SourceB, []routeros.LogEvent{
		{LogEntry: routeros.LogEntry{Message: "b2"}, Kind: routeros.LogKindVRRP, At: at(2)},
		{LogEntry: routeros.LogEntry{Message: "b5"}, Kind: routeros.LogKindMtha, At: at(5)},
	})
	tool := FromActions([]Action{{At: at(3), Kind: ActionSync, Summary: "t3"}})

	got := Merge(a, b, tool)

	want := []string{"a1", "b2", "t3", "a4", "b5"}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.Message != want[i] {
			t.Errorf("event %d = %q, want %q", i, e.Message, want[i])
		}
	}
	if got[0].Source != SourceA || got[0].Kind != "vrrp" {
		t.Errorf("event 0 source/kind = %s/%s, want a/vrrp", got[0].Source, got[0].Kind)
	}
	if got[2].Source != SourceTool || got[2].Kind != "sync" {
		t.Errorf("event 2 source/kind = %s/%s, want tool/sync", got[2].Source, got[2].Kind)
	}
}

// Same-second events keep their log order within a router, and across
// sources follow argument order (A, B, tool) — see Merge's doc comment.
func TestMergeIsStableForEqualTimestamps(t *testing.T) {
	a := []Event{{At: at(1), Message: "a-first"}, {At: at(1), Message: "a-second"}}
	b := []Event{{At: at(1), Message: "b"}}
	tool := []Event{{At: at(1), Message: "tool"}}

	got := Merge(a, b, tool)

	want := []string{"a-first", "a-second", "b", "tool"}
	for i, e := range got {
		if e.Message != want[i] {
			t.Errorf("event %d = %q, want %q", i, e.Message, want[i])
		}
	}
}

// A router whose clock was stepped mid-log can yield out-of-order entries;
// Merge must still produce a chronological timeline.
func TestMergeSortsOutOfOrderInput(t *testing.T) {
	got := Merge([]Event{{At: at(5), Message: "late"}, {At: at(1), Message: "early"}})
	if got[0].Message != "early" || got[1].Message != "late" {
		t.Errorf("got %q, %q; want early, late", got[0].Message, got[1].Message)
	}
}

func TestMergeEmpty(t *testing.T) {
	if got := Merge(nil, nil, nil); len(got) != 0 {
		t.Errorf("Merge of nothing = %+v, want empty", got)
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	a := []Event{{At: at(5), Message: "x"}, {At: at(1), Message: "y"}}
	Merge(a)
	if a[0].Message != "x" {
		t.Error("Merge reordered its input slice")
	}
}

func TestFromActionsFormatsTargetAndError(t *testing.T) {
	got := FromActions([]Action{
		{At: at(1), Kind: ActionSync, Target: "b", Summary: "apply ip/dns/static"},
		{At: at(2), Kind: ActionFailover, Summary: "fail over to B", Err: errors.New("pre-flight failed")},
	})

	if got[0].Message != "→ B: apply ip/dns/static" || got[0].Failed {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].Message != "fail over to B: pre-flight failed" || !got[1].Failed {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[1].Source != SourceTool || got[1].Kind != "failover" {
		t.Errorf("event 1 source/kind = %s/%s", got[1].Source, got[1].Kind)
	}
}

func TestJournalStampsZeroTimeAndReturnsCopy(t *testing.T) {
	j := NewJournal()
	j.now = func() time.Time { return at(42) }

	j.Record(Action{Kind: ActionSync, Summary: "stamped"})
	j.Record(Action{At: at(7), Kind: ActionSync, Summary: "explicit"})

	got := j.Actions()
	if len(got) != 2 {
		t.Fatalf("got %d actions, want 2", len(got))
	}
	if !got[0].At.Equal(at(42)) {
		t.Errorf("zero At stamped as %v, want %v", got[0].At, at(42))
	}
	if !got[1].At.Equal(at(7)) {
		t.Errorf("explicit At overwritten: %v", got[1].At)
	}

	got[0].Summary = "mutated"
	if j.Actions()[0].Summary != "stamped" {
		t.Error("Actions returned the journal's backing slice, not a copy")
	}
}

// Recorder is documented as safe from any goroutine; run under -race.
func TestJournalConcurrentRecord(t *testing.T) {
	var r Recorder = NewJournal()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Record(Action{Kind: ActionSync})
		}()
	}
	wg.Wait()
	if n := len(r.(*Journal).Actions()); n != 50 {
		t.Errorf("recorded %d actions, want 50", n)
	}
}
