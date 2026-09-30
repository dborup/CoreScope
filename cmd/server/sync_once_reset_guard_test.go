package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"
)

// A sync.Once must never be reassigned. Do holds the Once's internal mutex
// for the whole callback; overwriting the struct zeroes that mutex under a
// running Do, whose deferred unlock then dies with "fatal error: sync: unlock
// of unlocked mutex" (#149). When something has to be able to run again, use
// explicit state under a mutex instead.
//
// The guard parses this package's non-test sources. A type "holds a Once"
// when it is sync.Once, or a struct or array holding one by value. The guard
// rejects any assignment to a field or variable declared with such a type,
// and any assignment of a composite literal of such a type. Not covered:
// copies through pointers (*p = *q), which need type information; go vet's
// copylocks sees those, but go test's vet subset does not run it.
func TestNoSyncOnceIsReassigned(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no production sources parsed: the guard scaffold is broken")
	}
	holdsOnce := onceHolderCheck(files)
	for _, typ := range []string{"StoreTx", "PacketStore"} {
		if !holdsOnce(ast.NewIdent(typ)) {
			t.Fatalf("%s holds a sync.Once but the type scan misses it: the guard is broken", typ)
		}
	}
	if found := syncOnceReassignments(fset, files); len(found) > 0 {
		t.Fatalf("sync.Once reassigned (a running Do dies on its deferred unlock, #149):\n  %s",
			strings.Join(found, "\n  "))
	}
}

func TestSyncOnceGuardControls(t *testing.T) {
	const src = `package p
import "sync"
type gate struct {
	once  sync.Once
	built bool
}
type T struct {
	once  sync.Once
	ptr   *sync.Once
	n     int
	g     gate
	gates [2]gate
	gp    *gate
}
var global sync.Once
func (t *T) resetField()       { t.once = sync.Once{} }
func (t *T) resetGlobal()      { global = sync.Once{} }
func (t *T) resetDeref()       { *t.ptr = sync.Once{} }
func (t *T) copyInto(o T)      { t.once = o.once }
func (t *T) resetHolder()      { t.g = gate{} }
func (t *T) copyHolder(o gate) { t.g = o }
func (t *T) resetHolderArray() { t.gates = [2]gate{} }
func (t *T) resetHolderDeref() { *t.gp = gate{} }
func (t *T) resetOuter()       { *t = T{} }
func (t *T) newPointer()       { t.ptr = new(sync.Once) }
func (t *T) newHolderPointer() { t.gp = &gate{} }
func (t *T) otherField()       { t.n = 1; t.g.built = true }
func (t *T) localOnce()        { var o sync.Once; o.Do(func() {}) }
func (t *T) definedOnce()      { o := sync.Once{}; o.Do(func() {}) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := syncOnceReassignments(fset, []*ast.File{f})
	got := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start, end := fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line
		for _, v := range found {
			var line int
			fmt.Sscanf(v[strings.Index(v, ":")+1:], "%d", &line)
			if line >= start && line <= end {
				got[fn.Name.Name] = true
			}
		}
	}
	for name, want := range map[string]bool{
		"resetField": true, "resetGlobal": true, "resetDeref": true, "copyInto": true,
		"resetHolder": true, "copyHolder": true, "resetHolderArray": true,
		"resetHolderDeref": true, "resetOuter": true,
		"newPointer": false, "newHolderPointer": false, "otherField": false,
		"localOnce": false, "definedOnce": false,
	} {
		if got[name] != want {
			t.Errorf("%s: flagged = %v, want %v (found: %v)", name, got[name], want, found)
		}
	}
}

// syncOnceReassignments returns "file:line: ..." for each assignment in files
// that overwrites a value holding a sync.Once.
func syncOnceReassignments(fset *token.FileSet, files []*ast.File) []string {
	holdsOnce := onceHolderCheck(files)
	onceNames := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				if holdsOnce(n.Type) {
					for _, id := range n.Names {
						onceNames[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				if n.Type != nil && holdsOnce(n.Type) {
					for _, id := range n.Names {
						onceNames[id.Name] = true
					}
				}
			}
			return true
		})
	}
	var found []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || as.Tok == token.DEFINE {
				return true
			}
			reassigns := false
			for _, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && onceNames[sel.Sel.Name] {
					reassigns = true
				}
				if id, ok := lhs.(*ast.Ident); ok && onceNames[id.Name] {
					reassigns = true
				}
			}
			for _, rhs := range as.Rhs {
				if lit, ok := rhs.(*ast.CompositeLit); ok && lit.Type != nil && holdsOnce(lit.Type) {
					reassigns = true
				}
			}
			if reassigns {
				found = append(found, fmt.Sprintf("%s: %s", fset.Position(as.Pos()), types.ExprString(as.Lhs[0])))
			}
			return true
		})
	}
	return found
}

// onceHolderCheck returns a predicate reporting whether a type expression
// holds a sync.Once by value: sync.Once itself, or a package type, struct or
// array that holds one.
func onceHolderCheck(files []*ast.File) func(ast.Expr) bool {
	typeDecls := map[string]ast.Expr{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				typeDecls[ts.Name.Name] = ts.Type
			}
			return true
		})
	}
	holders := map[string]bool{} // package types that hold a Once by value
	var holdsOnce func(e ast.Expr) bool
	holdsOnce = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.SelectorExpr:
			return isIdent(e.X, "sync") && e.Sel.Name == "Once"
		case *ast.Ident:
			return holders[e.Name]
		case *ast.ParenExpr:
			return holdsOnce(e.X)
		case *ast.ArrayType:
			return e.Len != nil && holdsOnce(e.Elt) // an array, not a slice
		case *ast.StructType:
			for _, f := range e.Fields.List {
				if holdsOnce(f.Type) {
					return true
				}
			}
		}
		return false
	}
	for changed := true; changed; {
		changed = false
		for name, typ := range typeDecls {
			if !holders[name] && holdsOnce(typ) {
				holders[name], changed = true, true
			}
		}
	}
	return holdsOnce
}
