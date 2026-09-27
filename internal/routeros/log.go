package routeros

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LogEntry is one entry of GET /rest/log, exactly as the router returns it.
// Time is the router's own rendering of its local wall clock and is only
// meaningful relative to that router's clock (see ParseLogTime).
type LogEntry struct {
	ID      string `json:".id"`
	Time    string `json:"time"`
	Topics  string `json:"topics"`
	Message string `json:"message"`
}

// Clock is the response of GET /rest/system/clock.
type Clock struct {
	Time      string `json:"time"`
	Date      string `json:"date"`
	GMTOffset string `json:"gmt-offset"`
}

// LogKind is why a log entry was kept by the events filter (project.md §5.7).
type LogKind string

const (
	LogKindVRRP     LogKind = "vrrp"
	LogKindNetwatch LogKind = "netwatch"
	// LogKindMtha is a message written by a tool-deployed script (the
	// runtime templates log "mtha: ..." from on-master/on-backup, §5.5).
	LogKindMtha LogKind = "mtha"
)

// mthaLogPrefix marks log lines written by the tool's own runtime scripts,
// matching the "mtha:" tag every tool-created object carries (§7.3).
const mthaLogPrefix = "mtha:"

// ClassifyLog reports whether e belongs on the events timeline (project.md
// §5.7: vrrp, netwatch and "mtha:"-prefixed entries) and, if so, why.
// Topics are matched as whole comma-separated tokens ("vrrp,info"), not
// substrings. An "mtha:" message wins over its topics, since it is the tool's
// own script speaking rather than RouterOS reporting.
func ClassifyLog(e LogEntry) (LogKind, bool) {
	if strings.HasPrefix(strings.TrimSpace(e.Message), mthaLogPrefix) {
		return LogKindMtha, true
	}
	for _, topic := range strings.Split(e.Topics, ",") {
		switch strings.TrimSpace(topic) {
		case "vrrp":
			return LogKindVRRP, true
		case "netwatch":
			return LogKindNetwatch, true
		}
	}
	return "", false
}

// LogEvent is a LogEntry kept by ClassifyLog, with its timestamp resolved to
// an absolute instant on the tool's clock.
type LogEvent struct {
	LogEntry
	Kind LogKind
	// At is when the entry was logged, in the tool's time frame (see
	// ReadEvents). It is the zero Time only when neither this entry's
	// timestamp nor any earlier one in the same log could be parsed.
	At time.Time
}

// EventLog is one router's filtered log as read by ReadEvents.
type EventLog struct {
	Events []LogEvent
	// FetchedAt is the tool's clock when the router's clock was read.
	FetchedAt time.Time
	// Skew is how far the router's clock is ahead of the tool's (negative
	// if behind). SkewKnown is false when the router's gmt-offset could not
	// be parsed, in which case Skew is zero and meaningless.
	Skew      time.Duration
	SkewKnown bool
}

// Log fetches the router's entire in-memory log (/log). RouterOS bounds this
// by its memory logging action (1000 lines by default), so it is read whole
// and filtered client-side: REST query filters only do exact matches, which
// can't express "topics contains vrrp".
func (c *Client) Log(ctx context.Context) ([]LogEntry, error) {
	return getInto[[]LogEntry](ctx, c, "/log")
}

// Clock fetches the router's current wall-clock date, time and UTC offset.
func (c *Client) Clock(ctx context.Context) (*Clock, error) {
	clk, err := getInto[Clock](ctx, c, "/system/clock")
	if err != nil {
		return nil, err
	}
	return &clk, nil
}

// ReadEvents is the events log reader (project.md §5.7): it reads /log,
// keeps only the entries ClassifyLog accepts, and resolves their timestamps.
//
// Log timestamps are router-local wall-clock strings, often without a date
// or year. Rather than trusting each router's timezone and clock, ReadEvents
// reads /system/clock straight after /log and places every entry at
// FetchedAt minus its wall-clock distance from the router's "now". That puts
// both routers' entries, and the tool's own actions, on one clock even when
// the routers' clocks are unsynchronised or in different zones, at the cost
// of about a second of jitter (RouterOS timestamps have 1s resolution). The
// log is read before the clock so no entry is newer than the reference,
// which is what lets ParseLogTime infer a missing date or year.
func (c *Client) ReadEvents(ctx context.Context) (EventLog, error) {
	entries, err := c.Log(ctx)
	if err != nil {
		return EventLog{}, err
	}
	clk, err := c.Clock(ctx)
	if err != nil {
		return EventLog{}, err
	}
	fetchedAt := time.Now()

	routerNow, err := clk.wallClock()
	if err != nil {
		return EventLog{}, err
	}
	log := EventLog{
		Events:    FilterEvents(entries, routerNow, fetchedAt),
		FetchedAt: fetchedAt,
	}
	if offset, ok := parseGMTOffset(clk.GMTOffset); ok {
		// routerNow is wall time labelled UTC; subtracting the router's
		// offset gives the instant it believes it is.
		log.Skew = routerNow.Add(-offset).Sub(fetchedAt).Round(time.Second)
		log.SkewKnown = true
	}
	return log, nil
}

// FilterEvents keeps the entries ClassifyLog accepts, in log order, and
// resolves each timestamp against routerNow (the router's wall clock at
// fetchedAt, labelled UTC as ParseLogTime expects). An entry whose timestamp
// can't be parsed inherits the previous entry's instant, since RouterOS logs
// are written in order; it is kept rather than dropped so nothing silently
// disappears from the timeline.
func FilterEvents(entries []LogEntry, routerNow, fetchedAt time.Time) []LogEvent {
	var (
		out  []LogEvent
		last time.Time
	)
	for _, e := range entries {
		// Resolve every entry, not only kept ones, so an unparseable kept
		// entry inherits its true predecessor's time.
		if wall, err := ParseLogTime(e.Time, routerNow); err == nil {
			last = fetchedAt.Add(wall.Sub(routerNow))
		}
		kind, ok := ClassifyLog(e)
		if !ok {
			continue
		}
		out = append(out, LogEvent{LogEntry: e, Kind: kind, At: last})
	}
	return out
}

// logTimeLayouts are the renderings RouterOS 7 uses for a log entry's time,
// most specific first. Which one appears depends on the entry's age and on
// the RouterOS version (7.10 moved from "sep/27" style dates to ISO ones).
var logTimeLayouts = []struct {
	layout           string
	hasDate, hasYear bool
}{
	{"2006-01-02 15:04:05", true, true},
	{"Jan/02/2006 15:04:05", true, true},
	{"Jan/02 15:04:05", true, false},
	{"01-02 15:04:05", true, false},
	{"15:04:05", false, false},
}

// ParseLogTime parses a /log "time" value into a wall-clock time labelled
// UTC, filling a missing date or year from routerNow (the router's wall
// clock, also labelled UTC, read after the log). RouterOS omits the date for
// today's entries and the year for this year's, so an entry that would land
// after routerNow must belong to the previous day or year respectively —
// which is also what makes an entry logged just before midnight, read just
// after it, resolve correctly.
func ParseLogTime(raw string, routerNow time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, l := range logTimeLayouts {
		t, err := time.Parse(l.layout, raw)
		if err != nil {
			continue
		}
		switch {
		case !l.hasDate:
			t = time.Date(routerNow.Year(), routerNow.Month(), routerNow.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			if t.After(routerNow) {
				t = t.AddDate(0, 0, -1)
			}
		case !l.hasYear:
			t = time.Date(routerNow.Year(), t.Month(), t.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			if t.After(routerNow) {
				t = t.AddDate(-1, 0, 0)
			}
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised log time %q", raw)
}

// wallClock returns the router's current wall-clock time labelled UTC.
// /system/clock renders the date as "sep/27/2026" before RouterOS 7.10 and
// "2026-09-27" from it.
func (c Clock) wallClock() (time.Time, error) {
	raw := strings.TrimSpace(c.Date) + " " + strings.TrimSpace(c.Time)
	for _, layout := range []string{"2006-01-02 15:04:05", "Jan/02/2006 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised router clock %q", raw)
}

var gmtOffsetRe = regexp.MustCompile(`^([+-])(\d{1,2}):?(\d{2})$`)

// parseGMTOffset parses /system/clock's gmt-offset ("+02:00", "-0530").
func parseGMTOffset(raw string) (time.Duration, bool) {
	m := gmtOffsetRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[2])
	mins, _ := strconv.Atoi(m[3])
	d := time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute
	if m[1] == "-" {
		d = -d
	}
	return d, true
}
