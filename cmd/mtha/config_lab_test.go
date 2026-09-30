//go:build lab

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"mtha/internal/config"
	"mtha/internal/labtest"
)

// -init's sample, with only its two routers pointed at the lab, is a pair
// file a following run accepts as it stands: it loads without warnings, both
// routers are polled, and every sync section it lists is one a real RouterOS
// 7 device answers for.
func TestLabInitSampleRunsAgainstLab(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	path := filepath.Join(t.TempDir(), "mtha", "pairs.yaml")

	out, err := exec.Command(lab.Binary(), "-init", "-config", path).CombinedOutput()
	if err != nil {
		t.Fatalf("mtha -init: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "MTHA_<PAIR>_<ROUTER>_PASSWORD") {
		t.Errorf("-init does not say how to supply passwords:\n%s", out)
	}
	pointAtLab(t, lab, path)

	// The sample's pair is "core", so it reads MTHA_CORE_*.
	env := []string{"MTHA_CORE_A_PASSWORD=" + lab.Password(), "MTHA_CORE_B_PASSWORD=" + lab.Password()}
	tui, err := lab.RunTUIEnv(30*time.Second, env, []string{"-config", path},
		labtest.Input{Until: "✓ Both routers reachable", Keys: "q"})
	if err != nil {
		t.Fatalf("mtha on the -init sample: %v\noutput:\n%s", err, tui)
	}
	for _, unwanted := range []string{"warning:", "unreachable"} {
		if strings.Contains(tui, unwanted) {
			t.Errorf("run on the -init sample shows %q:\n%s", unwanted, tui)
		}
	}
	if !strings.Contains(tui, "mtha — core") {
		t.Errorf("run on the -init sample did not select its only pair:\n%s", tui)
	}

	// No -pair: a single-pair file needs none.
	for k, v := range envMap(env) {
		t.Setenv(k, v)
	}
	d := labtest.Drive(t, startModel(t, "-config", path))
	d.Send(labtest.Key("2"))
	s := screen(driftFetched(d))
	for _, unwanted := range []string{"error:", "fetch failed"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("drift on the -init sample's sections shows %q:\n%s", unwanted, s)
		}
	}
	sample, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Warnings) > 0 {
		t.Errorf("the -init sample loads with warnings: %q", sample.Warnings)
	}
	for section, status := range sectionStatuses(s) {
		t.Logf("%s: %s", section, status)
	}
	if got, want := len(sectionStatuses(s)), len(sample.Pairs[0].Sync.Sections); got != want {
		t.Errorf("drift lists %d sections, the sample configures %d:\n%s", got, want, s)
	}
}

// pointAtLab rewrites the routers of the sample at path to the lab's, in
// place, leaving every other line of the sample as -init wrote it.
func pointAtLab(t *testing.T, lab *labtest.Lab, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	pairs := child(doc.Content[0], "pairs")
	if pairs == nil || len(pairs.Content) != 1 {
		t.Fatalf("-init sample has no single pair:\n%s", src)
	}
	routers := child(pairs.Content[0], "routers")
	for _, key := range []string{"a", "b"} {
		r := child(routers, key)
		if r == nil {
			t.Fatalf("-init sample has no router %q:\n%s", key, src)
		}
		if err := r.Encode(lab.Pair.Routers[key]); err != nil {
			t.Fatal(err)
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// A file with two pairs and no -pair is refused before anything is polled,
// with an error naming the flag; naming either pair with -pair then runs it.
func TestLabTwoPairsNeedPairFlag(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	path := writePairFile(t, *lab.Pair, labPairAs(lab, cliPair, nil))
	env := append(lab.Env(), cliEnv(lab)...)

	out, err := lab.RunTUIEnv(20*time.Second, env, []string{"-config", path})
	if err == nil {
		t.Fatalf("mtha exited 0 on two pairs without -pair:\n%s", out)
	}
	if strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("mtha hung on two pairs without -pair: %v\n%s", err, out)
	}
	if !strings.Contains(out, "multiple pairs defined in "+path) || !strings.Contains(out, "pass -pair") {
		t.Errorf("error does not name the file and the -pair flag:\n%s", out)
	}
	if strings.Contains(out, "Router A") {
		t.Errorf("mtha started the TUI without choosing a pair:\n%s", out)
	}
	assertNoPanic(t, out)

	out, err = lab.RunTUIEnv(30*time.Second, env, []string{"-config", path, "-pair", cliPair},
		labtest.Input{Until: "✓ Both routers reachable", Keys: "q"})
	if err != nil {
		t.Fatalf("mtha -pair %s: %v\noutput:\n%s", cliPair, err, out)
	}
	if !strings.Contains(out, "mtha — "+cliPair) {
		t.Errorf("-pair %s did not select that pair:\n%s", cliPair, out)
	}
}

// missingSection is a section the lab's CHR does not have: /container comes
// with the container package, which the lab does not install, so RouterOS
// answers 400 "no such command or directory".
const missingSection = "container/mounts"

// A configured section the device does not have fails on its own: drift
// still reports every other section (clean, as the lab is at baseline), and
// names the missing one rather than failing the whole fetch.
func TestLabMissingSectionGivesPartialDrift(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	pair := labPairAs(lab, cliPair, func(p *config.Pair) {
		// In the middle, so sections either side of it are fetched too.
		mid := len(p.Sync.Sections) / 2
		p.Sync.Sections = append(p.Sync.Sections[:mid], append([]string{missingSection}, p.Sync.Sections[mid:]...)...)
	})
	path := writePairFile(t, pair)
	for k, v := range envMap(cliEnv(lab)) {
		t.Setenv(k, v)
	}

	d := labtest.Drive(t, startModel(t, "-config", path))
	d.Send(labtest.Key("2"))
	s := screen(driftFetched(d))

	if !strings.Contains(s, "error: fetch "+missingSection+" from router a") || !strings.Contains(s, "no such command") {
		t.Errorf("drift does not name the missing section and the router's refusal:\n%s", s)
	}
	statuses := sectionStatuses(s)
	for _, section := range pair.Sync.Sections {
		want := "clean"
		if section == missingSection {
			want = "fetch failed"
		}
		if got, ok := statuses[section]; !ok {
			t.Errorf("drift does not list %s:\n%s", section, s)
		} else if got != want {
			t.Errorf("%s: %q, want %q", section, got, want)
		}
	}
}

// A router unreachable when mtha starts is not fatal and not permanent:
// startup succeeds, the dashboard shows it unreachable, and once it answers
// again its poller picks it up with no restart and the checks the pollers
// feed (reachability, versions, one master) go green again.
//
// Router a's container is frozen with docker pause, so its REST port stops
// answering without the guest's configuration changing; the thaw leaves
// VRRP re-electing, so this is a writing test and restores the golden
// backup afterwards.
func TestLabRouterRecoversAfterStartupOutage(t *testing.T) {
	lab := labtest.New(t)
	container := lab.Container("a")
	docker := func(args ...string) error {
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Logf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return err
	}
	if err := docker("pause", container); err != nil {
		t.Fatal("pause router a")
	}
	// Registered after New's reset, so it runs first: the restore needs a
	// thawed router.
	paused := true
	t.Cleanup(func() {
		if paused {
			docker("unpause", container)
		}
	})

	for k, v := range envMap(lab.Env()) {
		t.Setenv(k, v)
	}
	d := labtest.Drive(t, startModel(t, "-config", lab.PairFile, "-pair", labtest.PairName))

	s := screen(d.Until("router a polled unreachable", time.Minute, func(m tea.Model) bool {
		return strings.Contains(screen(m), "✗ Both routers reachable (router a unreachable)")
	}))
	if !strings.Contains(s, "vrrp vrrp-lan") {
		t.Errorf("router b was not polled while a was down:\n%s", s)
	}

	if err := docker("unpause", container); err != nil {
		t.Fatal("unpause router a")
	}
	paused = false

	green := []string{
		"✓ Both routers reachable",
		"✓ RouterOS versions match",
		"✓ Exactly one master per VRRP instance",
	}
	s = screen(d.Until("readiness green again", 2*time.Minute, func(m tea.Model) bool {
		s := screen(m)
		for _, want := range green {
			if !strings.Contains(s, want) {
				return false
			}
		}
		return true
	}))
	if strings.Contains(s, "unreachable") {
		t.Errorf("a router still renders unreachable after recovery:\n%s", s)
	}
}

// driftFetched waits for the drift screen's fetch to finish.
func driftFetched(d *labtest.Driver[tea.Model]) tea.Model {
	return d.Until("drift fetched", time.Minute, func(m tea.Model) bool {
		s := screen(m)
		return strings.Contains(s, "Sections") && !strings.Contains(s, "fetching drift")
	})
}

// sectionStatuses parses the drift screen's section list ("> section
// status" lines under "Sections") into section → status.
func sectionStatuses(s string) map[string]string {
	out := map[string]string{}
	_, list, ok := strings.Cut(s, "Sections\n")
	if !ok {
		return out
	}
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), ">"))
		if len(fields) < 2 {
			break
		}
		out[fields[0]] = strings.Join(fields[1:], " ")
	}
	return out
}
