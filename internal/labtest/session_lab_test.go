//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/labtest"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

// Helpers shared by the readiness, VRRP and events suites: a real ui.Model
// driven against the lab, read back through what it renders, and the device
// arrangements that bring the pair to a fully Ready state.
//
// These tests sit outside package ui, so they see the model only as an
// operator does, through View(). That is the point: the readiness verdict,
// its reasons and the timeline are asserted as they are shown.

// session is one run of the TUI's model against the lab.
type session struct {
	t   *testing.T
	lab *labtest.Lab
	d   *labtest.Driver[ui.Model]
}

// Long enough for a fresh poll or read to land on a loaded host (see the
// README's concurrency figures), short enough to fail fast.
const (
	pollWait  = 30 * time.Second
	writeWait = 90 * time.Second
)

// startSession drives a write-mode model with a 1s poll interval, on a
// screen large enough that no timeline row or readiness note is cut short.
func startSession(t *testing.T, lab *labtest.Lab) *session {
	t.Helper()
	d := labtest.Drive(t, ui.New(lab.Pair, true, lab.Pollers(time.Second)))
	d.Send(tea.WindowSizeMsg{Width: 250, Height: 400})
	s := &session{t: t, lab: lab, d: d}
	s.until("both routers polled", pollWait, func(v string) bool {
		return len(panelRoles(v)) == 2 && !strings.Contains(v, "unreachable") && !strings.Contains(v, "waiting for first poll")
	})
	return s
}

// screen is the current screen with escape sequences stripped.
func (s *session) screen() string { return labtest.StripANSI(s.d.Model().View()) }

// until processes messages until the rendered screen satisfies cond.
func (s *session) until(what string, timeout time.Duration, cond func(screen string) bool) string {
	s.t.Helper()
	m := s.d.Until(what, timeout, func(m ui.Model) bool { return cond(labtest.StripANSI(m.View())) })
	return labtest.StripANSI(m.View())
}

// settle is how long a polled condition must hold before it counts. The
// driver can hold one snapshot per router polled before the device was
// changed, delivered first when processing resumes; three seconds at a 1s
// poll interval spans at least two snapshots taken after it.
const settle = 3 * time.Second

// settled is until for conditions on polled state: cond must hold on every
// screen for settle.
func (s *session) settled(what string, timeout time.Duration, cond func(screen string) bool) string {
	s.t.Helper()
	var since time.Time
	return s.until(what, timeout, func(v string) bool {
		if !cond(v) {
			since = time.Time{}
			return false
		}
		if since.IsZero() {
			since = time.Now()
		}
		return time.Since(since) >= settle
	})
}

// keys presses each key in turn.
func (s *session) keys(keys ...string) {
	s.t.Helper()
	for _, k := range keys {
		s.d.Send(labtest.Key(k))
	}
}

// fetchDrift opens the Drift screen and (re)fetches it, returning the
// rendered result.
func (s *session) fetchDrift() string {
	s.t.Helper()
	s.keys("2")
	if !strings.Contains(s.screen(), "fetching drift...") {
		s.keys("r")
	}
	v := s.until("drift fetched", pollWait, func(v string) bool {
		return !strings.Contains(v, "fetching drift...") && strings.Contains(v, "Sections")
	})
	if strings.Contains(v, "error:") {
		s.t.Fatalf("drift fetch failed:\n%s", v)
	}
	return v
}

// verifyRuntime opens the Runtime screen and (re)verifies it.
func (s *session) verifyRuntime() string {
	s.t.Helper()
	s.keys("3")
	s.until("runtime screen idle", pollWait, func(v string) bool { return !strings.Contains(v, "working...") })
	s.keys("r")
	v := s.until("runtime verified", pollWait, func(v string) bool {
		return !strings.Contains(v, "working...") && strings.Contains(v, "scheduler mtha-snapshot")
	})
	if strings.Contains(v, "error:") {
		s.t.Fatalf("runtime verify failed:\n%s", v)
	}
	return v
}

// deployRuntime runs the Runtime screen's deploy through the Apply screen,
// confirming it for router a, the VRRP master, with the second key.
func (s *session) deployRuntime() {
	s.t.Helper()
	s.verifyRuntime()
	s.keys("d")
	s.until("deploy planned", pollWait, func(v string) bool { return strings.Contains(v, "press y to apply") })
	s.keys("y")
	s.until("master confirmation asked", pollWait, func(v string) bool { return strings.Contains(v, "press Y (shift+y)") })
	s.keys("Y")
	v := s.until("deploy finished", writeWait, func(v string) bool {
		return strings.Contains(v, "applied ") || strings.Contains(v, "apply stopped")
	})
	if !strings.Contains(v, "verified — see the Runtime screen (3)") {
		s.t.Fatalf("runtime deploy did not verify clean:\n%s", v)
	}
}

// The six readiness checks, as the dashboard labels them (project.md §5.2).
const (
	checkReachable = "Both routers reachable"
	checkVersions  = "RouterOS versions match"
	checkDrift     = "No unresolved drift in synced sections"
	checkMaster    = "Exactly one master per VRRP instance"
	checkRuntime   = "Runtime logic present and identical on both routers"
	checkNetwatch  = "Standby netwatch targets up"
)

var allChecks = []string{checkReachable, checkVersions, checkDrift, checkMaster, checkRuntime, checkNetwatch}

// readiness is the dashboard's verdict block as rendered: the verdict, and
// each check's line — "ok" when it passes, otherwise "red", followed by its
// note in parentheses when it has one.
type readiness struct {
	verdict string
	checks  map[string]string
}

func (r readiness) String() string {
	lines := []string{"Readiness: " + r.verdict}
	for _, c := range allChecks {
		lines = append(lines, fmt.Sprintf("  %s: %s", c, r.checks[c]))
	}
	return strings.Join(lines, "\n")
}

// parseReadiness reads the verdict block off a rendered dashboard.
func parseReadiness(screen string) readiness {
	r := readiness{checks: map[string]string{}}
	for _, line := range strings.Split(screen, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Readiness: "); ok {
			r.verdict = v
			continue
		}
		var state string
		switch {
		case strings.HasPrefix(line, "✓ "):
			state = "ok"
		case strings.HasPrefix(line, "✗ "):
			state = "red"
		default:
			continue
		}
		rest := line[len("✓ "):]
		for _, c := range allChecks {
			if note, ok := strings.CutPrefix(rest, c); ok {
				r.checks[c] = strings.TrimSpace(state + " " + strings.TrimSpace(note))
			}
		}
	}
	return r
}

// wantReadiness builds the expected verdict block: every check passes with
// no note except those in red, each mapped to its note ("" for none).
func wantReadiness(verdict string, red map[string]string) readiness {
	r := readiness{verdict: verdict, checks: map[string]string{}}
	for _, c := range allChecks {
		r.checks[c] = "ok"
	}
	for c, note := range red {
		r.checks[c] = "red"
		if note != "" {
			r.checks[c] += " (" + note + ")"
		}
	}
	return r
}

// requireReadiness switches to the dashboard and waits until its verdict
// block is exactly want: the verdict, which checks fail, and why.
func (s *session) requireReadiness(want readiness, timeout time.Duration) string {
	s.t.Helper()
	s.keys("1")
	return s.settled("readiness:\n"+want.String(), timeout, func(v string) bool {
		return parseReadiness(v).String() == want.String()
	})
}

// requireReady waits for the fully green verdict.
func (s *session) requireReady(timeout time.Duration) string {
	s.t.Helper()
	return s.requireReadiness(wantReadiness("Ready", nil), timeout)
}

// panelRoles are the VRRP roles the dashboard's router panels show for
// vrrp-lan, router a's first.
var panelRole = regexp.MustCompile(`vrrp vrrp-lan\s+(\w+)`)

func panelRoles(screen string) []string {
	var roles []string
	for _, m := range panelRole.FindAllStringSubmatch(screen, -1) {
		roles = append(roles, m[1])
	}
	return roles
}

// requireRoles switches to the dashboard and waits until its panels show
// vrrp-lan as roleA on router a and roleB on router b.
func (s *session) requireRoles(roleA, roleB string, timeout time.Duration) string {
	s.t.Helper()
	s.keys("1")
	return s.settled(fmt.Sprintf("dashboard showing a %s, b %s", roleA, roleB), timeout, func(v string) bool {
		r := panelRoles(v)
		return len(r) == 2 && r[0] == roleA && r[1] == roleB
	})
}

// Runtime tags, as internal/runtime writes and matches them for the lab
// pair's one instance (testlab/pairs.yaml).
const (
	vrrpTag = "mtha:vrrp:" + labtest.VRRPName
	vipTag  = "mtha:vrrp:" + labtest.VRRPName + ":" + labtest.VIP
)

// adoptPair hands the lab's VRRP pair to mtha's runtime management and
// clears the fixture's netwatch entries, on both routers, so a deploy can
// take the pair to Ready:
//
//   - provision.sh builds vrrp-lan and its VIP untagged, and a deploy leaves
//     an untagged object carrying mtha's name alone ("exists, not managed by
//     mtha"), so both are tagged as mtha's. The deploy then adds its scripts
//     to that same instance; VRRP itself is never taken down.
//   - The netwatch check counts every entry on both routers, and the
//     fixture's "lab: nobody" is down by design (a disabled entry reports
//     "unknown", which counts against it too), so both fixture entries go.
//     What is left after the deploy is mtha's own targets.
func adoptPair(t *testing.T, lab *labtest.Lab) {
	t.Helper()
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		v := onlyVRRP(t, c)
		patch(t, c, "interface/vrrp", v.ID, map[string]string{"comment": vrrpTag})
		vip := only(t, c, "ip/address", func(e map[string]string) bool {
			return e["address"] == labtest.VIP && e["interface"] == labtest.VRRPName
		})
		patch(t, c, "ip/address", vip[".id"], map[string]string{"comment": vipTag})
		for _, comment := range []string{"lab: vip", "lab: nobody"} {
			remove(t, c, "tool/netwatch", only(t, c, "tool/netwatch", byComment(comment))[".id"])
		}
	}
}

// skipNetwatchStartupDelay sets startup-delay=0s on every netwatch entry on
// both routers. RouterOS holds a netwatch probe for startup-delay (5m by
// default) after boot, reporting "unknown" meanwhile, and every lab reset
// reboots both routers; without this each test would wait out five minutes
// for its netwatch to come up. The deploy does not set the field, so this
// changes nothing the runtime verification compares.
func skipNetwatchStartupDelay(t *testing.T, lab *labtest.Lab) {
	t.Helper()
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		var entries []map[string]string
		if err := c.Get(context.Background(), "/tool/netwatch", &entries); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			patch(t, c, "tool/netwatch", e[".id"], map[string]string{"startup-delay": "0s"})
		}
	}
}

// readyPair is a lab pair taken to the fully green state only a live pair can
// produce: both routers reachable on one version, drift fetched and clean,
// mtha's runtime deployed and verified on both, and every netwatch target
// up — with a model that has seen all of it.
func readyPair(t *testing.T) *session {
	t.Helper()
	lab := labtest.New(t)
	logOnFailure(t, lab)
	adoptPair(t, lab)
	s := startSession(t, lab)
	s.deployRuntime()
	skipNetwatchStartupDelay(t, lab)
	s.fetchDrift()
	// The deploy restarts vrrp-lan on both routers (it sets on-master and
	// on-backup), so a re-election follows it.
	s.requireRoles("master", "backup", time.Minute)
	s.requireReady(time.Minute)
	return s
}

// logOnFailure registers a cleanup that, if the test has failed, logs both
// routers' vrrp, netwatch and script lines and their VRRP state: what the
// devices did is the first question about a failure here, and the reset
// that follows reboots them, which clears their logs.
func logOnFailure(t *testing.T, lab *labtest.Lab) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx := context.Background()
		for _, key := range []string{"a", "b"} {
			c := lab.Client(key)
			log, err := c.Log(ctx)
			if err != nil {
				t.Logf("router %s log: %v", key, err)
				continue
			}
			for _, e := range log {
				if _, ok := routeros.ClassifyLog(e); ok || strings.Contains(e.Topics, "script") || strings.Contains(e.Message, "vrrp") {
					t.Logf("router %s log: %s %s %s", key, e.Time, e.Topics, e.Message)
				}
			}
			vrrp, err := c.VRRP(ctx)
			t.Logf("router %s VRRP: %+v %v", key, vrrp, err)
		}
	})
}

// setPriority sets vrrp-lan's priority on c.
func setPriority(t *testing.T, c *routeros.Client, priority string) {
	t.Helper()
	patch(t, c, "interface/vrrp", onlyVRRP(t, c).ID, map[string]string{"priority": priority})
}

// setVRRPDisabled enables or disables vrrp-lan on c.
func setVRRPDisabled(t *testing.T, c *routeros.Client, disabled bool) {
	t.Helper()
	patch(t, c, "interface/vrrp", onlyVRRP(t, c).ID, map[string]string{"disabled": fmt.Sprint(disabled)})
}

// logLine writes msg to c's log from a script, as the deployed runtime
// scripts do (topics "script,info").
func logLine(t *testing.T, c *routeros.Client, msg string) {
	t.Helper()
	script := fmt.Sprintf(`:log info %q`, msg)
	if err := c.Command(context.Background(), "/execute", map[string]string{"script": script, "as-string": ""}, nil); err != nil {
		t.Fatalf("log %q: %v", msg, err)
	}
}

// row is one rendered timeline row.
type row struct {
	at             string // "Jan 02 15:04:05", this machine's local time
	src, kind, msg string
}

func (r row) String() string { return fmt.Sprintf("%-4s %-8s %s", r.src, r.kind, r.msg) }

// timelineRow matches renderTimeline's "time  src  kind  message" rows.
var timelineRow = regexp.MustCompile(`^([A-Z][a-z]{2} \d{2} \d{2}:\d{2}:\d{2}|--)\s{2,}(A|B|tool)\s+(\S+)\s+(.*)$`)

// timeline reads the Events screen's rows, newest first as rendered.
func timeline(screen string) []row {
	var rows []row
	for _, line := range strings.Split(screen, "\n") {
		if m := timelineRow.FindStringSubmatch(strings.TrimRight(line, " ")); m != nil {
			rows = append(rows, row{at: m[1], src: m[2], kind: m[3], msg: m[4]})
		}
	}
	return rows
}

// readEvents opens the Events screen, which re-reads both logs, and waits
// until the read has landed and cond holds for the rows; with a false cond
// it refreshes and tries again, so a log line still being written is
// picked up by a later read.
func (s *session) readEvents(what string, timeout time.Duration, cond func([]row) bool) (string, []row) {
	s.t.Helper()
	s.keys("6")
	deadline := time.Now().Add(timeout)
	for {
		v := s.until("events read", pollWait, func(v string) bool {
			return strings.Contains(v, "Router A: ") && strings.Contains(v, "Router B: ") &&
				!strings.Contains(v, "reading") && strings.Contains(v, "event(s), read")
		})
		if strings.Contains(v, "error:") {
			s.t.Fatalf("events read failed:\n%s", v)
		}
		rows := timeline(v)
		if cond(rows) {
			return v, rows
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after %s waiting for %s; screen:\n%s", timeout, what, v)
		}
		time.Sleep(time.Second)
		s.keys("r")
	}
}

// hasRow reports whether rows has one from src of kind whose message
// contains msg.
func hasRow(rows []row, src, kind, msg string) bool {
	return indexRow(rows, src, kind, msg) >= 0
}

func indexRow(rows []row, src, kind, msg string) int {
	for i, r := range rows {
		if r.src == src && r.kind == kind && strings.Contains(r.msg, msg) {
			return i
		}
	}
	return -1
}
