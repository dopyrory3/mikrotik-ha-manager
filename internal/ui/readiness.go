package ui

import (
	"fmt"

	"mtha/internal/diff"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// Verdict is the overall readiness state shown on the dashboard.
type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictReady
	VerdictDegraded
)

func (v Verdict) String() string {
	switch v {
	case VerdictReady:
		return "Ready"
	case VerdictDegraded:
		return "Degraded"
	default:
		return "Unknown"
	}
}

// Check is one readiness criterion from project.md §5.2.
type Check struct {
	Label string
	OK    bool
	Note  string
}

// evaluateReadiness applies the checks that are implementable so far.
// Runtime-logic verification lands in milestone 4 (project.md §9) and is
// reported as not-yet-available, which keeps the verdict from claiming a
// fully Ready state it can't back up.
func evaluateReadiness(a, b poll.Snapshot, haveA, haveB bool, driftData map[string]diff.SectionDiff, driftErr error) (Verdict, []Check) {
	checks := []Check{}

	bothReachable := haveA && haveB && a.Reachable && b.Reachable
	checks = append(checks, Check{
		Label: "Both routers reachable",
		OK:    bothReachable,
		Note:  reachabilityNote(a, b, haveA, haveB),
	})

	versionsMatch := bothReachable && a.Resource != nil && b.Resource != nil &&
		a.Resource.Version == b.Resource.Version
	checks = append(checks, Check{
		Label: "RouterOS versions match",
		OK:    versionsMatch,
		Note:  versionNote(a, b, bothReachable),
	})

	checks = append(checks, Check{
		Label: "No unresolved drift in synced sections",
		OK:    driftErr == nil && driftClean(driftData),
		Note:  driftNote(driftData, driftErr),
	})

	vrrpKnown := a.VRRPErr == nil && b.VRRPErr == nil
	singleMaster := bothReachable && vrrpKnown && exactlyOneMaster(a.VRRP, b.VRRP)
	checks = append(checks, Check{
		Label: "Exactly one master per VRRP instance",
		OK:    singleMaster,
		Note:  vrrpNote(a, b, bothReachable),
	})

	checks = append(checks, Check{
		Label: "Runtime logic present and identical on both routers",
		OK:    false,
		Note:  "runtime deployment not yet implemented",
	})

	netwatchKnown := a.NetwatchErr == nil && b.NetwatchErr == nil
	netwatchUp := bothReachable && netwatchKnown && allNetwatchUp(a.Netwatch) && allNetwatchUp(b.Netwatch)
	checks = append(checks, Check{
		Label: "Standby netwatch targets up",
		OK:    netwatchUp,
		Note:  netwatchNote(a, b, bothReachable),
	})

	verdict := VerdictDegraded
	if !haveA && !haveB {
		verdict = VerdictUnknown
	} else {
		allOK := true
		for _, c := range checks {
			if !c.OK {
				allOK = false
				break
			}
		}
		if allOK {
			verdict = VerdictReady
		}
	}

	return verdict, checks
}

func reachabilityNote(a, b poll.Snapshot, haveA, haveB bool) string {
	if !haveA || !haveB {
		return "waiting for first poll"
	}
	if !a.Reachable {
		return "router a unreachable"
	}
	if !b.Reachable {
		return "router b unreachable"
	}
	return ""
}

func versionNote(a, b poll.Snapshot, bothReachable bool) string {
	if !bothReachable || a.Resource == nil || b.Resource == nil {
		return ""
	}
	if a.Resource.Version != b.Resource.Version {
		return a.Resource.Version + " vs " + b.Resource.Version
	}
	return ""
}

// exactlyOneMaster checks the §5.2 criterion per VRRP instance, matching
// instances between routers by name (falling back to interface), not by
// summing master counts across every instance on both routers.
func exactlyOneMaster(a, b []routeros.VRRPInstance) bool {
	masters := map[string]int{}
	seen := map[string]bool{}

	for _, v := range a {
		key := vrrpInstanceKey(v)
		seen[key] = true
		if v.State == "master" {
			masters[key]++
		}
	}
	for _, v := range b {
		key := vrrpInstanceKey(v)
		seen[key] = true
		if v.State == "master" {
			masters[key]++
		}
	}

	if len(seen) == 0 {
		return false
	}
	for key := range seen {
		if masters[key] != 1 {
			return false
		}
	}
	return true
}

func vrrpInstanceKey(v routeros.VRRPInstance) string {
	if v.Name != "" {
		return v.Name
	}
	return v.Interface
}

func vrrpNote(a, b poll.Snapshot, bothReachable bool) string {
	if !bothReachable {
		return ""
	}
	if a.VRRPErr != nil {
		return "router a: " + a.VRRPErr.Error()
	}
	if b.VRRPErr != nil {
		return "router b: " + b.VRRPErr.Error()
	}
	return ""
}

func netwatchNote(a, b poll.Snapshot, bothReachable bool) string {
	if !bothReachable {
		return ""
	}
	if a.NetwatchErr != nil {
		return "router a: " + a.NetwatchErr.Error()
	}
	if b.NetwatchErr != nil {
		return "router b: " + b.NetwatchErr.Error()
	}
	return ""
}

func driftClean(driftData map[string]diff.SectionDiff) bool {
	if driftData == nil {
		return false
	}
	for _, sd := range driftData {
		if !sd.Clean() {
			return false
		}
	}
	return true
}

func driftNote(driftData map[string]diff.SectionDiff, driftErr error) string {
	if driftErr != nil {
		return driftErr.Error()
	}
	if driftData == nil {
		return "press 2 to check drift"
	}
	dirty := 0
	for _, sd := range driftData {
		if !sd.Clean() {
			dirty++
		}
	}
	if dirty > 0 {
		return fmt.Sprintf("%d section(s) have drift", dirty)
	}
	return ""
}

func allNetwatchUp(entries []routeros.NetwatchEntry) bool {
	for _, e := range entries {
		if e.Status != "up" {
			return false
		}
	}
	return true
}
