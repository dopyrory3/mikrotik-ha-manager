package runtime

import (
	"fmt"
	"strconv"

	"mtha/internal/config"
)

// routers is the fixed pair of router keys every Plan/Status/Deploy/Verify
// call operates over, in a stable order.
var routers = []string{"a", "b"}

// Op is one idempotent "ensure this object exists with these fields" unit:
// find the entry in Section (a bare REST path, e.g. "interface/vrrp") where
// MatchField equals MatchValue, create it if missing, patch any mismatched
// fields if present, leave it alone otherwise.
type Op struct {
	Section    string
	MatchField string
	MatchValue string
	Fields     map[string]string
	Label      string

	// Guarded lists entries of Fields whose current on-router value, if
	// non-empty and not this Op's own already-deployed value, must start
	// with the given marker before it may be overwritten — used for
	// on-master/on-backup so a hand-written script is never clobbered.
	Guarded map[string]string
}

// Plan is the full set of Ops for one router.
type Plan struct {
	Router string
	Ops    []Op
}

// BuildPlan renders the desired runtime state for both routers from the pair
// definition. A VRRP instance with none of "on"/"vrid"/"addresses" set is
// tracked for the dashboard/drift screens only and produces no Ops; one with
// some but not all of them set is an error, naming the missing field.
// runtime.toggles, when set, is rendered into the on-master/on-backup
// scripts of the one instance it names; naming no deployable instance is an
// error.
func BuildPlan(pair *config.Pair) (map[string]Plan, error) {
	plans := map[string]Plan{
		"a": {Router: "a"},
		"b": {Router: "b"},
	}

	if err := validateToggles(pair); err != nil {
		return nil, err
	}

	for _, inst := range pair.VRRP {
		deployable, err := validateInstance(inst)
		if err != nil {
			return nil, err
		}
		if !deployable {
			continue
		}

		for _, router := range routers {
			plan := plans[router]
			plan.Ops = append(plan.Ops, vrrpOp(inst, router, pair.Runtime))
			for _, addr := range inst.Addresses {
				plan.Ops = append(plan.Ops, addressOp(inst, addr))
			}
			plans[router] = plan
		}
	}

	for _, target := range pair.Runtime.NetwatchTargets {
		for _, router := range routers {
			plan := plans[router]
			plan.Ops = append(plan.Ops, netwatchOp(target, pair.Runtime))
			plans[router] = plan
		}
	}

	for _, router := range routers {
		plan := plans[router]
		plan.Ops = append(plan.Ops, schedulerOp())
		plans[router] = plan
	}

	return plans, nil
}

// validateToggles requires a configured runtime.toggles to name exactly
// the deployable instance it rides on: toggles attached to a tracked-only
// instance would silently never deploy, and toggles with no named instance
// would be ambiguous across several.
func validateToggles(pair *config.Pair) error {
	toggles := pair.Runtime.Toggles
	if !toggles.Enabled() {
		return nil
	}
	if toggles.VRRP == "" {
		return fmt.Errorf("runtime.toggles: \"vrrp\" is required to name the instance whose transitions drive the toggles")
	}
	for _, inst := range pair.VRRP {
		if inst.Interface != toggles.VRRP {
			continue
		}
		deployable, err := validateInstance(inst)
		if err != nil {
			return err
		}
		if !deployable {
			return fmt.Errorf("runtime.toggles: vrrp instance %s has no \"on\"/\"vrid\"/\"addresses\" set, so its scripts are never deployed", inst.Interface)
		}
		return nil
	}
	return fmt.Errorf("runtime.toggles: vrrp instance %s is not defined in the pair's vrrp list", toggles.VRRP)
}

// validateInstance reports whether inst has enough config to deploy. An
// instance with none of the deploy fields set is valid but not deployable
// (it's tracked read-only, as milestones 1-2 always supported); one with
// some but not all is a config error.
func validateInstance(inst config.VRRPInstance) (deployable bool, err error) {
	set := inst.On != "" || inst.VRID != 0 || len(inst.Addresses) > 0
	if !set {
		return false, nil
	}
	switch {
	case inst.On == "":
		return false, fmt.Errorf("vrrp instance %s: \"on\" is required to deploy", inst.Interface)
	case inst.VRID == 0:
		return false, fmt.Errorf("vrrp instance %s: \"vrid\" is required to deploy", inst.Interface)
	case len(inst.Addresses) == 0:
		return false, fmt.Errorf("vrrp instance %s: \"addresses\" is required to deploy", inst.Interface)
	}
	return true, nil
}

func vrrpTag(name string) string { return "mtha:vrrp:" + name }

// vrrpOp is the VRRP interface itself: role-dependent priority (router "a"
// starts at priority_master, "b" at priority_backup — project.md §5.1's
// router-key convention, not a per-pair choice), plus the tagged
// on-master/on-backup transition scripts, carrying runtime.toggles only if
// this is the instance they name.
func vrrpOp(inst config.VRRPInstance, router string, rt config.RuntimeConfig) Op {
	priority := rt.PriorityBackup
	if router == "a" {
		priority = rt.PriorityMaster
	}
	var toggles config.TogglesConfig
	if rt.Toggles.VRRP == inst.Interface {
		toggles = rt.Toggles
	}
	return Op{
		Section:    "interface/vrrp",
		MatchField: "name",
		MatchValue: inst.Interface,
		Label:      "vrrp interface " + inst.Interface,
		Fields: map[string]string{
			"name":            inst.Interface,
			"interface":       inst.On,
			"vrid":            strconv.Itoa(inst.VRID),
			"priority":        strconv.Itoa(priority),
			"interval":        "1s",
			"preemption-mode": "true",
			"version":         "3",
			"on-master":       onMasterScript(inst.Interface, toggles),
			"on-backup":       onBackupScript(inst.Interface, toggles),
			"comment":         vrrpTag(inst.Interface),
		},
		Guarded: map[string]string{
			"on-master": onMasterMarker(inst.Interface),
			"on-backup": onBackupMarker(inst.Interface),
		},
	}
}

func addressOp(inst config.VRRPInstance, addr string) Op {
	tag := fmt.Sprintf("mtha:vrrp:%s:%s", inst.Interface, addr)
	return Op{
		Section:    "ip/address",
		MatchField: "comment",
		MatchValue: tag,
		Label:      fmt.Sprintf("address %s on %s", addr, inst.Interface),
		Fields: map[string]string{
			"address":   addr,
			"interface": inst.Interface,
			"comment":   tag,
		},
	}
}

func netwatchOp(target string, rt config.RuntimeConfig) Op {
	tag := "mtha:netwatch:" + target
	return Op{
		Section:    "tool/netwatch",
		MatchField: "comment",
		MatchValue: tag,
		Label:      "netwatch " + target,
		Fields: map[string]string{
			"host":        target,
			"interval":    "10s",
			"up-script":   netwatchScript(target, rt.PriorityMaster),
			"down-script": netwatchScript(target, rt.PriorityDegraded),
			"comment":     tag,
		},
	}
}

// schedulerOp is the single periodic-export job (project.md §5.5); its
// interval is a fixed v1 default rather than a config knob.
func schedulerOp() Op {
	return Op{
		Section:    "system/scheduler",
		MatchField: "name",
		MatchValue: "mtha-snapshot",
		Label:      "scheduler mtha-snapshot",
		Fields: map[string]string{
			"name":     "mtha-snapshot",
			"interval": "1d",
			"on-event": "/export file=mtha-snapshot.rsc",
			"comment":  "mtha:scheduler:snapshot",
		},
	}
}
