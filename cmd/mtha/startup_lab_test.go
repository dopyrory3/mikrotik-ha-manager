//go:build lab

package main

import (
	"strings"
	"testing"
	"time"

	"mtha/internal/labtest"
)

// The scripted smoke test: the real binary, started from the lab pair file
// in a pty, polls both routers and renders the pair — which a model-driven
// test cannot show, since it bypasses flag parsing, credential resolution
// and the terminal. It never writes (no -write), so it runs ReadOnly and
// skips the reset.
func TestLabStartupRendersPair(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())

	out, err := lab.RunTUI(30*time.Second, []string{"-config", lab.PairFile, "-pair", labtest.PairName},
		labtest.Input{Until: "✓ Exactly one master", Keys: "q"})
	if err != nil {
		t.Fatalf("mtha: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{
		"mtha — lab",
		"Router A", "Router B",
		"reachable",
		"✓ Both routers reachable",
		"vrrp vrrp-lan", "master", "backup",
		"read-only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("a lab router rendered unreachable:\n%s", out)
	}
}
