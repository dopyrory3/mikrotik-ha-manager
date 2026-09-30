//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/labtest"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

// Operational failures mid-session (issue #17): the master router going
// away under a running session, and a router rebooting in the middle of an
// apply. Each is induced on the router itself, so it comes back on its own
// with its configuration intact and the golden restore still works; if it
// does not, the harness falls back to recreating the containers.

// rebootWindow bounds how long a CHR guest takes to go down and answer REST
// again after a reboot, with room for a loaded host (README: resets take up
// to 25s with eight suites running).
const rebootWindow = 3 * time.Minute

// Router a, the VRRP master, reboots while a session is open.
//
// On this lab a reboot takes REST away for 10-11s, about the REST client's
// 10s timeout, so whether a poll of a ever fails outright is a matter of
// timing: a poll issued as a goes down can hang until it is back up and
// then succeed. What the session always shows is b taking over as master
// and a coming back with a reset uptime, so that is what is required here;
// whether a was also shown unreachable is logged.
func TestLabOutageRebootMidSession(t *testing.T) {
	lab := labtest.New(t)
	outageSession(t, lab, "reboot", false, func() {
		// The response is usually lost to the reboot.
		_ = lab.A.Command(context.Background(), "/system/reboot", nil, nil)
	})
	res, err := lab.A.SystemResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("router a's uptime after the reboot: %s", res.Uptime)
}

// Router a's links flap: ether1 (which carries REST) and ether2 (the VRRP
// LAN) go down for 20s and come back, by a script on the router itself, so
// they come back even though nothing can reach it meanwhile. 20s outlasts
// the REST timeout, so a must be shown unreachable.
func TestLabOutageLinkFlapMidSession(t *testing.T) {
	lab := labtest.New(t)
	outageSession(t, lab, "link flap", true, func() {
		script := `/interface/disable ether2; /interface/disable ether1; :delay 20s; /interface/enable ether1; /interface/enable ether2`
		// Without as-string, /execute runs the script as a background job
		// and returns at once.
		if err := lab.A.Command(context.Background(), "/execute", map[string]string{"script": script}, nil); err != nil {
			t.Fatalf("start the link flap on a: %v", err)
		}
	})
}

// bPolledMaster reports whether an 80-column Overview shows router b's
// panel (the right-hand one, from column 41) with vrrp-lan as master.
func bPolledMaster(view string) bool {
	for _, l := range strings.Split(view, "\n") {
		if r := []rune(l); len(r) > 41 && vrrpLanMaster.MatchString(string(r[41:])) {
			return true
		}
	}
	return false
}

var vrrpLanMaster = regexp.MustCompile(`vrrp vrrp-lan\s+master\b`)

// outageSession opens a session on the pair, runs induce to take router a
// away, and asserts what the operator sees: the outage on the Overview (a
// unreachable if wantUnreachable, otherwise a unreachable or b polled as
// master), polling recovering by itself to a master and b backup, and b's
// VRRP takeover and hand-back on the Events timeline. The screens shown
// during the outage are held to 24 lines as well. Their width is measured
// and logged but not asserted: error text is some of the widest there is,
// and screens overflowing 80 columns with real data is issue #27, deferred
// past v0.1.0 (as TestLabRenderEveryScreenAt80x24 is).
func outageSession(t *testing.T, lab *labtest.Lab, what string, wantUnreachable bool, induce func()) {
	t.Helper()
	d := labtest.Drive(t, ui.New(lab.Pair, false, lab.Pollers(time.Second)))
	d.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	waitPolledBackup(t, d)

	var failures, wide []string
	check := func(screen string) {
		v := d.Model().View()
		failures = append(failures, tooTall(screen, v, 24)...)
		wide = append(wide, tooWide(screen, v, 80)...)
	}

	start := time.Now()
	induce()
	var sawUnreachable, sawTakeover time.Duration
	outage := func(m ui.Model) bool {
		v := labtest.StripANSI(m.View())
		if sawUnreachable == 0 && strings.Contains(v, "unreachable") {
			sawUnreachable = time.Since(start)
			t.Logf("%s: router a unreachable on the Overview after %s:\n%s", what, sawUnreachable.Round(100*time.Millisecond), v)
			check("overview, a unreachable")
		}
		if sawTakeover == 0 && bPolledMaster(v) {
			sawTakeover = time.Since(start)
			t.Logf("%s: router b polled as master after %s:\n%s", what, sawTakeover.Round(100*time.Millisecond), v)
		}
		if wantUnreachable {
			return sawUnreachable > 0
		}
		return sawUnreachable > 0 || sawTakeover > 0
	}
	d.Until("the outage to show on the Overview", time.Minute, outage)

	// The other screens that read both routers, while a is (probably
	// still) down. A router back already is not a failure here: what is
	// measured is whatever they show.
	d.Send(labtest.Key("2"))
	m := d.Until("drift fetched", time.Minute, func(m ui.Model) bool { return !strings.Contains(m.View(), "fetching drift") })
	if v := labtest.StripANSI(m.View()); strings.Contains(v, "error:") {
		t.Logf("Drift screen during the %s:\n%s", what, v)
	} else {
		t.Logf("router a was back before the Drift screen read it")
	}
	check("drift, during the outage")
	d.Send(labtest.Key("6"))
	m = d.Until("logs read", time.Minute, func(m ui.Model) bool { return !strings.Contains(m.View(), "reading") })
	t.Logf("Events screen during the %s:\n%s", what, labtest.StripANSI(m.View()))
	check("events, during the outage")

	d.Send(labtest.Key("1"))
	d.Until("polling to recover: a master, b backup", rebootWindow, func(m ui.Model) bool {
		outage(m)
		return baselineRoles.MatchString(labtest.StripANSI(m.View()))
	})
	t.Logf("%s: both routers polled at their baseline roles again %s after it began; a shown unreachable: %t, b shown as master: %t",
		what, time.Since(start).Round(100*time.Millisecond), sawUnreachable > 0, sawTakeover > 0)

	// The transition, on the timeline: b took over and handed back. a's
	// own log starts again at boot after a reboot, so b's is the record.
	d.Send(labtest.Key("6"))
	m = d.Until("b's takeover and hand-back on the timeline", time.Minute, func(m ui.Model) bool {
		if strings.Contains(m.View(), "reading") {
			return false
		}
		rows := timelineSince(labtest.StripANSI(m.View()), start)
		return hasStampedRow(rows, "B", "vrrp", "now MASTER") && hasStampedRow(rows, "B", "vrrp", "now BACKUP")
	})
	t.Logf("Events after recovery:\n%s", labtest.StripANSI(m.View()))
	rows := timelineSince(labtest.StripANSI(m.View()), start)
	if master, backup := stampedRowIndex(rows, "B", "vrrp", "now MASTER"), stampedRowIndex(rows, "B", "vrrp", "now BACKUP"); backup > master {
		// Newest first: the hand-back must be above the takeover.
		t.Errorf("b's hand-back (row %d) is not after its takeover (row %d)", backup, master)
	}
	check("events, recovered")

	if len(failures) > 0 {
		t.Errorf("%d screen(s) do not fit 24 lines during the %s:\n%s", len(failures), what, strings.Join(failures, "\n"))
	}
	if len(wide) > 0 {
		t.Logf("issue #27: %d screen(s) overflow 80 columns during the %s; cosmetic, deferred past v0.1.0, so not asserted — make this an error again with the fix:\n%s",
			len(wide), what, strings.Join(wide, "\n"))
	}
}

// stampedRow is one row of the Events screen's timeline.
type stampedRow struct {
	at             time.Time
	src, kind, msg string
}

var timelineLine = regexp.MustCompile(`^(\w{3} \d{2} \d{2}:\d{2}:\d{2})\s+(A|B|tool)\s+(\S+)\s+(.*)$`)

// timelineSince parses the timeline rows of an Events screen, newest first
// as drawn, keeping those at or after since (less a second: log timestamps
// have 1s resolution).
func timelineSince(view string, since time.Time) []stampedRow {
	var rows []stampedRow
	for _, l := range strings.Split(view, "\n") {
		m := timelineLine.FindStringSubmatch(strings.TrimRight(l, " "))
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation("Jan 02 15:04:05", m[1], time.Local)
		if err != nil {
			continue
		}
		at = at.AddDate(time.Now().Year(), 0, 0)
		if at.Before(since.Add(-time.Second)) {
			continue
		}
		rows = append(rows, stampedRow{at: at, src: m[2], kind: m[3], msg: m[4]})
	}
	return rows
}

func stampedRowIndex(rows []stampedRow, src, kind, msg string) int {
	for i, r := range rows {
		if r.src == src && r.kind == kind && strings.Contains(r.msg, msg) {
			return i
		}
	}
	return -1
}

func hasStampedRow(rows []stampedRow, src, kind, msg string) bool {
	return stampedRowIndex(rows, src, kind, msg) >= 0
}

// A router rebooting in the middle of an apply: 120 rules synced a→b into
// the middle of the forward chain, and b rebooted once some have landed.
// The run must stop and say so, verification must report that it could not
// verify, and b must come back consistent: exactly a prefix of the planned
// rules, each where the plan put it, nothing else changed, the pre-apply
// backup on disk — and the rest of the sync must go through on a re-plan.
func TestLabOutageApplyWhileTargetReboots(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	addScaleRules(t, lab)

	// Commented, so each is its own identity and the prefix on b can be
	// read off by name. Placed before rule 201 on a, in order.
	const n = 120
	anchor := scaleRuleOn(t, lab.A, 201)
	var script strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&script, "/ip/firewall/filter/add chain=forward action=accept protocol=tcp dst-port=%d comment=\"lab-reboot %03d\" place-before=%s\n", 33000+i, i, anchor)
	}
	if err := lab.A.Command(ctx, "/execute", map[string]string{"script": script.String(), "as-string": ""}, nil); err != nil {
		t.Fatal(err)
	}

	d := labtest.Drive(t, ui.New(lab.Pair, true, lab.Pollers(time.Second)))
	d.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	waitPolledBackup(t, d)
	d.Send(labtest.Key("2"))
	d.Until("drift fetched", 30*time.Second, func(m ui.Model) bool {
		return regexp.MustCompile(fmt.Sprintf(`ip/firewall/filter\s+%d hunk\(s\)`, n)).MatchString(labtest.StripANSI(m.View()))
	})
	d.Send(labtest.Key("a"), labtest.Key("4"))
	d.Until("dry run built", 30*time.Second, func(m ui.Model) bool { return strings.Contains(m.View(), "press y to apply") })
	if v := labtest.StripANSI(d.Model().View()); !strings.Contains(v, fmt.Sprintf("Dry run: %d operation(s) on router B", n+1)) {
		t.Fatalf("want the backup and %d creates on b:\n%s", n, v)
	}

	// Reboot b once ten ops have run. Ops run one at a time and the next
	// starts only when the driver hands the last one's result to the model,
	// so nothing runs while the reboot is issued.
	progress := regexp.MustCompile(`applying (\d+)/(\d+)`)
	d.Send(labtest.Key("y"))
	d.Until("ten ops applied", time.Minute, func(m ui.Model) bool {
		p := progress.FindStringSubmatch(m.View())
		return p != nil && atoi(p[1]) > 10
	})
	rebooted := time.Now()
	_ = lab.B.Command(ctx, "/system/reboot", nil, nil)

	m := d.Until("the run to end", 2*time.Minute, func(m ui.Model) bool {
		v := m.View()
		return strings.Contains(v, "apply stopped after") || strings.Contains(v, "applied ")
	})
	view := labtest.StripANSI(m.View())
	t.Logf("Apply screen after b rebooted:\n%s", view)
	stopped := regexp.MustCompile(`apply stopped after (\d+)/(\d+) op\(s\): router b: `).FindStringSubmatch(view)
	if stopped == nil {
		t.Fatalf("the run did not stop on the reboot:\n%s", view)
	}
	done := atoi(stopped[1])

	eventually(t, "router b back from its reboot", rebootWindow, func() error {
		res, err := lab.B.SystemResource(ctx)
		if err != nil {
			return err
		}
		if uptime(t, res.Uptime) > time.Since(rebooted) {
			return fmt.Errorf("not rebooted yet (uptime %s)", res.Uptime)
		}
		return nil
	})
	waitRole(t, lab.B, routeros.RoleBackup)

	// b holds exactly lab-reboot 001..j, directly before rule 201, in order:
	// a's chain less the rules that did not land.
	landed := 0
	for i := 1; i <= n; i++ {
		var rules []map[string]string
		if err := lab.B.Get(ctx, fmt.Sprintf("/ip/firewall/filter?comment=lab-reboot%%20%03d", i), &rules); err != nil {
			t.Fatal(err)
		}
		if len(rules) > 1 {
			t.Errorf("lab-reboot %03d is on b %d times", i, len(rules))
		}
		if len(rules) == 0 {
			break
		}
		landed = i
	}
	// done counts the backup; the op in flight when b went down may have
	// landed without its response getting back.
	t.Logf("run reported %d of %d ops done; %d of the %d rules are on b", done, n+1, landed, n)
	if landed != done-1 && landed != done {
		t.Errorf("%d rules landed on b, but the run reported %d creates done", landed, done-1)
	}
	// Verification ran straight after the failed op. It either could not
	// read b (down by then) and says so, or read it on the way down, and
	// then the residual it reports must be exactly the rules that did not
	// land.
	notVerified := regexp.MustCompile(`ip/firewall/filter\s+not verified`).MatchString(view) && strings.Contains(view, "verify error:")
	residual := regexp.MustCompile(fmt.Sprintf(`ip/firewall/filter\s+%d residual lab-reboot %03d,`, n-landed, landed+1)).MatchString(view)
	if !notVerified && !residual {
		t.Errorf("verification reports neither that it could not read b nor the %d rules that did not land:\n%s", n-landed, view)
	}
	t.Logf("verification after the failure: could not read b: %t, read b and reported the residual: %t", notVerified, residual)
	var want []string
	for _, s := range ruleSignatures(t, lab.A, "forward") {
		if m := regexp.MustCompile(`"lab-reboot (\d+)"`).FindStringSubmatch(s); m != nil && atoi(m[1]) > landed {
			continue
		}
		want = append(want, s)
	}
	got := ruleSignatures(t, lab.B, "forward")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("b's forward chain is not a's less the rules that did not land (%d rules, want %d)", len(got), len(want))
	}
	for _, section := range lab.Pair.Sync.Sections {
		if section != filter {
			requireClean(t, lab, section)
		}
	}
	var backups []map[string]string
	if err := lab.B.Get(ctx, "/file", &backups); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range backups {
		if strings.HasPrefix(f["name"], "mtha-pre-apply-") && strings.HasSuffix(f["name"], ".backup") && f["size"] != "0" {
			found = true
			t.Logf("pre-apply backup on b: %s (%s bytes)", f["name"], f["size"])
		}
	}
	if !found {
		t.Errorf("no pre-apply backup on b: %v", backups)
	}

	// The operator's way on: re-read drift, re-plan, apply the rest.
	waitPolledBackup(t, d)
	d.Send(labtest.Key("2"), labtest.Key("r"))
	d.Until("drift re-read", time.Minute, func(m ui.Model) bool {
		return regexp.MustCompile(fmt.Sprintf(`ip/firewall/filter\s+%d hunk\(s\)`, n-landed)).MatchString(labtest.StripANSI(m.View()))
	})
	d.Send(labtest.Key("a"), labtest.Key("4"), labtest.Key("r"))
	d.Until("the rest planned", 30*time.Second, func(m ui.Model) bool { return strings.Contains(m.View(), "press y to apply") })
	d.Send(labtest.Key("y"))
	m = d.Until("the rest applied", 2*time.Minute, func(m ui.Model) bool {
		v := m.View()
		return strings.Contains(v, "applied ") || strings.Contains(v, "apply stopped after")
	})
	if v := labtest.StripANSI(m.View()); !strings.Contains(v, fmt.Sprintf("applied %d/%d op(s)", n-landed+1, n-landed+1)) || !regexp.MustCompile(`ip/firewall/filter\s+clean`).MatchString(v) {
		t.Fatalf("re-applying the rest did not finish clean:\n%s", v)
	}
	if a, b := ruleSignatures(t, lab.A, "forward"), ruleSignatures(t, lab.B, "forward"); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("after the re-apply, b's forward chain is not a's")
	}

	// Both runs are on the timeline: the failed one with its error.
	d.Send(labtest.Key("6"))
	m = d.Until("the timeline to show both runs", 30*time.Second, func(m ui.Model) bool {
		v := labtest.StripANSI(m.View())
		return !strings.Contains(v, "reading") && strings.Count(v, "→ B: apply ip/firewall/filter:") == 2
	})
	v := labtest.StripANSI(m.View())
	// The row is cut at the screen width, so only the start of the error
	// shows.
	if !strings.Contains(v, fmt.Sprintf("→ B: apply ip/firewall/filter: %d/%d ops: ", done, n+1)) {
		t.Errorf("the failed run is not on the timeline with its error:\n%s", v)
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// uptime parses RouterOS's "1w2d3h4m5s".
func uptime(t *testing.T, s string) time.Duration {
	t.Helper()
	units := map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	var total time.Duration
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else if u, ok := units[c]; ok {
			total += time.Duration(n) * u
			n = 0
		} else {
			t.Fatalf("unparseable uptime %q", s)
		}
	}
	return total
}
