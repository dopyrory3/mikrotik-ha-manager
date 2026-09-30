//go:build lab

package labtest_test

import (
	"strconv"
	"testing"

	"mtha/internal/routeros"
)

// A priority change on the device moves VRRP master, the dashboard follows
// it, and the pair is Ready on the other side of the move: one master
// either way, and a router at its degraded priority is still one the
// runtime check accepts. (Not Ready at every instant: RouterOS restarts an
// instance whose priority is set, so the move passes through a few seconds
// with no master, which the verdict rightly shows as Degraded.) Drift and
// runtime are re-checked after each move so the verdict rests on fresh
// reads, not on results from before it.
func TestLabVRRPPriorityMovesMaster(t *testing.T) {
	s := readyPair(t)
	rt := s.lab.Pair.Runtime

	// a drops below b: b takes over.
	setPriority(t, s.lab.A, strconv.Itoa(rt.PriorityDegraded))
	waitRole(t, s.lab.B, routeros.RoleMaster)
	waitRole(t, s.lab.A, routeros.RoleBackup)
	s.requireRoles("backup", "master", pollWait)
	s.fetchDrift()
	s.verifyRuntime()
	s.requireReady(pollWait)

	// a back at its base priority preempts b.
	setPriority(t, s.lab.A, strconv.Itoa(rt.PriorityMaster))
	waitRole(t, s.lab.A, routeros.RoleMaster)
	waitRole(t, s.lab.B, routeros.RoleBackup)
	s.requireRoles("master", "backup", pollWait)
	s.fetchDrift()
	s.verifyRuntime()
	s.requireReady(pollWait)
}
