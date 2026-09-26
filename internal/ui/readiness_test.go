package ui

import (
	"testing"

	"mtha/internal/diff"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// The check labels are asserted literally: they are the §5.2 criteria shown
// to the operator, so a rename should fail here deliberately.

func snapshot(version, vrrpState, netwatchStatus string) poll.Snapshot {
	return poll.Snapshot{
		Reachable: true,
		Resource:  &routeros.SystemResource{Version: version},
		Identity:  &routeros.Identity{Name: "r"},
		VRRP:      []routeros.VRRPInstance{{Name: "vrrp-lan", State: vrrpState}},
		Netwatch:  []routeros.NetwatchEntry{{Host: "1.1.1.1", Status: netwatchStatus}},
	}
}

func cleanDrift() map[string]diff.SectionDiff {
	return map[string]diff.SectionDiff{"ip/service": {Section: "ip/service"}}
}

func check(t *testing.T, checks []Check, label string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("no check labelled %q in %+v", label, checks)
	return Check{}
}

func TestReadinessUnknownBeforeFirstPoll(t *testing.T) {
	verdict, checks := evaluateReadiness(poll.Snapshot{}, poll.Snapshot{}, false, false, nil, nil)
	if verdict != VerdictUnknown {
		t.Errorf("verdict = %v, want Unknown before any snapshot", verdict)
	}
	if c := check(t, checks, "Both routers reachable"); c.OK {
		t.Error("reachable check passed with no snapshots")
	}
}

// The Runtime check is hardcoded not-yet-implemented (milestone 4), so a
// pair that is otherwise perfect must still read Degraded rather than Ready.
// This test pins that known limitation and will need updating when milestone
// 4 lands.
func TestReadinessDegradedWhileRuntimeNotImplemented(t *testing.T) {
	verdict, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.15.3", "backup", "up"),
		true, true, cleanDrift(), nil,
	)

	if verdict != VerdictDegraded {
		t.Errorf("verdict = %v, want Degraded (runtime logic not yet implemented)", verdict)
	}
	for _, label := range []string{
		"Both routers reachable",
		"RouterOS versions match",
		"No unresolved drift in synced sections",
		"Exactly one master per VRRP instance",
		"Standby netwatch targets up",
	} {
		if c := check(t, checks, label); !c.OK {
			t.Errorf("check %q = false, want true when the pair is healthy", label)
		}
	}
	if c := check(t, checks, "Runtime logic present and identical on both routers"); c.OK {
		t.Error("runtime check passed; it is intentionally unimplemented until milestone 4")
	}
}

func TestReadinessVersionMismatch(t *testing.T) {
	verdict, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.16.0", "backup", "up"),
		true, true, cleanDrift(), nil,
	)

	if verdict != VerdictDegraded {
		t.Errorf("verdict = %v, want Degraded", verdict)
	}
	c := check(t, checks, "RouterOS versions match")
	if c.OK {
		t.Error("versions match check passed for 7.15.3 vs 7.16.0")
	}
	if c.Note != "7.15.3 vs 7.16.0" {
		t.Errorf("note = %q, want %q", c.Note, "7.15.3 vs 7.16.0")
	}
}

func TestReadinessDriftBlocksReadiness(t *testing.T) {
	dirty := map[string]diff.SectionDiff{
		"ip/service": {
			Section: "ip/service",
			Hunks:   []diff.Hunk{{Identity: "api", OnA: true, OnB: false}},
		},
	}

	_, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.15.3", "backup", "up"),
		true, true, dirty, nil,
	)

	c := check(t, checks, "No unresolved drift in synced sections")
	if c.OK {
		t.Error("drift check passed with a dirty section")
	}
	if c.Note != "1 section(s) have drift" {
		t.Errorf("note = %q, want the section count", c.Note)
	}
}

func TestReadinessBothMasterIsNotReady(t *testing.T) {
	_, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.15.3", "master", "up"),
		true, true, cleanDrift(), nil,
	)

	if c := check(t, checks, "Exactly one master per VRRP instance"); c.OK {
		t.Error("single-master check passed with two masters")
	}
}

func TestReadinessNetwatchDown(t *testing.T) {
	_, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.15.3", "backup", "down"),
		true, true, cleanDrift(), nil,
	)

	if c := check(t, checks, "Standby netwatch targets up"); c.OK {
		t.Error("netwatch check passed with a target down")
	}
}

func TestReadinessUnreachableRouter(t *testing.T) {
	down := poll.Snapshot{Reachable: false, Err: errFake{}}

	verdict, checks := evaluateReadiness(
		down,
		snapshot("7.15.3", "backup", "up"),
		true, true, cleanDrift(), nil,
	)

	if verdict != VerdictDegraded {
		t.Errorf("verdict = %v, want Degraded", verdict)
	}
	c := check(t, checks, "Both routers reachable")
	if c.OK {
		t.Error("reachable check passed with router a down")
	}
	if c.Note != "router a unreachable" {
		t.Errorf("note = %q, want %q", c.Note, "router a unreachable")
	}
	if c := check(t, checks, "Exactly one master per VRRP instance"); c.OK {
		t.Error("VRRP check must not pass when a router is unreachable")
	}
}

type errFake struct{}

func (errFake) Error() string { return "dial tcp: connection refused" }

// A healthy split pair (one master per instance, on opposite routers) must
// not be flagged Degraded just because summing masters across both routers'
// full instance lists happens to exceed 1.
func TestExactlyOneMasterAcrossMultipleInstances(t *testing.T) {
	a := []routeros.VRRPInstance{
		{Name: "vrrp-lan", State: "master"},
		{Name: "vrrp-wan", State: "backup"},
	}
	b := []routeros.VRRPInstance{
		{Name: "vrrp-lan", State: "backup"},
		{Name: "vrrp-wan", State: "master"},
	}

	if !exactlyOneMaster(a, b) {
		t.Error("exactlyOneMaster = false for a correctly split multi-instance pair, want true")
	}
}

// Summed across both routers' full instance lists, this case totals exactly
// one master (vrrp-lan's), which the old bug would have accepted as Ready
// even though vrrp-wan has no master at all on either router.
func TestExactlyOneMasterDetectsInstanceWithNoMaster(t *testing.T) {
	a := []routeros.VRRPInstance{
		{Name: "vrrp-lan", State: "master"},
		{Name: "vrrp-wan", State: "backup"},
	}
	b := []routeros.VRRPInstance{
		{Name: "vrrp-lan", State: "backup"},
		{Name: "vrrp-wan", State: "backup"},
	}

	if exactlyOneMaster(a, b) {
		t.Error("exactlyOneMaster = true with vrrp-wan having zero masters, want false")
	}
}

// A VRRP fetch failure on either router must not silently read as "no
// instances contributed" (which could mask a real dual-master condition);
// the check must refuse to pass with unknown VRRP state.
func TestReadinessVRRPFetchErrorFailsClosed(t *testing.T) {
	aSnap := snapshot("7.15.3", "master", "up")
	bSnap := snapshot("7.15.3", "master", "up") // real split-brain
	bSnap.VRRPErr = errFake{}
	bSnap.VRRP = nil // fetch failed, not "no instances"

	_, checks := evaluateReadiness(aSnap, bSnap, true, true, cleanDrift(), nil)

	if c := check(t, checks, "Exactly one master per VRRP instance"); c.OK {
		t.Error("VRRP check passed despite a fetch error on router b; must fail closed")
	}
}

func TestReadinessNetwatchFetchErrorFailsClosed(t *testing.T) {
	aSnap := snapshot("7.15.3", "master", "up")
	bSnap := snapshot("7.15.3", "backup", "up")
	bSnap.NetwatchErr = errFake{}
	bSnap.Netwatch = nil

	_, checks := evaluateReadiness(aSnap, bSnap, true, true, cleanDrift(), nil)

	if c := check(t, checks, "Standby netwatch targets up"); c.OK {
		t.Error("netwatch check passed despite a fetch error on router b; must fail closed")
	}
}

// A drift fetch error must block the "clean" verdict even if driftData still
// holds a stale clean result from a previous successful fetch.
func TestReadinessDriftErrorOverridesStaleCleanData(t *testing.T) {
	_, checks := evaluateReadiness(
		snapshot("7.15.3", "master", "up"),
		snapshot("7.15.3", "backup", "up"),
		true, true, cleanDrift(), errFake{},
	)

	if c := check(t, checks, "No unresolved drift in synced sections"); c.OK {
		t.Error("drift check passed using stale clean data despite a fresh fetch error")
	}
}

func TestExactlyOneMaster(t *testing.T) {
	cases := []struct {
		name string
		a, b []routeros.VRRPInstance
		want bool
	}{
		{"a master b backup", []routeros.VRRPInstance{{State: "master"}}, []routeros.VRRPInstance{{State: "backup"}}, true},
		{"b master a backup", []routeros.VRRPInstance{{State: "backup"}}, []routeros.VRRPInstance{{State: "master"}}, true},
		{"both master", []routeros.VRRPInstance{{State: "master"}}, []routeros.VRRPInstance{{State: "master"}}, false},
		{"neither master", []routeros.VRRPInstance{{State: "backup"}}, []routeros.VRRPInstance{{State: "backup"}}, false},
		{"no instances configured", nil, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exactlyOneMaster(tc.a, tc.b); got != tc.want {
				t.Errorf("exactlyOneMaster = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllNetwatchUp(t *testing.T) {
	if !allNetwatchUp(nil) {
		t.Error("no configured targets should be vacuously up")
	}
	if allNetwatchUp([]routeros.NetwatchEntry{{Status: "up"}, {Status: "down"}}) {
		t.Error("a single down target should fail the check")
	}
}

func TestDriftClean(t *testing.T) {
	if driftClean(nil) {
		t.Error("nil drift data means not yet fetched, so not clean")
	}
	if !driftClean(map[string]diff.SectionDiff{}) {
		t.Error("empty drift data is clean")
	}
	if !driftClean(cleanDrift()) {
		t.Error("hunk-free sections are clean")
	}
	if driftClean(map[string]diff.SectionDiff{"ip/service": {Hunks: []diff.Hunk{{Identity: "x"}}}}) {
		t.Error("a section with a hunk is not clean")
	}
}
