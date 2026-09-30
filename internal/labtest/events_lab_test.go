//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mtha/internal/labtest"
	"mtha/internal/routeros"
)

// The Events timeline (project.md §5.7) against real router logs: what the
// devices themselves write when VRRP and netwatch change state, the lines
// mtha's deployed scripts add, the tool's own actions, and two properties
// only a live pair can show — ordering across routers whose clocks
// disagree, and that nothing outlives the session.

// A real failover writes vrrp lines on both routers and "mtha:" lines from
// the deployed on-master/on-backup scripts; netwatch coming up writes
// netwatch lines. All of them reach the timeline under the right router and
// kind. Everything else in the log — script output without the "mtha:"
// prefix, the config changes this test itself made — does not.
func TestLabEventsDeviceLog(t *testing.T) {
	s := readyPair(t)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	excluded := "lab-events-excluded-" + nonce
	before := "mtha: lab-events-before-" + nonce

	// Two lines the filter must drop: a plain script line, and the
	// fixture's own script, which logs two lines of its own.
	logLine(t, s.lab.A, excluded)
	if err := s.lab.A.Command(context.Background(), "/system/script/run",
		map[string]string{".id": only(t, s.lab.A, "system/script", byName("lab-hello"))[".id"]}, nil); err != nil {
		t.Fatalf("run lab-hello: %v", err)
	}
	// Marks where the failover starts, so its lines can be told from the
	// ones the boot and the deploy wrote.
	logLine(t, s.lab.A, before)

	setPriority(t, s.lab.A, strconv.Itoa(s.lab.Pair.Runtime.PriorityDegraded))
	waitRole(t, s.lab.B, routeros.RoleMaster)

	failover := []row{
		{src: "A", kind: "vrrp", msg: "vrrp-lan now BACKUP"},
		{src: "A", kind: "mtha", msg: "mtha: vrrp-lan transitioned to backup"},
		{src: "B", kind: "vrrp", msg: "vrrp-lan now MASTER"},
		{src: "B", kind: "mtha", msg: "mtha: vrrp-lan transitioned to master"},
	}
	// Each must be the newest row of its kind and no older than the marker.
	// Timestamps, not row positions: RouterOS logs to the second, so lines
	// from both routers in the marker's own second are shown in the
	// merge's fixed router order, not in the order they happened.
	v, rows := s.readEvents("the failover's lines", pollWait, func(rows []row) bool {
		mark := indexRow(rows, "A", "mtha", before)
		if mark < 0 {
			return false
		}
		for _, want := range failover {
			i := indexRow(rows, want.src, want.kind, want.msg)
			if i < 0 || !notOlder(rows[i].at, rows[mark].at) {
				return false
			}
		}
		return true
	})

	// Netwatch: each router's mtha targets came up during setup.
	for _, src := range []string{"A", "B"} {
		for _, host := range []string{"192.168.88.2", "192.168.88.3"} {
			if !hasRow(rows, src, "netwatch", "event up [ type: simple, host: "+host+" ]") {
				t.Errorf("no netwatch row from %s for %s coming up", src, host)
			}
		}
	}
	for _, r := range rows {
		switch {
		case r.kind != "vrrp" && r.kind != "netwatch" && r.kind != "mtha" && r.src != "tool":
			t.Errorf("row of unexpected kind: %s", r)
		case r.kind == "mtha" && !strings.HasPrefix(r.msg, "mtha:"):
			t.Errorf("mtha row without the mtha: prefix: %s", r)
		}
	}
	for _, gone := range []string{excluded, "lab hello", "lab second line", "changed by api", "logged in"} {
		if strings.Contains(v, gone) {
			t.Errorf("timeline shows %q, which is not a vrrp, netwatch or mtha: line:\n%s", gone, v)
		}
	}
	// The same lines really are in the device's log: the filter dropped
	// them, they were not missing.
	log, err := s.lab.A.Log(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sawExcluded, sawHello bool
	for _, e := range log {
		sawExcluded = sawExcluded || strings.Contains(e.Message, excluded)
		sawHello = sawHello || e.Message == "lab hello"
	}
	if !sawExcluded || !sawHello {
		t.Errorf("router a's log lacks the lines the filter should drop (excluded %v, lab hello %v)", sawExcluded, sawHello)
	}
}

// The tool's own writes appear on the timeline with source "tool": each
// router's runtime deploy, and a sync apply once it has finished.
func TestLabEventsToolActions(t *testing.T) {
	lab := labtest.New(t)
	logOnFailure(t, lab)
	adoptPair(t, lab)
	s := startSession(t, lab)

	s.deployRuntime()
	// The deploy restarts vrrp-lan; plan the sync once b is backup again,
	// so a single y confirms it.
	s.requireRoles("master", "backup", time.Minute)

	add(t, lab.A, "ip/firewall/filter", map[string]string{
		"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53", "comment": "mtha-lab-events-sync",
	})
	s.fetchDrift()
	s.keys("enter", " ", "4")
	s.until("sync planned", pollWait, func(v string) bool { return strings.Contains(v, "press y to apply") })
	s.keys("y")
	v := s.until("sync finished", writeWait, func(v string) bool {
		return strings.Contains(v, "applied ") || strings.Contains(v, "apply stopped")
	})
	if !strings.Contains(v, "ip/firewall/filter") || !strings.Contains(v, "clean") || strings.Contains(v, "apply stopped") {
		t.Fatalf("sync did not finish clean:\n%s", v)
	}

	_, rows := s.readEvents("the tool's actions", pollWait, func(rows []row) bool {
		return hasRow(rows, "tool", "sync", "→ B: apply ip/firewall/filter") &&
			hasRow(rows, "tool", "runtime", "→ A: deploy runtime logic") &&
			hasRow(rows, "tool", "runtime", "→ B: deploy runtime logic")
	})
	var tool []row
	for _, r := range rows {
		if r.src == "tool" {
			tool = append(tool, r)
		}
	}
	// Newest first: the sync, then the deploy's two routers.
	if len(tool) != 3 || tool[0].kind != "sync" {
		t.Fatalf("tool rows %v, want the sync then the deploy's two", tool)
	}
	for _, r := range tool {
		if m := opsDone.FindStringSubmatch(r.msg); m == nil || m[1] != m[2] {
			t.Errorf("tool row %s: want every op done", r)
		}
	}
	// The devices' own lines are still there alongside them.
	if !hasRow(rows, "A", "mtha", "mtha: vrrp-lan transitioned to") {
		t.Errorf("no mtha row from router a's deployed scripts beside the tool rows: %v", rows)
	}
}

// notOlder reports whether rendered timeline time a is at or after b. Both
// come from one read in one year, so the layout's missing year is moot.
func notOlder(a, b string) bool {
	ta, errA := time.Parse("Jan 02 15:04:05", a)
	tb, errB := time.Parse("Jan 02 15:04:05", b)
	return errA == nil && errB == nil && !ta.Before(tb)
}

var opsDone = regexp.MustCompile(`: (\d+)/(\d+) ops$`)

// clockSkew is how far router b's clock is set ahead: well past the 3s at
// which the Events screen warns, and far more than the gaps between the
// lines whose order it has to get right.
const clockSkew = 45 * time.Second

// With router b's clock well ahead, its read on the Events screen carries a
// warning with the measured skew, and the timeline still orders b's lines
// and a's by when they really happened — which b's raw timestamps alone
// would get wrong.
func TestLabEventsClockSkew(t *testing.T) {
	// Registered before New, so it runs after the reset: the reboot must
	// not have brought a skewed clock back with it.
	var lab *labtest.Lab
	t.Cleanup(func() {
		if lab != nil {
			for _, c := range []*routeros.Client{lab.A, lab.B} {
				restoreClock(t, c)
			}
		}
	})
	lab = labtest.New(t)
	logOnFailure(t, lab)
	t.Cleanup(func() { restoreClock(t, lab.B) }) // before the reset, too
	s := startSession(t, lab)

	setClock(t, lab.B, time.Now().Add(clockSkew))
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	lines := []struct {
		c   *routeros.Client
		src string
		msg string
	}{
		{lab.B, "B", "mtha: lab-skew-1-" + nonce},
		{lab.A, "A", "mtha: lab-skew-2-" + nonce},
		{lab.B, "B", "mtha: lab-skew-3-" + nonce},
	}
	for i, l := range lines {
		if i > 0 {
			// Past RouterOS's 1s timestamp resolution on both sides.
			time.Sleep(3 * time.Second)
		}
		logLine(t, l.c, l.msg)
	}

	v, rows := s.readEvents("the three skew lines", pollWait, func(rows []row) bool {
		for _, l := range lines {
			if !hasRow(rows, l.src, "mtha", l.msg) {
				return false
			}
		}
		return true
	})
	// Newest first on screen: 3, 2, 1.
	i1, i2, i3 := indexRow(rows, "B", "mtha", lines[0].msg), indexRow(rows, "A", "mtha", lines[1].msg), indexRow(rows, "B", "mtha", lines[2].msg)
	if !(i3 < i2 && i2 < i1) {
		t.Errorf("timeline order (newest first) puts line 3 at %d, 2 at %d, 1 at %d; want 3, 2, 1:\n%s", i3, i2, i1, v)
	}

	lineA, lineB := eventsRouterLine(v, "A"), eventsRouterLine(v, "B")
	m := skewWarning.FindStringSubmatch(lineB)
	if m == nil {
		t.Fatalf("router b's line does not warn of its clock: %q", lineB)
	}
	if got, err := time.ParseDuration(m[1]); err != nil || got < clockSkew-3*time.Second || got > clockSkew+3*time.Second {
		t.Errorf("router b's clock reported %s ahead, want about %s: %q", m[1], clockSkew, lineB)
	}
	if strings.Contains(lineA, "clock") {
		t.Errorf("router a's line warns of a clock that is in sync: %q", lineA)
	}

	restoreClock(t, lab.B)
}

var skewWarning = regexp.MustCompile(`clock (\S+) ahead of this machine \(timeline corrected\)`)

// eventsRouterLine is the Events screen's summary line for router src.
func eventsRouterLine(screen, src string) string {
	for _, l := range strings.Split(screen, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Router "+src+": ") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// Nothing persists across sessions: tool actions from one run of mtha are
// gone from the next, while the device's own log is still there. Both runs
// are the real binary with an empty home directory, which is also left
// empty.
func TestLabEventsNoPersistence(t *testing.T) {
	lab := labtest.New(t)
	logOnFailure(t, lab)
	adoptPair(t, lab)
	marker := "mtha: lab-persist-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	logLine(t, lab.A, marker)

	home := t.TempDir()
	env := append(lab.Env(), "HOME="+home)
	for _, xdg := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		env = append(env, xdg+"="+filepath.Join(home, strings.ToLower(xdg)))
	}
	args := []string{"-config", lab.PairFile, "-pair", labtest.PairName}

	first, err := lab.RunTUIEnv(3*time.Minute, env, append(args, "-write"),
		labtest.Input{Until: "Readiness:", Keys: "3"},
		labtest.Input{Until: "scheduler mtha-snapshot", Keys: "d"},
		labtest.Input{Until: "press y to apply", Keys: "y"},
		labtest.Input{Until: "press Y (shift+y)", Keys: "Y"},
		labtest.Input{Until: "verified — see the Runtime screen", Keys: "6"},
		labtest.Input{Until: "deploy runtime logic", Keys: "q"},
	)
	if err != nil {
		t.Fatalf("first run: %v\noutput:\n%s", err, first)
	}
	if !strings.Contains(first, marker) || !toolRow.MatchString(first) {
		t.Fatalf("first run's timeline lacks the device's marker or the deploy's tool row:\n%s", first)
	}

	second, err := lab.RunTUIEnv(time.Minute, env, args,
		labtest.Input{Until: "Readiness:", Keys: "6"},
		labtest.Input{Until: marker, Keys: "q"},
	)
	if err != nil {
		t.Fatalf("second run: %v\noutput:\n%s", err, second)
	}
	if toolRow.MatchString(second) || strings.Contains(second, "runtime logic:") {
		t.Errorf("second run's timeline still shows the first run's tool actions:\n%s", second)
	}
	// The device kept its own lines, including those from the first run's
	// deploy: the vrrp restart it caused, and the new scripts reporting it.
	for _, want := range []string{"vrrp-lan now BACKUP", "mtha: vrrp-lan transitioned to"} {
		if !strings.Contains(second, want) {
			t.Errorf("second run's timeline lacks the device's %q:\n%s", want, second)
		}
	}

	var left []string
	filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if path != home {
			left = append(left, path)
		}
		return nil
	})
	if len(left) > 0 {
		t.Errorf("mtha wrote to its home directory: %v", left)
	}
}

// toolRow matches a tool action's timeline row.
var toolRow = regexp.MustCompile(`\btool\s+(sync|runtime|failover)\s`)

// setClock sets c's wall clock to the instant at, in c's own time zone.
func setClock(t *testing.T, c *routeros.Client, at time.Time) {
	t.Helper()
	ctx := context.Background()
	clk, err := c.Clock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	offset, err := gmtOffset(clk.GMTOffset)
	if err != nil {
		t.Fatal(err)
	}
	wall := at.UTC().Add(offset)
	if err := c.Command(ctx, "/system/clock/set", map[string]string{
		"date": wall.Format("2006-01-02"), "time": wall.Format("15:04:05"),
	}, nil); err != nil {
		t.Fatalf("set clock: %v", err)
	}
}

// clockSkewOf is how far c's clock is ahead of this machine's.
func clockSkewOf(c *routeros.Client) (time.Duration, error) {
	clk, err := c.Clock(context.Background())
	if err != nil {
		return 0, err
	}
	now := time.Now()
	offset, err := gmtOffset(clk.GMTOffset)
	if err != nil {
		return 0, err
	}
	wall, err := time.Parse("2006-01-02 15:04:05", clk.Date+" "+clk.Time)
	if err != nil {
		return 0, fmt.Errorf("router clock %+v: %w", clk, err)
	}
	return wall.Add(-offset).Sub(now), nil
}

// restoreClock puts c's clock back on this machine's time, if it is more
// than a second or so off, and checks it took.
func restoreClock(t *testing.T, c *routeros.Client) {
	t.Helper()
	skew, err := clockSkewOf(c)
	if err != nil {
		t.Errorf("read clock: %v", err)
		return
	}
	if skew.Abs() <= 2*time.Second {
		return
	}
	setClock(t, c, time.Now())
	if skew, err := clockSkewOf(c); err != nil || skew.Abs() > 2*time.Second {
		t.Errorf("clock still %s off after restoring it: %v", skew, err)
	}
}

var gmtOffsetRe = regexp.MustCompile(`^([+-])(\d{1,2}):?(\d{2})$`)

// gmtOffset parses /system/clock's gmt-offset ("+01:00").
func gmtOffset(s string) (time.Duration, error) {
	m := gmtOffsetRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("unrecognised gmt-offset %q", s)
	}
	h, _ := strconv.Atoi(m[2])
	mins, _ := strconv.Atoi(m[3])
	d := time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute
	if m[1] == "-" {
		d = -d
	}
	return d, nil
}
