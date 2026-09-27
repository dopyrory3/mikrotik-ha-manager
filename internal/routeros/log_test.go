package routeros

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestClassifyLog(t *testing.T) {
	cases := []struct {
		name  string
		entry LogEntry
		want  LogKind
		keep  bool
	}{
		{"vrrp topic", LogEntry{Topics: "vrrp,info", Message: "vrrp-lan now MASTER"}, LogKindVRRP, true},
		{"netwatch topic", LogEntry{Topics: "netwatch,info", Message: "event down [ type: simple, host: 1.1.1.1 ]"}, LogKindNetwatch, true},
		{"mtha prefix under script topic", LogEntry{Topics: "script,info", Message: "mtha: vrrp-lan transitioned to master"}, LogKindMtha, true},
		{"mtha prefix wins over vrrp topic", LogEntry{Topics: "vrrp,info", Message: "mtha: note"}, LogKindMtha, true},
		{"leading whitespace before mtha prefix", LogEntry{Topics: "script,info", Message: "  mtha: x"}, LogKindMtha, true},
		{"topic with spaces", LogEntry{Topics: "system, vrrp", Message: "x"}, LogKindVRRP, true},
		{"unrelated topic", LogEntry{Topics: "system,info,account", Message: "user admin logged in"}, "", false},
		{"topic is a substring only", LogEntry{Topics: "vrrpx,netwatchy", Message: "x"}, "", false},
		{"mtha not a prefix", LogEntry{Topics: "script,info", Message: "hello mtha: world"}, "", false},
		{"mention of vrrp in message only", LogEntry{Topics: "interface,info", Message: "vrrp-lan link up"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ClassifyLog(tc.entry)
			if ok != tc.keep || got != tc.want {
				t.Errorf("ClassifyLog = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.keep)
			}
		})
	}
}

func TestParseLogTime(t *testing.T) {
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"13:59:58", time.Date(2026, 9, 27, 13, 59, 58, 0, time.UTC)},
		// Time-only after "now" can only be yesterday's (read across midnight).
		{"23:59:59", time.Date(2026, 9, 26, 23, 59, 59, 0, time.UTC)},
		{"sep/26 10:00:00", time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)},
		{"Sep/26 10:00:00", time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)},
		// Year-less date after "now" belongs to last year.
		{"dec/31 23:00:00", time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)},
		{"dec/31/2024 23:00:00", time.Date(2024, 12, 31, 23, 0, 0, 0, time.UTC)},
		{"09-26 10:00:00", time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)},
		{"2025-01-02 03:04:05", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := ParseLogTime(tc.raw, now)
		if err != nil {
			t.Errorf("ParseLogTime(%q): %v", tc.raw, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseLogTime(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}

	if _, err := ParseLogTime("yesterday-ish", now); err == nil {
		t.Error("expected an error for an unrecognised time")
	}
}

func TestFilterEventsKeepsMatchesInOrderOnToolClock(t *testing.T) {
	routerNow := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	// The tool's clock is in another zone and 90s behind the router's.
	fetchedAt := time.Date(2026, 9, 27, 15, 58, 30, 0, time.FixedZone("x", 2*3600))

	entries := []LogEntry{
		{ID: "*1", Time: "13:00:00", Topics: "system,info", Message: "boot"},
		{ID: "*2", Time: "13:30:00", Topics: "vrrp,info", Message: "vrrp-lan now BACKUP"},
		{ID: "*3", Time: "garbled", Topics: "netwatch,info", Message: "1.1.1.1 down"},
		{ID: "*4", Time: "13:59:00", Topics: "script,info", Message: "mtha: vrrp-lan transitioned to master"},
	}

	got := FilterEvents(entries, routerNow, fetchedAt)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}

	wantIDs := []string{"*2", "*3", "*4"}
	wantKinds := []LogKind{LogKindVRRP, LogKindNetwatch, LogKindMtha}
	// Router 13:30 is 30m before router-now, so 30m before fetchedAt.
	wantAt := []time.Time{
		fetchedAt.Add(-30 * time.Minute),
		fetchedAt.Add(-30 * time.Minute), // unparseable: inherits predecessor
		fetchedAt.Add(-1 * time.Minute),
	}
	for i, e := range got {
		if e.ID != wantIDs[i] || e.Kind != wantKinds[i] {
			t.Errorf("event %d = %s/%s, want %s/%s", i, e.ID, e.Kind, wantIDs[i], wantKinds[i])
		}
		if !e.At.Equal(wantAt[i]) {
			t.Errorf("event %d At = %v, want %v", i, e.At, wantAt[i])
		}
	}
}

func TestFilterEventsUnparseableFirstEntryHasZeroTime(t *testing.T) {
	got := FilterEvents([]LogEntry{{Time: "??", Topics: "vrrp"}}, time.Now(), time.Now())
	if len(got) != 1 || !got[0].At.IsZero() {
		t.Errorf("got %+v, want one event with zero At", got)
	}
}

func TestParseGMTOffset(t *testing.T) {
	cases := map[string]time.Duration{
		"+02:00": 2 * time.Hour,
		"-05:30": -(5*time.Hour + 30*time.Minute),
		"+0100":  time.Hour,
		"+00:00": 0,
	}
	for raw, want := range cases {
		got, ok := parseGMTOffset(raw)
		if !ok || got != want {
			t.Errorf("parseGMTOffset(%q) = %v, %v; want %v", raw, got, ok, want)
		}
	}
	if _, ok := parseGMTOffset(""); ok {
		t.Error("empty offset should not parse")
	}
}

func TestReadEventsFetchesLogThenClock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/log", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
			{".id":"*1","time":"sep/26 23:00:00","topics":"vrrp,info","message":"vrrp-lan now MASTER"},
			{".id":"*2","time":"10:00:00","topics":"dhcp,info","message":"lease"},
			{".id":"*3","time":"10:00:05","topics":"netwatch,info","message":"1.1.1.1 up"}
		]`)
	})
	mux.HandleFunc("/rest/system/clock", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"date":"2026-09-27","time":"10:00:10","gmt-offset":"+02:00"}`)
	})
	c, rec := newRestClient(t, mux)

	before := time.Now()
	log, err := c.ReadEvents(context.Background())
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}

	rec.mu.Lock()
	if len(rec.recs) != 2 || rec.recs[0].Path != "/rest/log" || rec.recs[1].Path != "/rest/system/clock" {
		t.Errorf("requests = %+v, want /rest/log then /rest/system/clock", rec.recs)
	}
	rec.mu.Unlock()

	if len(log.Events) != 2 || log.Events[0].ID != "*1" || log.Events[1].ID != "*3" {
		t.Fatalf("events = %+v, want *1 and *3", log.Events)
	}
	if d := log.FetchedAt.Sub(log.Events[1].At); d != 5*time.Second {
		t.Errorf("*3 is %v before FetchedAt, want 5s", d)
	}
	if d := log.Events[1].At.Sub(log.Events[0].At); d != 11*time.Hour+5*time.Second {
		t.Errorf("*1→*3 gap = %v, want 11h0m5s", d)
	}
	if !log.SkewKnown {
		t.Error("SkewKnown = false, want true for gmt-offset +02:00")
	}
	if log.FetchedAt.Before(before) {
		t.Errorf("FetchedAt %v predates the call at %v", log.FetchedAt, before)
	}
}

func TestReadEventsRejectsUnparseableClock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/log", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) })
	mux.HandleFunc("/rest/system/clock", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"date":"someday","time":"noon"}`)
	})
	c, _ := newRestClient(t, mux)

	if _, err := c.ReadEvents(context.Background()); err == nil {
		t.Error("expected an error for an unparseable router clock")
	}
}

func TestClockAcceptsPre710DateFormat(t *testing.T) {
	got, err := Clock{Date: "sep/27/2026", Time: "10:00:00"}.wallClock()
	if err != nil {
		t.Fatalf("wallClock: %v", err)
	}
	if want := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("wallClock = %v, want %v", got, want)
	}
}
