package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/diff"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// The Apply screen (project.md §7.1 screen 4, milestone 3) turns the hunks
// selected on the Drift screen into a plan, shows it as a dry run, and —
// only with -write and explicit confirmation — runs it op by op, then
// re-runs drift detection on the touched sections and reports what still
// differs (§5.4).
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

	residual  map[string]diff.SectionDiff // post-apply drift of the touched sections
	verifyErr error

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

// applyVerifyMsg carries the post-apply drift re-check.
type applyVerifyMsg struct {
	result driftResultMsg
}

func (m Model) enterApplyScreen() (tea.Model, tea.Cmd) {
	m.screen = screenApply
	if m.apply.running() || m.apply.stage == applyDone {
		return m, nil
	}
	return m.startApplyPlan(false)
}

// startApplyPlan reads every section with a selection from both routers and
// builds the plan from those fresh reads.
func (m Model) startApplyPlan(recheck bool) (tea.Model, tea.Cmd) {
	if m.selectedCount() == 0 {
		m.apply = applyState{stage: applyIdle}
		return m, nil
	}

	if recheck {
		m.apply.stage = applyRechecking
	} else {
		m.apply = applyState{
			stage:      applyPlanning,
			backupName: "mtha-pre-apply-" + time.Now().Format("20060102-150405"),
		}
	}

	ctx := m.runtimeCtx()
	clientA, clientB := m.pollers["a"].Client, m.pollers["b"].Client
	sectionOrder := m.driftSections
	exempt := m.pair.Sync.Exempt
	choices := m.selectionSnapshot()
	backupName := m.apply.backupName

	return m, func() tea.Msg {
		p, err := buildApplyPlan(ctx, clientA, clientB, sectionOrder, choices, exempt, backupName)
		return applyPlanMsg{plan: p, err: err, recheck: recheck}
	}
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
func buildApplyPlan(ctx context.Context, clientA, clientB *routeros.Client, sectionOrder []string, choices map[string]map[plan.HunkRef]plan.Direction, exempt []string, backupName string) (plan.Plan, error) {
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
	return plan.Build(inputs, plan.Options{Exempt: exempt, BackupName: backupName}), nil
}

// handleApplyMsg is Update's entry point for every Apply-screen message.
func (m Model) handleApplyMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case applyPlanMsg:
		if msg.err != nil {
			m.apply = applyState{stage: applyIdle, err: msg.err}
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
		if m.apply.scroll < len(renderPlanOps(m.apply))-1 {
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

// masterTargets returns which of targets currently hold VRRP master, or
// whose VRRP state can't be confirmed (not polled yet, unreachable, or the
// VRRP read failed) — both require the second confirmation (project.md
// §5.4, §7.3), since "unknown" must not be treated as "safe".
func masterTargets(m Model, targets []string) []string {
	var out []string
	for _, router := range targets {
		key := poll.RouterKey(router)
		snap, ok := m.snapshots[key]
		if !ok || !snap.Reachable() || snap.VRRPErr != nil || currentMaster(m, router) {
			out = append(out, router)
		}
	}
	return out
}

func renderApply(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — apply", m.pair.Name)))
	b.WriteString("\n\n")

	a := m.apply
	var header, body, footer []string

	switch a.stage {
	case applyIdle:
		switch {
		case a.err != nil:
			header = append(header, styleDown.Render("planning failed: "+a.err.Error()), styleMuted.Render("press r to retry"))
		case m.selectedCount() == 0:
			header = append(header, styleMuted.Render("no hunks selected — on the Drift screen (2), select hunks with space, or a whole section with a (A→B) / b (B→A)"))
		default:
			header = append(header, styleMuted.Render("press r to build the plan"))
		}
	case applyPlanning:
		header = append(header, styleMuted.Render("reading both routers and planning..."))
	default:
		header = append(header, planSummary(a.plan))
		body = renderPlanOps(a)
		footer = applyFooter(m)
	}
	if a.notice != "" {
		header = append(header, styleDegraded.Render(a.notice))
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
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | %s | apply | %s ", m.pair.Name, modeLabel(m.writeMode), applyHint(a.stage))))
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
// skipped hunks.
func renderPlanOps(a applyState) []string {
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
		lines = append(lines, fmt.Sprintf("%s%2d. %s", marker, i+1, op))
		if op.Note != "" {
			lines = append(lines, styleMuted.Render("       "+op.Note))
		}
	}
	if len(a.plan.Skipped) > 0 {
		lines = append(lines, styleDegraded.Render("Skipped"))
		for _, s := range a.plan.Skipped {
			lines = append(lines, fmt.Sprintf("  %s %s [%s]: %s", s.Section, s.Ref, s.Direction, styleMuted.Render(s.Reason)))
		}
	}
	return lines
}

func applyFooter(m Model) []string {
	a := m.apply
	switch a.stage {
	case applyReview:
		if a.plan.Empty() {
			return []string{styleMuted.Render("nothing to apply — press r to re-plan")}
		}
		if !m.writeMode {
			return []string{styleDown.Render("read-only — restart with -write to apply this plan")}
		}
		lines := []string{"press y to apply, r to re-plan"}
		if masters := masterTargets(m, a.plan.Targets()); len(masters) > 0 {
			lines = append(lines, styleDegraded.Render(fmt.Sprintf("router %s is the current VRRP master (or its state is unknown): a second confirmation will be required", upperJoin(masters))))
		}
		return lines
	case applyConfirmMaster:
		return []string{
			styleDown.Render(fmt.Sprintf("‼ this plan writes to router %s, the current VRRP master (or VRRP state unknown).", upperJoin(masterTargets(m, a.plan.Targets())))),
			styleDown.Render("  press Y (shift+y) to write to it anyway, n/esc to cancel"),
		}
	case applyRechecking:
		return []string{styleMuted.Render("re-reading both routers to confirm the plan is still current...")}
	case applyRunning:
		return []string{styleMuted.Render(fmt.Sprintf("applying %d/%d...", a.next+1, len(a.plan.Ops)))}
	case applyVerifying:
		lines := []string{styleMuted.Render("verifying: re-running drift detection...")}
		if a.err != nil {
			lines = append([]string{styleDown.Render("apply stopped: " + a.err.Error())}, lines...)
		}
		return lines
	case applyDone:
		return renderApplyResult(a)
	}
	return nil
}

// renderApplyResult reports the run's outcome and the residual drift of the
// sections it touched.
func renderApplyResult(a applyState) []string {
	var lines []string
	done := 0
	for _, s := range a.status {
		if s == opDone {
			done++
		}
	}
	if a.err != nil {
		lines = append(lines, styleDown.Render(fmt.Sprintf("apply stopped after %d/%d op(s): %s", done, len(a.plan.Ops), a.err.Error())))
	} else {
		lines = append(lines, styleReady.Render(fmt.Sprintf("applied %d/%d op(s)", done, len(a.plan.Ops))))
	}

	if a.verifyErr != nil {
		lines = append(lines, styleDown.Render("verify error: "+a.verifyErr.Error()))
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
			residual += len(sd.Hunks)
			ids := make([]string, 0, len(sd.Hunks))
			for _, h := range sd.Hunks {
				ids = append(ids, h.Identity)
			}
			lines = append(lines, fmt.Sprintf("  %-28s %s %s", section,
				styleDegraded.Render(fmt.Sprintf("%d residual", len(sd.Hunks))), styleMuted.Render(strings.Join(ids, ", "))))
		}
	}
	if residual > 0 {
		lines = append(lines, styleMuted.Render("residual differences remain — see the Drift screen (2)"))
	}
	return lines
}

func applyHint(stage applyStage) string {
	switch stage {
	case applyReview:
		return "y: apply, r: re-plan, j/k: scroll, q: quit"
	case applyConfirmMaster:
		return "Y: write to master, n/esc: cancel"
	case applyIdle, applyDone:
		return "r: re-plan, j/k: scroll, 2: drift, q: quit"
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
