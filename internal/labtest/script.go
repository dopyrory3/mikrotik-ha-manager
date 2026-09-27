//go:build lab

package labtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The scripted layer: runs of the real mtha binary in a pty, for what the
// model-driven tests cannot see — flags, startup sequencing and rendering to
// a terminal. Keep these few; assert on device state with the model-driven
// harness instead wherever possible.

// Input is keystrokes typed into a TUI run once the screen shows Until
// (escape sequences stripped; empty types at once). Waiting on the screen
// rather than a delay keeps runs independent of how fast the routers answer.
type Input struct {
	Until string
	Keys  string
}

var binary struct {
	once sync.Once
	path string
	err  error
}

// Binary builds cmd/mtha once per test binary and returns its path.
func (l *Lab) Binary() string {
	l.t.Helper()
	binary.once.Do(func() {
		dir, err := os.MkdirTemp("", "mtha-lab-bin-")
		if err != nil {
			binary.err = err
			return
		}
		binary.path = filepath.Join(dir, "mtha")
		binary.err = run(l.root, nil, "go", "build", "-o", binary.path, "./cmd/mtha")
	})
	if binary.err != nil {
		l.t.Fatalf("build mtha: %v", binary.err)
	}
	return binary.path
}

// RunTUI runs mtha with args in an 80x24 pty (via script(1), from
// util-linux), with the lab passwords in its environment, types inputs in
// order, and waits for it to exit. It returns everything written to the
// terminal with escape sequences stripped: the renderer redraws only changed
// lines, so assert on substrings, not on a screen layout.
func (l *Lab) RunTUI(timeout time.Duration, args []string, inputs ...Input) (string, error) {
	l.t.Helper()
	return l.RunTUIEnv(timeout, l.Env(), args, inputs...)
}

// RunTUIEnv is RunTUI with env in place of the lab passwords, for runs that
// test credential resolution itself: a missing or wrong password, or a pair
// under another name. Any MTHA_*_PASSWORD inherited from the caller's
// environment is dropped, so the run sees exactly the passwords in env.
func (l *Lab) RunTUIEnv(timeout time.Duration, env []string, args []string, inputs ...Input) (string, error) {
	l.t.Helper()
	quoted := []string{shellQuote(l.Binary())}
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "script", "-qefc", "stty cols 80 rows 24; exec "+strings.Join(quoted, " "), "/dev/null")
	cmd.Dir = l.root
	cmd.Env = append(append(inheritedEnv(), "TERM=xterm-256color"), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	term := &terminal{in: stdin, changed: make(chan struct{}, 1)}
	cmd.Stdout = term
	cmd.Stderr = term
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start script(1): %w", err)
	}

	// stdin stays open until mtha exits: script(1) may end the session
	// when its input closes.
	typed := make(chan error, 1)
	go func() {
		mark := 0
		for _, in := range inputs {
			for {
				screen, n := term.since(mark)
				if strings.Contains(screen, in.Until) {
					mark = n
					break
				}
				select {
				case <-term.changed:
				case <-ctx.Done():
					typed <- fmt.Errorf("screen never showed %q", in.Until)
					return
				}
			}
			term.write(in.Keys)
		}
		typed <- nil
	}()
	err = cmd.Wait()
	stdin.Close()
	out, _ := term.since(0)
	if ctx.Err() != nil {
		err = fmt.Errorf("mtha did not exit within %s: %w", timeout, ctx.Err())
		if typeErr := <-typed; typeErr != nil {
			err = fmt.Errorf("%w (%v)", err, typeErr)
		}
	}
	return out, err
}

// inheritedEnv is the caller's environment less any mtha password.
func inheritedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "MTHA_") && strings.HasSuffix(name, "_PASSWORD") && name != PasswordEnv {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// terminal collects a run's output and answers the queries a real terminal
// would. Bubble Tea asks for the background colour (OSC 11) and the cursor
// position (CSI 6n) at startup and reads its input for several seconds
// waiting for replies, swallowing any key typed meanwhile; answering at once
// keeps typed keys going to the program.
type terminal struct {
	mu      sync.Mutex
	out     []byte
	replied map[string]int
	in      io.Writer
	changed chan struct{}
}

var terminalReplies = map[string]string{
	"\x1b]11;?": "\x1b]11;rgb:0000/0000/0000\x1b\\",
	"\x1b[6n":   "\x1b[1;1R",
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.out = append(t.out, p...)
	if t.replied == nil {
		t.replied = make(map[string]int)
	}
	var replies []string
	for query, reply := range terminalReplies {
		for n := strings.Count(string(t.out), query); t.replied[query] < n; t.replied[query]++ {
			replies = append(replies, reply)
		}
	}
	t.mu.Unlock()

	for _, r := range replies {
		t.write(r)
	}
	select {
	case t.changed <- struct{}{}:
	default:
	}
	return len(p), nil
}

// since returns the stripped output from byte offset mark, and the offset
// of its end.
func (t *terminal) since(mark int) (string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return StripANSI(string(t.out[mark:])), len(t.out)
}

var inputMu sync.Mutex

func (t *terminal) write(s string) {
	inputMu.Lock()
	defer inputMu.Unlock()
	io.WriteString(t.in, s)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Z0-9]|\x1b[=>]`)

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string {
	return ansi.ReplaceAllString(s, "")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
