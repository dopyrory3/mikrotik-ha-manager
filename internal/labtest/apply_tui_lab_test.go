//go:build lab

package labtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

// The apply tests (issue #13) drive the real write path: ui.Model, with real
// pollers and REST clients, from the Drift screen's selection through the
// Apply screen's dry run, confirmations, pre-run recheck, op-by-op execution
// and post-apply verification. They see the model only as an operator does
// (its rendered screens) and assert on the routers themselves over REST.
// This file is their shared driver; apply_*_lab_test.go hold the scenarios.

// tui is one mtha session against the lab.
type tui struct {
	t        *testing.T
	d        *labtest.Driver[ui.Model]
	sections []string // the Drift screen's section list, in order
	cursor   int      // its section cursor
	fetched  bool     // drift fetched at least once
}

type tuiOptions struct {
	pair    *config.Pair // default lab.Pair
	write   bool
	pollers map[poll.RouterKey]*poll.Poller // default lab.Pollers(time.Second)
	// ready is what the Overview must show before the session is used;
	// default polledAtBaseline.
	ready func(view string) bool
}

var (
	vrrpMaster = regexp.MustCompile(`vrrp \S+\s+master`)
	vrrpBackup = regexp.MustCompile(`vrrp \S+\s+backup`)
)

// polledAtBaseline: both routers polled and reachable, one showing VRRP
// master (a) and one backup (b), so the planner's master check sees the
// roles the baseline has.
func polledAtBaseline(view string) bool {
	return !strings.Contains(view, "waiting for first poll") && !strings.Contains(view, "unreachable") &&
		vrrpMaster.MatchString(view) && vrrpBackup.MatchString(view)
}

func startTUI(t *testing.T, lab *labtest.Lab, o tuiOptions) *tui {
	t.Helper()
	if o.pair == nil {
		o.pair = lab.Pair
	}
	if o.pollers == nil {
		o.pollers = lab.Pollers(time.Second)
	}
	if o.ready == nil {
		o.ready = polledAtBaseline
	}
	s := &tui{t: t, d: labtest.Drive(t, ui.New(o.pair, o.write, o.pollers))}
	for _, sec := range o.pair.Sync.Sections {
		if !model.SectionExempt(sec, o.pair.Sync.Exempt) {
			s.sections = append(s.sections, sec)
		}
	}
	s.until("both routers polled", 45*time.Second, o.ready)
	return s
}

func (s *tui) view() string { return labtest.StripANSI(s.d.Model().View()) }

func (s *tui) keys(keys ...string) {
	s.t.Helper()
	for _, k := range keys {
		s.d.Send(labtest.Key(k))
	}
}

func (s *tui) until(what string, timeout time.Duration, cond func(view string) bool) string {
	s.t.Helper()
	s.d.Until(what, timeout, func(m ui.Model) bool { return cond(labtest.StripANSI(m.View())) })
	return s.view()
}

// drift opens the Drift screen on fresh reads.
func (s *tui) drift() string {
	s.t.Helper()
	s.keys("2")
	if s.fetched {
		s.keys("r")
	}
	s.fetched = true
	v := s.until("drift fetched", 45*time.Second, func(v string) bool {
		return !strings.Contains(v, "fetching drift...") && strings.Contains(v, "Hunks:")
	})
	if strings.Contains(v, "error:") {
		s.t.Fatalf("drift fetch failed:\n%s", v)
	}
	return v
}

// focusSection moves the Drift screen's section cursor to section.
func (s *tui) focusSection(section string) {
	s.t.Helper()
	idx := -1
	for i, sec := range s.sections {
		if sec == section {
			idx = i
		}
	}
	if idx < 0 {
		s.t.Fatalf("section %s is not on the Drift screen: %v", section, s.sections)
	}
	s.keys("esc")
	for ; s.cursor < idx; s.cursor++ {
		s.keys("j")
	}
	for ; s.cursor > idx; s.cursor-- {
		s.keys("k")
	}
}

// selectSection selects every hunk of section in dir, as a / b do.
func (s *tui) selectSection(section string, dir plan.Direction) {
	s.t.Helper()
	s.focusSection(section)
	if dir == plan.BtoA {
		s.keys("b")
	} else {
		s.keys("a")
	}
}

// selectHunk selects the index'th hunk of section (diff order) in dir, by
// cycling it with space as an operator does.
func (s *tui) selectHunk(section string, index int, dir plan.Direction) {
	s.t.Helper()
	s.focusSection(section)
	s.keys("enter")
	for i := 0; i < index; i++ {
		s.keys("j")
	}
	s.keys(" ")
	if dir == plan.BtoA {
		s.keys(" ")
	}
	s.keys("esc")
}

// plan opens the Apply screen and waits for the dry run.
func (s *tui) plan() string {
	s.t.Helper()
	s.keys("4")
	// A finished sync stays on screen until re-planned.
	if finished.MatchString(s.view()) {
		s.keys("r")
	}
	v := s.until("dry run built", 45*time.Second, func(v string) bool {
		return !strings.Contains(v, "reading both routers and planning...")
	})
	if strings.Contains(v, "planning failed") {
		s.t.Fatalf("planning failed:\n%s", v)
	}
	return v
}

var finished = regexp.MustCompile(`applied \d+/\d+ op\(s\)|apply stopped after \d+/\d+ op\(s\)`)

// confirm presses y — and, when the plan writes to a possible master,
// requires the second confirmation and presses Y — then waits until the
// run and its verification have finished.
func (s *tui) confirm(master bool) string {
	s.t.Helper()
	s.keys("y")
	if master {
		if v := s.view(); !strings.Contains(v, "press Y (shift+y)") {
			s.t.Fatalf("y alone should have asked for the master confirmation:\n%s", v)
		}
		s.keys("Y")
	} else if v := s.view(); strings.Contains(v, "press Y (shift+y)") {
		s.t.Fatalf("a second confirmation was asked for a plan that writes only to a backup:\n%s", v)
	}
	return s.waitDone()
}

func (s *tui) waitDone() string {
	s.t.Helper()
	return s.until("apply finished and verified", 2*time.Minute, func(v string) bool { return finished.MatchString(v) })
}

// requireApplied fails unless the run finished without error and verified
// every section it touched clean.
func requireApplied(t *testing.T, v string) {
	t.Helper()
	if !regexp.MustCompile(`applied (\d+)/(\d+) op\(s\)`).MatchString(v) {
		t.Fatalf("apply did not finish cleanly:\n%s", v)
	}
	m := regexp.MustCompile(`applied (\d+)/(\d+) op\(s\)`).FindStringSubmatch(v)
	if m[1] != m[2] {
		t.Fatalf("applied %s of %s ops:\n%s", m[1], m[2], v)
	}
	if strings.Contains(v, "residual") || strings.Contains(v, "not verified") || strings.Contains(v, "verify error") {
		t.Fatalf("post-apply verification does not report the touched sections clean:\n%s", v)
	}
}

// shownOp is one op as the dry run lists it.
type shownOp struct {
	Router string // "a" or "b"
	Method string
	Path   string
	Body   map[string]string
}

// Section is the config section the op's path writes to ("" for the
// backup).
func (o shownOp) Section() string {
	p := strings.TrimPrefix(o.Path, "/")
	if strings.HasPrefix(p, "system/backup/") {
		return ""
	}
	p = strings.TrimSuffix(p, "/unset")
	if i := strings.LastIndex(p, "/*"); i >= 0 {
		p = p[:i]
	}
	return p
}

func (o shownOp) String() string {
	body, _ := json.Marshal(o.Body)
	return fmt.Sprintf("%s: %s %s %s", o.Router, o.Method, o.Path, body)
}

var (
	opLine     = regexp.MustCompile(`^\S*\s*\d+\. (PUT|PATCH|DELETE|POST) (\S+)(?: (\{.*\}))?\s*$`)
	routerLine = regexp.MustCompile(`^Router ([AB])\s*$`)
)

// shownOps parses the ops the Apply screen lists, in order.
func shownOps(t *testing.T, view string) []shownOp {
	t.Helper()
	var ops []shownOp
	router := ""
	for _, line := range strings.Split(view, "\n") {
		line = strings.TrimSpace(line)
		if m := routerLine.FindStringSubmatch(line); m != nil {
			router = strings.ToLower(m[1])
			continue
		}
		m := opLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		op := shownOp{Router: router, Method: m[1], Path: m[2]}
		if m[3] != "" {
			if err := json.Unmarshal([]byte(m[3]), &op.Body); err != nil {
				t.Fatalf("op body %s: %v", m[3], err)
			}
		}
		ops = append(ops, op)
	}
	return ops
}

// backupOf is the name the plan's pre-apply backup on router gives its
// file, and fails if the plan does not start router's writes with one.
func backupOf(t *testing.T, ops []shownOp, router string) string {
	t.Helper()
	for _, op := range ops {
		if op.Router != router {
			continue
		}
		if op.Method != "POST" || op.Path != "/system/backup/save" || op.Body["name"] == "" {
			t.Fatalf("router %s's writes do not start with a pre-apply backup: first op is %s", router, op)
		}
		return op.Body["name"]
	}
	t.Fatalf("the plan does not write to router %s", router)
	return ""
}

// preApplyBackups lists the mtha-pre-apply* files on c.
func preApplyBackups(t *testing.T, c *routeros.Client) []string {
	t.Helper()
	var files []map[string]string
	if err := c.Get(context.Background(), "/file", &files); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f["name"], plan.DefaultBackupName) {
			out = append(out, f["name"])
		}
	}
	return out
}

// chainPos is the position of the one static rule match accepts within
// chain on c (0 is the head), or -1 if there is none.
func chainPos(t *testing.T, c *routeros.Client, section, chain string, match func(map[string]string) bool) int {
	t.Helper()
	var rules []map[string]string
	if err := c.Get(context.Background(), "/"+section, &rules); err != nil {
		t.Fatalf("%s: %v", section, err)
	}
	pos, found := 0, -1
	for _, r := range rules {
		if r["dynamic"] == "true" || r["chain"] != chain {
			continue
		}
		if match(r) {
			if found >= 0 {
				t.Fatalf("%s chain %s: more than one matching rule", section, chain)
			}
			found = pos
		}
		pos++
	}
	return found
}

// requireSamePosition asserts that the rule is on both routers, at the same
// position in its chain.
func requireSamePosition(t *testing.T, lab *labtest.Lab, section, chain string, match func(map[string]string) bool) {
	t.Helper()
	a, b := chainPos(t, lab.A, section, chain, match), chainPos(t, lab.B, section, chain, match)
	if a < 0 || b < 0 || a != b {
		t.Fatalf("%s chain %s: rule at position %d on a and %d on b (-1: absent)\n  a: %v\n  b: %v", section, chain, a, b,
			chainOrder(t, lab.A, section, chain), chainOrder(t, lab.B, section, chain))
	}
	t.Logf("%s chain %s: rule at position %d on both routers", section, chain, a)
}

// hunkIndex is the position of identity among section's hunks, the order
// the Drift screen lists them in.
func hunkIndex(t *testing.T, lab *labtest.Lab, section, identity string) int {
	t.Helper()
	d := compare(t, lab, section, lab.Pair.Sync.Exempt)
	for i, h := range d.Hunks {
		if h.Identity == identity {
			return i
		}
	}
	t.Fatalf("%s: no hunk %q in %v", section, identity, describe(d))
	return -1
}

// deviceState is a router's configuration in the given sections — every
// static entry's .id and normalised fields, in list order — plus its file
// names. Two equal states mean nothing was added, removed, recreated or
// changed; counters and other read-only state are left out, as drift
// leaves them out.
func deviceState(t *testing.T, lab *labtest.Lab, c *routeros.Client, sections []string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, section := range sections {
		raw, err := c.GetSection(ctx, section)
		if err != nil {
			t.Fatalf("%s: %v", section, err)
		}
		kept := model.Select(section, raw)
		norm := model.Normalize(section, raw, nil)
		fmt.Fprintf(&b, "%s:\n", section)
		for i := range kept {
			fields, _ := json.Marshal(norm[i])
			fmt.Fprintf(&b, "  %v %s\n", kept[i][".id"], fields)
		}
	}
	var files []map[string]string
	if err := c.Get(ctx, "/file", &files); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f["name"]
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "files: %v\n", names)
	return b.String()
}

// withSections is a copy of the lab pair syncing sections in the given
// order.
func withSections(lab *labtest.Lab, sections []string) *config.Pair {
	p := *lab.Pair
	p.Sync.Sections = append([]string(nil), sections...)
	return &p
}

// withoutExempt is a copy of the lab pair that no longer exempts the given
// entries of sync.exempt, as a pair file that leaves them out would.
func withoutExempt(lab *labtest.Lab, drop ...string) *config.Pair {
	p := *lab.Pair
	p.Sync.Exempt = nil
	for _, ex := range lab.Pair.Sync.Exempt {
		keep := true
		for _, d := range drop {
			keep = keep && ex != d
		}
		if keep {
			p.Sync.Exempt = append(p.Sync.Exempt, ex)
		}
	}
	return &p
}

// sampleSections is the section order the shipped sample pair file
// (`mtha -init`) configures.
func sampleSections(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairs.yaml")
	if err := os.WriteFile(path, []byte(config.SampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Pairs) == 0 {
		t.Fatal("the sample pair file has no pairs")
	}
	return f.Pairs[0].Sync.Sections
}

// clientAs is a REST client for router key logging in as user.
func clientAs(lab *labtest.Lab, key, user, password string) *routeros.Client {
	r := lab.Pair.Routers[key]
	return routeros.New(routeros.Config{
		Host: r.Host, Port: r.Port, User: user, Password: password,
		InsecureTLS: r.InsecureTLS, Timeout: 10 * time.Second,
	})
}
