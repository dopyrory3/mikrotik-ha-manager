package labtest

import (
	"fmt"
	"os"
	"strconv"
)

// A lab instance is one copy of testlab/: two CHR routers under their own
// compose project, with their own containers, host ports, volumes, networks
// and MACs, so several can run side by side. testlab/lab.sh brings one up.
//
// Everything that identifies an instance is derived here from its id, by a
// fixed formula, and never read from configuration: the guard checks the
// pair file and docker against these values, so asking for another instance
// selects another member of this one family of lab identities -- it cannot
// name an arbitrary port or container. Instance 1 is the original lab, with
// exactly the names and ports it has always had. testlab/lab.sh derives the
// same values; TestInstanceMatchesLabScript keeps the two in step.
//
// This file has no build tag so that `make check` covers the derivation; it
// only computes names and touches nothing.

const (
	// InstanceEnv selects the instance the lab suite runs against; unset
	// means instance 1.
	InstanceEnv = "MTHA_LAB_INSTANCE"
	// MaxInstance bounds the id, keeping every derived port in the
	// 20243-29944 range and every derived MAC byte distinct.
	MaxInstance = 99

	// labImage is the image testlab/docker-compose.yml builds; a container
	// running anything else is not a lab router.
	labImage = "mtha-routeros-lab:latest"
	// labEntrypoint is testlab/entrypoint.sh as the image installs it.
	labEntrypoint = "/usr/local/bin/lab-entrypoint.sh"
)

// Instance is one lab instance's identity.
type Instance struct {
	ID      int
	Project string // compose project name, and the prefix of its networks

	Routers [2]InstanceRouter // router1 (pair key a), router2 (pair key b)
}

// InstanceRouter is one router of an instance.
type InstanceRouter struct {
	Service   string // compose service name
	Container string // container_name
	HTTPSPort int    // host port published for the guest's www-ssl
	SSHPort   int    // host port published for the guest's SSH
	Volume    string // its /data volume
	// DefaultMAC and BridgeMAC are the container's MACs on the compose
	// default network and on routeros_net, the network bridged to the
	// guest; testlab/entrypoint.sh picks the bridge port by BridgeMAC.
	DefaultMAC, BridgeMAC string
}

// NewInstance derives instance id's identity. Instance 1 is the original
// lab; instance n >= 2 is project mtha-lab-n on host ports 20000+100n+43
// (router1 HTTPS), +44 (router2 HTTPS), +22 and +23 (SSH).
func NewInstance(id int) (Instance, error) {
	if id < 1 || id > MaxInstance {
		return Instance{}, fmt.Errorf("lab instance %d out of range 1-%d", id, MaxInstance)
	}
	in := Instance{ID: id, Project: "mtha-lab"}
	for i := range in.Routers {
		n := i + 1
		r := &in.Routers[i]
		r.Service = fmt.Sprintf("router%d", n)
		// The fourth MAC byte is id-1, so instance 1 keeps its original
		// MACs and no two instances share one.
		r.DefaultMAC = fmt.Sprintf("02:00:00:%02x:%02x:10", id-1, n)
		r.BridgeMAC = fmt.Sprintf("02:00:00:%02x:%02x:11", id-1, n)
	}
	if id == 1 {
		in.Routers[0].Container, in.Routers[1].Container = "mikrotik-router1", "mikrotik-router2"
		in.Routers[0].HTTPSPort, in.Routers[1].HTTPSPort = 443, 8443
		in.Routers[0].SSHPort, in.Routers[1].SSHPort = 2211, 2212
	} else {
		in.Project = fmt.Sprintf("mtha-lab-%d", id)
		base := 20000 + 100*id
		for i := range in.Routers {
			r := &in.Routers[i]
			r.Container = in.Project + "-" + r.Service
			r.HTTPSPort = base + 43 + i
			r.SSHPort = base + 22 + i
		}
	}
	for i := range in.Routers {
		in.Routers[i].Volume = in.Project + "-" + in.Routers[i].Service + "-data"
	}
	return in, nil
}

// InstanceFromEnv is the instance MTHA_LAB_INSTANCE names, or instance 1.
func InstanceFromEnv() (Instance, error) {
	s, ok := os.LookupEnv(InstanceEnv)
	if !ok || s == "" {
		return NewInstance(1)
	}
	id, err := strconv.Atoi(s)
	if err != nil || strconv.Itoa(id) != s {
		return Instance{}, fmt.Errorf("%s=%q is not an instance id (1-%d)", InstanceEnv, s, MaxInstance)
	}
	return NewInstance(id)
}

// ComposeEnv is the environment testlab/docker-compose.yml reads for this
// instance. Every variable is set, so nothing inherited can leak in.
func (in Instance) ComposeEnv() []string {
	env := []string{"MTHA_LAB_PROJECT=" + in.Project}
	for i, r := range in.Routers {
		p := fmt.Sprintf("MTHA_LAB_ROUTER%d_", i+1)
		env = append(env,
			p+"CONTAINER="+r.Container,
			p+"HTTPS_PORT="+strconv.Itoa(r.HTTPSPort),
			p+"SSH_PORT="+strconv.Itoa(r.SSHPort),
			p+"VOLUME="+r.Volume,
			p+"DEFAULT_MAC="+r.DefaultMAC,
			p+"BRIDGE_MAC="+r.BridgeMAC,
		)
	}
	return env
}
