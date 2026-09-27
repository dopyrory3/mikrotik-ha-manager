//go:build lab

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/poll"
)

// The startup, credential and configuration paths against the real lab
// rather than fakes: what an operator sees when a password, a certificate or
// the pair file is wrong. Every pair file these tests write is the lab pair
// (already checked against the instance's endpoints by labtest.New) under
// another name or with one field changed, so they reach no other router.

// cliPair is the pair name the credential tests use. Its hyphen is mapped
// to an underscore in the variable name (MTHA_LAB_CLI_A_PASSWORD), and a
// name other than the harness's "lab" keeps its variables from colliding
// with the ones RunTUI and the caller's shell provide.
const cliPair = "lab-cli"

// cliEnv is the right password for both routers of cliPair. The names are
// spelled out, not derived, so a change to the mapping fails here.
func cliEnv(lab *labtest.Lab) []string {
	return []string{
		"MTHA_LAB_CLI_A_PASSWORD=" + lab.Password(),
		"MTHA_LAB_CLI_B_PASSWORD=" + lab.Password(),
	}
}

// Wrong password: the run does not fail or hang at startup — the router is
// rendered unreachable with RouterOS's 401, the other router (right
// password) is polled normally, and the program still quits on q.
func TestLabWrongPasswordIsReportedNotFatal(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	path := writePairFile(t, labPairAs(lab, cliPair, nil))
	env := []string{
		"MTHA_LAB_CLI_A_PASSWORD=not-" + lab.Password(),
		"MTHA_LAB_CLI_B_PASSWORD=" + lab.Password(),
	}

	out, err := lab.RunTUIEnv(30*time.Second, env, []string{"-config", path},
		labtest.Input{Until: "(router a unreachable)", Keys: "q"})
	if err != nil {
		t.Fatalf("mtha: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"mtha — lab-cli", "unreachable", "status 401", "vrrp vrrp-lan"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	assertNoPanic(t, out)

	// The same pair file in-process: exactly router a is refused, with the
	// router's 401, and b answers.
	for k, v := range envMap(env) {
		t.Setenv(k, v)
	}
	snaps := firstSnapshots(t, loadPair(t, path, cliPair))
	if err := snaps["a"].Err; err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Errorf("router a with the wrong password: err = %v, want a 401", err)
	}
	if err := snaps["b"].Err; err != nil {
		t.Errorf("router b with the right password: %v", err)
	}
}

// A missing password variable stops mtha before it polls anything, naming
// the exact variable to set — with the pair name's hyphen mapped to an
// underscore — and exits non-zero rather than hanging.
func TestLabMissingPasswordNamesVariable(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	path := writePairFile(t, labPairAs(lab, cliPair, nil))

	for _, tc := range []struct {
		set, missing string
	}{
		{set: "MTHA_LAB_CLI_B_PASSWORD", missing: "MTHA_LAB_CLI_A_PASSWORD"},
		{set: "MTHA_LAB_CLI_A_PASSWORD", missing: "MTHA_LAB_CLI_B_PASSWORD"},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			// The other router's variable is set, so the error can only
			// be about this one.
			out, err := lab.RunTUIEnv(20*time.Second, []string{tc.set + "=" + lab.Password()}, []string{"-config", path})
			if err == nil {
				t.Fatalf("mtha exited 0 without %s:\n%s", tc.missing, out)
			}
			if strings.Contains(err.Error(), "did not exit") {
				t.Fatalf("mtha hung without %s: %v\n%s", tc.missing, err, out)
			}
			if !strings.Contains(out, "set "+tc.missing) {
				t.Errorf("output does not name %s:\n%s", tc.missing, out)
			}
			if strings.Contains(out, "MTHA_LAB-CLI") {
				t.Errorf("output names the variable with the pair's hyphen:\n%s", out)
			}
			if strings.Contains(out, "Router A") {
				t.Errorf("mtha started the TUI without a password:\n%s", out)
			}
			assertNoPanic(t, out)
		})
	}
}

// insecure_tls is the only thing between mtha and a router's self-signed
// certificate, and only a real router has one: with the flag off, the lab's
// certificate fails verification (and the router renders unreachable,
// saying so); with it on, the same pair file otherwise unchanged is polled.
func TestLabInsecureTLSFlagDecidesCertificateCheck(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	for k, v := range envMap(cliEnv(lab)) {
		t.Setenv(k, v)
	}

	withTLS := func(insecure bool) config.Pair {
		return labPairAs(lab, cliPair, func(p *config.Pair) {
			for key, r := range p.Routers {
				r.InsecureTLS = insecure
				p.Routers[key] = r
			}
		})
	}

	t.Run("false", func(t *testing.T) {
		path := writePairFile(t, withTLS(false))
		snaps := firstSnapshots(t, loadPair(t, path, cliPair))
		for key, snap := range snaps {
			if !isCertificateError(snap.Err) {
				t.Errorf("router %s with insecure_tls: false: err = %v, want a certificate verification failure", key, snap.Err)
			}
		}

		out, err := lab.RunTUIEnv(30*time.Second, cliEnv(lab), []string{"-config", path},
			labtest.Input{Until: "(router a unreachable)", Keys: "q"})
		if err != nil {
			t.Fatalf("mtha: %v\noutput:\n%s", err, out)
		}
		if !strings.Contains(out, "certificate") {
			t.Errorf("the dashboard does not say the certificate was refused:\n%s", out)
		}
		if strings.Contains(out, "✓ Both routers reachable") {
			t.Errorf("a router was polled despite its certificate failing verification:\n%s", out)
		}
		assertNoPanic(t, out)
	})

	t.Run("true", func(t *testing.T) {
		snaps := firstSnapshots(t, loadPair(t, writePairFile(t, withTLS(true)), cliPair))
		for key, snap := range snaps {
			if snap.Err != nil || snap.Resource == nil || !strings.HasPrefix(snap.Resource.BoardName, "CHR") {
				t.Errorf("router %s with insecure_tls: true: err = %v, resource = %+v", key, snap.Err, snap.Resource)
			}
		}
	})
}

// isCertificateError reports whether err is the TLS handshake refusing the
// peer's certificate (unknown authority or a name mismatch), as opposed to
// any other connection failure.
func isCertificateError(err error) bool {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname)
}

// labPairAs is a copy of the lab pair named name, with edit (if any)
// applied. Only names and flags change, never an endpoint.
func labPairAs(lab *labtest.Lab, name string, edit func(*config.Pair)) config.Pair {
	p := *lab.Pair
	p.Name = name
	p.Routers = maps.Clone(lab.Pair.Routers)
	p.Sync.Sections = slices.Clone(lab.Pair.Sync.Sections)
	if edit != nil {
		edit(&p)
	}
	return p
}

// writePairFile writes pairs as a pair file in a temporary directory.
func writePairFile(t *testing.T, pairs ...config.Pair) string {
	t.Helper()
	out, err := yaml.Marshal(config.File{Pairs: pairs})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pairs.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadPair(t *testing.T, path, name string) *config.Pair {
	t.Helper()
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.Pair(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// firstSnapshots builds pair's pollers exactly as mtha does (credentials
// from the environment, the pair's TLS setting) and returns each router's
// first snapshot. Any failure must arrive as a snapshot within the client
// timeout, not as a hang.
func firstSnapshots(t *testing.T, pair *config.Pair) map[string]poll.Snapshot {
	t.Helper()
	pollers, err := buildPollers(pair)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snaps := make(map[string]poll.Snapshot, len(pollers))
	for key, p := range pollers {
		go p.Run(ctx)
		select {
		case snaps[string(key)] = <-p.C:
		case <-time.After(30 * time.Second):
			t.Fatalf("router %s: no snapshot within 30s", key)
		}
	}
	return snaps
}

// startModel runs mtha's own startup (flag parsing, pair selection,
// credential resolution, pollers) in-process and returns the root model,
// for driving with labtest.Drive where a pty run cannot show enough.
func startModel(t *testing.T, args ...string) tea.Model {
	t.Helper()
	opts, err := parseFlags(flag.NewFlagSet("mtha", flag.ContinueOnError), args, filepath.Join(t.TempDir(), "unused.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	model, err := setup(opts, &out)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	if out.Len() > 0 {
		// testlab/pairs.yaml lists ip/firewall/filter first on purpose,
		// which warns; tests that care assert on warnings themselves.
		t.Logf("startup printed:\n%s", out.String())
	}
	return model
}

func screen(m tea.Model) string { return labtest.StripANSI(m.View()) }

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func assertNoPanic(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "panic:") || strings.Contains(out, "goroutine ") {
		t.Errorf("mtha panicked:\n%s", out)
	}
}
