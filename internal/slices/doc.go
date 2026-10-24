// Package slices is the parent of the vertical slices, one package each.
// Importing it wires every slice in: the generated slices_gen.go blank-imports
// each slice package, and each slice registers itself with the agent in init.
// Run `go generate ./...` after adding or removing a slice directory; a test
// fails until the generated file matches the directories.
package slices

//go:generate go run gen.go
