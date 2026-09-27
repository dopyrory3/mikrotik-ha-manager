// Package labtest is the harness for the live-router integration suite: tests
// that drive mtha against the two RouterOS 7 CHR instances in testlab/.
//
// Everything except this file is behind the "lab" build tag, and the harness
// additionally refuses to run unless MTHA_LAB=1 is set, so an ordinary
// `go test ./...` never compiles it and a stray `-tags lab` never writes to a
// router by accident. Run the suite with `make test-lab`.
//
// Lab tests live in files named *_lab_test.go, start with
//
//	//go:build lab
//
// and begin by calling New, which checks the target really is the lab,
// establishes the baseline once per test binary (provision.sh, then a golden
// /system/backup/save on each router) and restores that backup from
// t.Cleanup. See README.md, "Testing against real RouterOS".
package labtest
