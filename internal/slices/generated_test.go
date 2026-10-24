package slices_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// generatedFile is written by `go generate` (see gen.go) and blank-imports
// every slice package so that each one registers itself with the app.
const generatedFile = "slices_gen.go"

func TestGeneratedFileImportsEverySliceDirectory(t *testing.T) {
	var dirs []string
	for _, dir := range mustSliceDirs(t) {
		dirs = append(dirs, slicesPath+"/"+dir)
	}

	f, err := parser.ParseFile(token.NewFileSet(), generatedFile, nil, parser.ParseComments|parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v (run go generate ./...)", generatedFile, err)
	}
	if !ast.IsGenerated(f) {
		t.Errorf("%s lacks the \"// Code generated ... DO NOT EDIT.\" header", generatedFile)
	}
	var imported []string
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote %s: %v", spec.Path.Value, err)
		}
		if spec.Name == nil || spec.Name.Name != "_" {
			t.Errorf("%s imports %s by name; want a blank import", generatedFile, path)
		}
		imported = append(imported, path)
	}

	for _, dir := range dirs {
		if !slices.Contains(imported, dir) {
			t.Errorf("%s is missing slice %s; run go generate ./...", generatedFile, dir)
		}
	}
	for _, path := range imported {
		if !slices.Contains(dirs, path) {
			t.Errorf("%s imports %s, which is not a slice directory; run go generate ./...", generatedFile, path)
		}
	}
	if !slices.IsSorted(imported) {
		t.Errorf("%s imports are not sorted: %v", generatedFile, imported)
	}
}
