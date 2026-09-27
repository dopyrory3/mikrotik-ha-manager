package config

import (
	"fmt"
	"os"
	"strings"
)

// ResolvePassword resolves the password for a router in a pair from the
// environment. The expected variable is MTHA_<PAIR>_<ROUTER>_PASSWORD, e.g.
// MTHA_CORE_A_PASSWORD for router "a" of pair "core". Credentials are never
// read from the pair file itself (project.md §5.1).
//
// <PAIR> and <ROUTER> are upper-cased with "-" mapped to "_", so pair
// "dc-edge" reads MTHA_DC_EDGE_A_PASSWORD: a POSIX shell cannot export a name
// containing a hyphen. The mapping applies to the variable name only; the
// pair keeps its configured name everywhere else.
func ResolvePassword(pairName, routerKey string) (string, error) {
	envVar := fmt.Sprintf("MTHA_%s_%s_PASSWORD", envName(pairName), envName(routerKey))

	if v, ok := os.LookupEnv(envVar); ok && v != "" {
		return v, nil
	}

	return "", fmt.Errorf("no credential found for pair %q router %q: set %s", pairName, routerKey, envVar)
}

// envName turns one part of a credential variable name into the form
// ResolvePassword looks up: upper-cased, with "-" mapped to "_".
func envName(s string) string {
	return strings.ReplaceAll(strings.ToUpper(s), "-", "_")
}
