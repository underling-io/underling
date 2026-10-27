// Package systemtest replays a real upstream bug fix through heinzel with
// the real Claude Code: see the tests behind the system build tag, run by
// make system-test. They cost real tokens and take minutes, so neither
// make check nor go test ./... runs them.
package systemtest
