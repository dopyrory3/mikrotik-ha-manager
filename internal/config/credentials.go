package config

import (
	"fmt"
	"os"
	"strings"
)

// ResolvePassword resolves the password for a router in a pair from the
// environment. The expected variable is MTHA_<PAIR>_<ROUTER>_PASSWORD, e.g.
// MTHA_CORE_A_PASSWORD for router "a" of pair "core". Credentials are never
// read from the pair file itself.
func ResolvePassword(pairName, routerKey string) (string, error) {
	envVar := fmt.Sprintf("MTHA_%s_%s_PASSWORD",
		strings.ToUpper(pairName), strings.ToUpper(routerKey))

	if v, ok := os.LookupEnv(envVar); ok && v != "" {
		return v, nil
	}

	return "", fmt.Errorf("no credential found for pair %q router %q: set %s", pairName, routerKey, envVar)
}
