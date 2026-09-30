package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/diff"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/runtime"
)

// The Apply screen (project.md §7.1 screen 4, milestone 3) turns the hunks
// selected on the Drift screen into a plan, shows it as a dry run, and —
// only with -write and explicit confirmation — runs it op by op, then
// re-runs drift detection on the touched sections and reports what still
// differs (§5.4).
//
// It is also where the Runtime screen's deploy and remove run (applyKind):
// runtime.Writes plans them from fresh reads, they go through the same
// steps below, and runtime.Verify re-checks them afterwards. Every router
// write mtha makes therefore goes through a plan shown here first (§7.3).
//
// Safety (§7.3), in the order an operator meets it:
//
//  1. Every entry to the screen re-plans from fresh reads, so the plan shown
//     reflects the routers now, not when drift was last fetched.
//  2. Nothing runs without -write; read-only sessions can still see the plan.
//  3. y confirms. If the plan writes to a router that currently holds VRRP
//     master — or whose VRRP state is unknown — a second, different key (Y)
//     is required.
//  4. Right before running, the plan is rebuilt once more from fresh reads.
//     If it differs from what was confirmed, nothing runs: the new plan is
//     shown for review instead.
//  5. Each router's writes start with a /system/backup/save, and execution
//     stops at the first failed op (a failed backup therefore writes
//     nothing to that router).

// applyStage is where the Apply screen is in its plan → confirm → run →
// verify flow.
type applyStage int

const (
	applyIdle          applyStage = iota // nothing selected, or planning failed
	applyPlanning                        // reading both routers and building the plan
	applyReview                          // dry run shown, waiting for y
	applyConfirmMaster                   // waiting for the second (master) confirmation, Y
	applyRechecking                      // re-planning just before running
	applyRunning                         // executing ops one at a time
	applyVerifying                       // re-running drift on the touched sections
	applyDone                            // finished (fully or after a failure); results shown
)

// applyKind is what the Apply screen's current plan is for.
type applyKind int

const (
	applySync          applyKind = iota // config sync from the Drift selection
	applyRuntimeDeploy                  // the Runtime screen's d
	applyRuntimeRemove                  // the Runtime screen's x
)

// title names the plan in the screen title.
func (k applyKind) title() string {
	switch k {
	case applyRuntimeDeploy:
		return "runtime deploy"
	case applyRuntimeRemove:
		return "runtime remove"
	default:
		return "apply"
	}
}

// runtimeAction maps a Runtime kind to the runtime package's action.
func (k applyKind) runtimeAction() runtime.Action {
	if k == applyRuntimeRemove {
		return runtime.ActionRemove
	}
	return runtime.ActionDeploy
}

// opStatus is the outcome of one op in the current run.
type opStatus int

const (
	opPending opStatus = iota
	opRunning
	opDone
	opFailed
)

// applyState is the Apply screen's state, held as a single field on Model.
type applyState struct {
	kind  applyKind
	stage applyStage
	plan  plan.Plan
	err   error // planning or execution error

	// backupName is stamped once per plan and reused by the pre-run
	// recheck, so the recheck's plan renders identically when nothing on
	// the routers has changed.
	backupName string

	// notice is a one-line message for the operator (read-only refusal, a
	// plan that changed under them, ...).
	notice string

	status []opStatus // per op in plan.Ops, during and after a run
	next   int        // index of the next op to execute

	residual      map[string]diff.SectionDiff // post-apply drift of the touched sections (sync)
	runtimeStatus runtime.Status              // post-run runtime verification (Runtime kinds)
	verifyErr     error

	scroll int // first visible line of the plan listing
}

// running reports whether the screen owns in-flight work that re-entering
// the screen must not interrupt or replace.
func (a applyState) running() bool {
	switch a.stage {
	case applyPlanning, applyRechecking, applyRunning, applyVerifying:
		return true
	}
	return false
}

// applyPlanMsg carries a freshly built plan. recheck marks the rebuild done
// immediately before execution (see safety step 4 above).
type applyPlanMsg struct {
	plan    plan.Plan
	err     error
	recheck bool
}

// applyStepMsg carries the outcome of executing plan.Ops[index].
type applyStepMsg struct {
	index int
	err   error
}

// applyVerifyMsg carries the post-run re-check: drift for a sync, runtime
// status for a Runtime kind.
type applyVerifyMsg struct {
	result        driftResultMsg
	runtimeStatus runtime.Status
	runtimeErr    error
}

// enterApplyScreen shows in-flight work, or a finished sync, as it is;
// otherwise (including after a Runtime plan) it plans a sync afresh.
func (m Model) enterApplyScreen() (tea.Model, tea.Cmd) {
	m.screen = screenApply
	if m.apply.running() || (m.apply.kind == applySync && m.apply.stage == applyDone) {
		return m, nil
	}
	m.apply.kind = applySync
	return m.startApplyPlan(false)
}

// writeBusy explains why no new write may start now, or returns "". Only
// one write path runs at a time: writing is held from the moment a
// confirmed plan starts running until its verification lands, and a plan
// still being built or checked would be replaced by a new one.
func (m Model) writeBusy() string {
	switch {
	case m.writing:
		return "a write is already running on the Apply screen (4) — wait for it to finish"
	case m.apply.running():
		return "the Apply screen (4) is busy — wait for it to finish"
	}
	return ""
}

// startApplyPlan builds a plan of the current kind from fresh reads: every
// section with a selection for a sync, the pair's runtime objects for a
// Runtime kind. A recheck keeps the confirmed plan's kind and backup name.
func (m Model) startApplyPlan(recheck bool) (tea.Model, tea.Cmd) {
	kind := m.apply.kind
	if kind == applySync && m.selectedCount() == 0 {
		m.apply = applyState{stage: applyIdle}
		return m, nil
	}

	if recheck {
		m.apply.stage = applyRechecking
	} else {
		m.apply = applyState{
			kind:       kind,
			stage:      applyPlanning,
			backupName: "mtha-pre-apply-" + time.Now().Format("20060102-150405"),
		}
	}

	ctx := m.runtimeCtx()
	clientA, clientB := m.pollers["a"].Client, m.pollers["b"].Client
	backupName := m.apply.backupName

	if kind != applySync {
		plans := m.runtimePlans
		action := kind.runtimeAction()
		return m, func() tea.Msg {
			p, err := runtime.Writes(ctx, clientA, clientB, plans, action, backupName)
			return applyPlanMsg{plan: p, err: err, recheck: recheck}
		}
	}

	sectionOrder := m.driftSections
	choices := m.selectionSnapshot()
	opts := plan.Options{
		Exempt:     m.pair.Sync.Exempt,
		BackupName: backupName,
		Users:      m.restUsers(),
	}

	return m, func() tea.Msg {
		p, err := buildApplyPlan(ctx, clientA, clientB, sectionOrder, choices, opts)
		return applyPlanMsg{plan: p, err: err, recheck: recheck}
	}
}

// restUsers is the REST user mtha logs in to each router as, so the planner
// can refuse changes that would lock it out (plan.Options.Users).
func (m Model) restUsers() map[string]string {
	users := make(map[string]string, len(m.pair.Routers))
	for key, r := range m.pair.Routers {
		users[key] = r.User
	}
	return users
}

// selectionSnapshot copies the current selection so the planning goroutine
// never reads the model's maps while Update may be mutating them.
func (m Model) selectionSnapshot() map[string]map[plan.HunkRef]plan.Direction {
	out := make(map[string]map[plan.HunkRef]plan.Direction, len(m.driftSelected))
	for section, refs := range m.driftSelected {
		cp := make(map[plan.HunkRef]plan.Direction, len(refs))
		for ref, dir := range refs {
			cp[ref] = dir
		}
		out[section] = cp
	}
	return out
}

// buildApplyPlan fetches each selected section from both routers
// concurrently and plans them, in configured section order. Unlike
// fetchDrift, any fetch failure fails the whole plan: a partial plan would
// silently drop writes the operator selected.
func buildApplyPlan(ctx context.Context, clientA, clientB *routeros.Client, sectionOrder []string, choices map[string]map[plan.HunkRef]plan.Direction, opts plan.Options) (plan.Plan, error) {
	var sections []string
	for _, s := range sectionOrder {
		if len(choices[s]) > 0 {
			sections = append(sections, s)
		}
	}

	inputs := make([]plan.SectionInput, len(sections))
	errs := make([]error, len(sections))
	var wg sync.WaitGroup
	wg.Add(len(sections))
	for i, section := range sections {
		go func(i int, section string) {
			defer wg.Done()
			a, b, err := fetchSectionPair(ctx, clientA, clientB, section)
			inputs[i] = plan.SectionInput{Section: section, A: a, B: b, Choices: choices[section]}
			errs[i] = err
		}(i, section)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return plan.Plan{}, err
		}
	}

	// The reference check's extra reads (docs/design-questions.md §3):
	// sections a planned body names that weren't fetched above, read-only
	// and only when some body names them. A failed read isn't fatal: the
	// plan then warns that the reference couldn't be checked.
	opts.Referents = map[plan.Read][]model.Entry{}
	for _, rd := range plan.ReferenceReads(inputs, opts) {
		client := clientA
		if rd.Router == "b" {
			client = clientB
		}
		if entries, err := client.GetSection(ctx, rd.Section); err == nil {
			opts.Referents[rd] = entries
		}
	}
	return plan.Build(inputs, opts), nil
}

// handleApplyMsg is Update's entry point for every Apply-screen message.
func (m Model) handleApplyMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case applyPlanMsg:
		if msg.err != nil {
			m.apply = applyState{kind: m.apply.kind, stage: applyIdle, err: msg.err}
			return m, nil
		}
		if msg.recheck && msg.plan.Render() == m.apply.plan.Render() {
			return m.startApplyRun()
		}
		m.apply.stage = applyReview
		m.apply.notice = ""
		if msg.recheck {
			m.apply.notice = "router state changed since the plan was shown — nothing was written; review the updated plan"
		}
		m.apply.plan = msg.plan
		return m, nil

	case applyStepMsg:
		if msg.index >= len(m.apply.status) {
			return m, nil
		}
		if msg.err != nil {
			m.apply.status[msg.index] = opFailed
			m.apply.err = msg.err
			return m.startApplyVerify()
		}
		m.apply.status[msg.index] = opDone
		m.apply.next = msg.index + 1
		if m.apply.next < len(m.apply.plan.Ops) {
			return m.startApplyStep()
		}
		return m.startApplyVerify()

	case applyVerifyMsg:
		m.apply.stage = applyDone
		m.writing = false
		for _, a := range applyActionEvents(m.apply) {
			m.journal.Record(a)
		}
		if m.apply.kind != applySync {
			// Fold the re-verification into the Runtime screen and readiness.
			m.apply.runtimeStatus = msg.runtimeStatus
			m.apply.verifyErr = msg.runtimeErr
			m.runtimeStatus = msg.runtimeStatus
			m.runtimeErr = msg.runtimeErr
			return m, nil
		}
		m.apply.residual = msg.result.data
		m.apply.verifyErr = msg.result.err
		// Fold the fresh drift into the Drift screen and readiness, and
		// drop selections the apply resolved.
		if m.driftData == nil {
			m.driftData = map[string]diff.SectionDiff{}
		}
		for section, sd := range msg.result.data {
			m.driftData[section] = sd
		}
		m.pruneDriftSelection(msg.result.data)
		return m, nil
	}
	return m, nil
}

func (m Model) startApplyRun() (tea.Model, tea.Cmd) {
	m.writing = true
	m.apply.stage = applyRunning
	m.apply.notice = ""
	m.apply.err = nil
	m.apply.status = make([]opStatus, len(m.apply.plan.Ops))
	m.apply.next = 0
	return m.startApplyStep()
}

// startApplyStep executes plan.Ops[next]. Ops run strictly one at a time so
// the screen can show progress and so execution stops at the first failure.
func (m Model) startApplyStep() (tea.Model, tea.Cmd) {
	i := m.apply.next
	op := m.apply.plan.Ops[i]
	m.apply.status[i] = opRunning

	ctx := m.runtimeCtx()
	client := m.pollers[poll.RouterKey(op.Router)].Client
	return m, func() tea.Msg {
		return applyStepMsg{index: i, err: plan.Execute(ctx, client, op)}
	}
}

// startApplyVerify re-runs drift detection over the sections the plan
// touched (project.md §5.4: "Re-run drift detection after apply and report
// residual differences"). It runs after a failure too, so the operator sees
// exactly what landed.
func (m Model) startApplyVerify() (tea.Model, tea.Cmd) {
	m.apply.stage = applyVerifying
	ctx := m.runtimeCtx()
	clientA, clientB := m.pollers["a"].Client, m.pollers["b"].Client
	if m.apply.kind != applySync {
		plans := m.runtimePlans
		return m, func() tea.Msg {
			status, err := runtime.Verify(ctx, clientA, clientB, plans)
			return applyVerifyMsg{runtimeStatus: status, runtimeErr: err}
		}
	}
	sections := m.apply.plan.Sections()
	exempt := m.pair.Sync.Exempt
	return m, func() tea.Msg {
		return applyVerifyMsg{result: fetchDrift(ctx, clientA, clientB, sections, exempt)}
	}
}

func (m Model) handleApplyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.apply.scroll > 0 {
			m.apply.scroll--
		}
		return m, nil
	case "down", "j":
		if m.apply.scroll < len(renderPlanOps(m.apply, m.width))-1 {
			m.apply.scroll++
		}
		return m, nil
	}

	switch m.apply.stage {
	case applyIdle, applyDone:
		if msg.String() == "r" {
			return m.startApplyPlan(false)
		}

	case applyReview:
		switch msg.String() {
		case "r":
			return m.startApplyPlan(false)
		case "y":
			if m.apply.plan.Empty() {
				return m, nil
			}
			if !m.writeMode {
				m.apply.notice = "read-only — restart with -write to apply this plan"
				return m, nil
			}
			if m.writing {
				m.apply.notice = m.writeBusy()
				return m, nil
			}
			if len(masterTargets(m, m.apply.plan.Targets())) > 0 {
				m.apply.stage = applyConfirmMaster
				m.apply.notice = ""
				return m, nil
			}
			return m.startApplyPlan(true)
		case "n", "esc":
			m.apply.notice = ""
		}

	case applyConfirmMaster:
		switch msg.String() {
		case "Y":
			return m.startApplyPlan(true)
		case "n", "esc":
			m.apply.stage = applyReview
			m.apply.notice = "cancelled — nothing was written"
		}
	}
	return m, nil
}

// masterTargets returns which of targets may hold VRRP master (see
// possibleMaster) — those require the second confirmation (project.md
// §5.4, §7.3).
func masterTargets(m Model, targets []string) []string {
	var out []string
	for _, router := range targets {
		if possibleMaster(m, router) {
			out = append(out, router)
		}
	}
	return out
}

// possibleMaster fails closed: a router counts as a possible VRRP master
// unless its last poll positively reports every VRRP entry as backup. Not
// polled yet, unreachable, a failed VRRP read, no VRRP entries at all, or
// any entry whose role is master or unknown all count, since "unknown" must
// not be treated as "safe" (project.md §10.1).
func possibleMaster(m Model, router string) bool {
	snap, ok := m.snapshots[poll.RouterKey(router)]
	if !ok || !snap.Reachable() || snap.VRRPErr != nil || len(snap.VRRP) == 0 {
		return true
	}
	for _, v := range snap.VRRP {
		if v.Role() != routeros.RoleBackup {
			return true
		}
	}
	return false
}

func renderApply(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — %s", m.pair.Name, m.apply.kind.title())))
	b.WriteString("\n\n")

	a := m.apply
	var header, body, footer []string
	say := func(style lipgloss.Style, text string) {
		header = append(header, wrapLines("", text, style.Render, m.width, 2)...)
	}

	switch a.stage {
	case applyIdle:
		switch {
		case a.err != nil:
			say(styleDown, "planning failed: "+a.err.Error())
			say(styleMuted, "press r to retry")
		case m.selectedCount() == 0:
			say(styleMuted, "no hunks selected — on the Drift screen (2), select hunks with space, or a whole section with a (A→B) / b (B→A)")
		default:
			say(styleMuted, "press r to build the plan")
		}
	case applyPlanning:
		say(styleMuted, "reading both routers and planning...")
	default:
		header = append(header, planSummary(a.plan))
		body = renderPlanOps(a, m.width)
		footer = applyFooter(m)
	}
	if a.notice != "" {
		say(styleDegraded, a.notice)
	}

	for _, l := range header {
		b.WriteString(l + "\n")
	}
	if len(body) > 0 {
		b.WriteString("\n")
		for _, l := range scrollWindow(body, a.scroll, applyBodyHeight(m.height, len(header), len(footer))) {
			b.WriteString(l + "\n")
		}
	}
	if len(footer) > 0 {
		b.WriteString("\n")
		for _, l := range footer {
			b.WriteString(l + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(styleStatusBar.Render(statusLine(m, applyHint(a.stage))))
	return b.String()
}

func planSummary(p plan.Plan) string {
	if p.Empty() {
		return styleMuted.Render("nothing to write")
	}
	targets := p.Targets()
	for i, t := range targets {
		targets[i] = strings.ToUpper(t)
	}
	return fmt.Sprintf("Dry run: %d operation(s) on router %s", len(p.Ops), strings.Join(targets, " and "))
}

// renderPlanOps lists every op (project.md §5.4: the dry run lists every
// REST operation), with a progress marker once a run has started, then any
// skipped hunks. Ops, notes and reasons wrap to width rather than run off
// the screen; the screen scrolls the result.
func renderPlanOps(a applyState, width int) []string {
	var lines []string
	current := ""
	for i, op := range a.plan.Ops {
		if op.Router != current {
			current = op.Router
			lines = append(lines, styleTitle.Render("Router "+strings.ToUpper(current)))
		}
		marker := "  "
		if i < len(a.status) {
			switch a.status[i] {
			case opRunning:
				marker = styleDegraded.Render("… ")
			case opDone:
				marker = styleReady.Render("✓ ")
			case opFailed:
				marker = styleDown.Render("✗ ")
			}
		}
		lines = append(lines, wrapLines(fmt.Sprintf("%s%2d. ", marker, i+1), op.String(), plain, width, opIndent)...)
		if op.Note != "" {
			lines = append(lines, wrapLines(strings.Repeat(" ", opIndent+1), op.Note, styleMuted.Render, width, opIndent+1)...)
		}
	}
	if len(a.plan.Warnings) > 0 {
		lines = append(lines, styleDegraded.Render("Warnings"))
		for _, w := range a.plan.Warnings {
			lines = append(lines, wrapLines("  ", w.String(), styleMuted.Render, width, 4)...)
		}
	}
	if len(a.plan.Skipped) > 0 {
		lines = append(lines, styleDegraded.Render("Skipped"))
		for _, s := range a.plan.Skipped {
			lines = append(lines, wrapPair("  ", s.Where()+":", s.Reason, styleMuted.Render, width, 4)...)
		}
	}
	return lines
}

// opIndent is where a wrapped op continues: under the op after its
// progress marker and number.
const opIndent = 6

// applyFooter is what the screen shows under the plan for its stage,
// wrapped to the terminal's width.
func applyFooter(m Model) []string {
	var lines []string
	for _, l := range applyFooterText(m) {
		lines = append(lines, wrapLines("", l.text, l.style.Render, m.width, 2)...)
	}
	if m.apply.stage == applyDone {
		lines = append(lines, renderApplyResult(m.apply, m.width)...)
	}
	return lines
}

// footerLine is one line of applyFooterText, before wrapping.
type footerLine struct {
	style lipgloss.Style
	text  string
}

func applyFooterText(m Model) []footerLine {
	a := m.apply
	styleNone := lipgloss.NewStyle()
	switch a.stage {
	case applyReview:
		if a.plan.Empty() {
			return []footerLine{{styleMuted, "nothing to apply — press r to re-plan"}}
		}
		if !m.writeMode {
			return []footerLine{{styleDown, "read-only — restart with -write to apply this plan"}}
		}
		lines := []footerLine{{styleNone, "press y to apply, r to re-plan"}}
		if masters := masterTargets(m, a.plan.Targets()); len(masters) > 0 {
			lines = append(lines, footerLine{styleDegraded, fmt.Sprintf("router %s is the current VRRP master (or its state is unknown): a second confirmation will be required", upperJoin(masters))})
		}
		return lines
	case applyConfirmMaster:
		return []footerLine{
			{styleDown, fmt.Sprintf("‼ this plan writes to router %s, the current VRRP master (or VRRP state unknown).", upperJoin(masterTargets(m, a.plan.Targets())))},
			{styleDown, "  press Y (shift+y) to write to it anyway, n/esc to cancel"},
		}
	case applyRechecking:
		return []footerLine{{styleMuted, "re-reading both routers to confirm the plan is still current..."}}
	case applyRunning:
		return []footerLine{{styleMuted, fmt.Sprintf("applying %d/%d...", a.next+1, len(a.plan.Ops))}}
	case applyVerifying:
		lines := []footerLine{{styleMuted, "verifying: re-running drift detection..."}}
		if a.err != nil {
			lines = append([]footerLine{{styleDown, "apply stopped: " + a.err.Error()}}, lines...)
		}
		return lines
	}
	return nil
}

// renderApplyResult reports the run's outcome and the residual drift of the
// sections it touched, wrapped to width.
func renderApplyResult(a applyState, width int) []string {
	var lines []string
	done := 0
	for _, s := range a.status {
		if s == opDone {
			done++
		}
	}
	if a.err != nil {
		lines = append(lines, wrapLines("", fmt.Sprintf("apply stopped after %d/%d op(s): %s", done, len(a.plan.Ops), a.err.Error()), styleDown.Render, width, 2)...)
	} else {
		lines = append(lines, styleReady.Render(fmt.Sprintf("applied %d/%d op(s)", done, len(a.plan.Ops))))
	}

	if a.verifyErr != nil {
		lines = append(lines, wrapLines("", "verify error: "+a.verifyErr.Error(), styleDown.Render, width, 2)...)
	}
	if a.kind != applySync {
		return append(lines, renderRuntimeVerify(a, width)...)
	}
	sections := a.plan.Sections()
	sort.Strings(sections)
	residual := 0
	for _, section := range sections {
		sd, ok := a.residual[section]
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("  %-28s %s", section, styleDown.Render("not verified")))
		case sd.Clean():
			lines = append(lines, fmt.Sprintf("  %-28s %s", section, styleReady.Render("clean")))
		default:
			residual += sd.Count()
			ids := make([]string, 0, sd.Count())
			for _, h := range sd.Hunks {
				ids = append(ids, h.Identity)
			}
			for _, o := range sd.Order {
				ids = append(ids, plan.OrderRef(o).String())
			}
			prefix := fmt.Sprintf("  %-28s %s ", section, styleDegraded.Render(fmt.Sprintf("%d residual", sd.Count())))
			lines = append(lines, wrapLines(prefix, strings.Join(ids, ", "), styleMuted.Render, width, 4)...)
		}
	}
	if residual > 0 {
		lines = append(lines, styleMuted.Render("residual differences remain — see the Drift screen (2)"))
	}
	return lines
}

// renderRuntimeVerify reports the re-verification after a Runtime deploy
// or remove: every object ok, or every object gone (or left alone as not
// mtha's), respectively.
func renderRuntimeVerify(a applyState, width int) []string {
	var left []string
	for _, router := range []string{"a", "b"} {
		for _, it := range a.runtimeStatus[router] {
			done := it.State == runtime.StateOK
			if a.kind == applyRuntimeRemove {
				done = it.State == runtime.StateMissing || it.State == runtime.StateConflict
			}
			if !done {
				left = append(left, fmt.Sprintf("router %s: %s %s", strings.ToUpper(router), it.Label, it.State))
			}
		}
	}
	if len(left) == 0 {
		if a.verifyErr != nil {
			return nil
		}
		return []string{styleReady.Render("verified — see the Runtime screen (3)")}
	}
	lines := []string{styleDegraded.Render(fmt.Sprintf("%d object(s) not as intended:", len(left)))}
	for _, l := range left {
		lines = append(lines, wrapLines("  ", l, plain, width, 4)...)
	}
	return append(lines, styleMuted.Render("see the Runtime screen (3)"))
}

func applyHint(stage applyStage) string {
	switch stage {
	case applyReview:
		return "y: apply, r: re-plan, j/k: scroll"
	case applyConfirmMaster:
		return "Y: write to master, n/esc: cancel"
	case applyIdle, applyDone:
		return "r: re-plan, j/k: scroll"
	default:
		return "working..."
	}
}

// applyBodyHeight is how many plan lines fit on screen alongside the title,
// header, footer and status bar, so the plan scrolls instead of pushing
// the confirmation prompt off an 80×24 terminal (project.md §6).
func applyBodyHeight(termHeight, headerLines, footerLines int) int {
	if termHeight <= 0 {
		return 1 << 30 // no size yet: show everything
	}
	// title + blank, header, blank, body, blank, footer, blank, status bar
	h := termHeight - (2 + headerLines + 1 + 1 + footerLines + 1 + 1)
	if h < 3 {
		h = 3
	}
	return h
}

// scrollWindow returns at most height lines of lines starting at offset
// (clamped), with a marker line when more lines are hidden below.
func scrollWindow(lines []string, offset, height int) []string {
	if len(lines) <= height {
		return lines
	}
	if max := len(lines) - height; offset > max {
		offset = max
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + height
	window := append([]string{}, lines[offset:end]...)
	if end < len(lines) {
		window[len(window)-1] = styleMuted.Render(fmt.Sprintf("  … %d more line(s) — j/k to scroll", len(lines)-end+1))
	}
	return window
}

func upperJoin(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.ToUpper(k)
	}
	return strings.Join(out, " and ")
}
