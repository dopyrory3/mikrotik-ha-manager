//go:build lab

package labtest

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Driver runs a Bubble Tea model the way tea.Program does, minus the
// terminal: Init's and Update's commands run on their own goroutines and
// their messages are fed back through Update, but only on the test's
// goroutine, inside Send and Until. That makes it usable with real pollers,
// whose commands block until the next snapshot and so never "settle" — the
// synchronous drive() helper in internal/ui's unit tests would hang on them.
//
// tea.Batch is expanded; tea.Sequence is not supported (its message type is
// unexported), and mtha does not use it.
type Driver[M tea.Model] struct {
	t    testing.TB
	m    M
	msgs chan tea.Msg
	done chan struct{}
	quit bool
}

// Drive starts m (running its Init) and stops it when the test ends, by
// sending ctrl+c so the model cancels its pollers.
func Drive[M tea.Model](t testing.TB, m M) *Driver[M] {
	t.Helper()
	d := &Driver[M]{t: t, m: m, msgs: make(chan tea.Msg, 64), done: make(chan struct{})}
	t.Cleanup(d.stop)
	d.dispatch(m.Init())
	return d
}

// Model is the model as of the last message processed.
func (d *Driver[M]) Model() M { return d.m }

// Send delivers msgs to Update in order, as if typed or received.
func (d *Driver[M]) Send(msgs ...tea.Msg) {
	d.t.Helper()
	for _, msg := range msgs {
		d.handle(msg)
	}
}

// Until processes messages until cond holds, failing the test if it does
// not within timeout. what describes the condition for the failure message.
func (d *Driver[M]) Until(what string, timeout time.Duration, cond func(M) bool) M {
	d.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for !cond(d.m) {
		select {
		case msg := <-d.msgs:
			d.handle(msg)
		case <-deadline.C:
			d.t.Fatalf("timed out after %s waiting for %s; screen:\n%s", timeout, what, d.m.View())
		}
	}
	return d.m
}

func (d *Driver[M]) handle(msg tea.Msg) {
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, cmd := range msg {
			d.dispatch(cmd)
		}
	case tea.QuitMsg:
		d.quit = true
	default:
		next, cmd := d.m.Update(msg)
		d.m = next.(M)
		d.dispatch(cmd)
	}
}

func (d *Driver[M]) dispatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if msg == nil {
			return
		}
		select {
		case d.msgs <- msg:
		case <-d.done:
		}
	}()
}

func (d *Driver[M]) stop() {
	// Let already-finished commands land first (Init's own bookkeeping
	// among them), so the quit key finds something to cancel.
	for settled := false; !settled; {
		select {
		case msg := <-d.msgs:
			d.handle(msg)
		case <-time.After(200 * time.Millisecond):
			settled = true
		}
	}
	if !d.quit {
		d.handle(tea.KeyMsg{Type: tea.KeyCtrlC})
	}
	close(d.done)
}

// Key builds a key press: a name tea.KeyMsg.String() reports ("enter",
// "esc", "tab", "up", "down", " " for space, "ctrl+c") or literal runes.
func Key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case " ", "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}
