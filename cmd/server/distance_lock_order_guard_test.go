package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Issue #149 lock-order rule: s.mu (Lock or RLock) is never acquired while
// distLazyMu is held. The background-load completion takes s.mu first, so any
// distLazyMu → s.mu path deadlocks against it.
//
// The guard parses this package's non-test sources and walks every function
// that uses distLazyMu statement by statement, tracking whether distLazyMu
// may be held (the union over branches). While it may be held, it rejects a
// .mu.Lock() / .mu.RLock() call and a call to any PacketStore method that
// takes s.mu, directly or through other PacketStore methods on the same
// receiver. Calls in go statements and function literals are not inherited:
// they run on another goroutine or later. Function literals are checked as
// functions of their own.
func TestDistLazyMuNeverHeldWhileTakingStoreMu(t *testing.T) {
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

	takers := storeMuTakers(files)
	for _, m := range []string{"Load", "loadBackgroundChunks"} {
		if !takers[m] {
			t.Fatalf("storeMuTakers misses %s, which takes s.mu: the call-graph scan is broken", m)
		}
	}

	var checked []string
	var violations []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			var name string
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				name, body = fn.Name.Name, fn.Body
			case *ast.FuncLit:
				name, body = "func literal", fn.Body
			}
			if body == nil || !usesField(body, "distLazyMu") {
				return true
			}
			checked = append(checked, name)
			violations = append(violations, lockOrderViolations(fset, body, takers)...)
			return true
		})
	}

	sort.Strings(checked)
	if !containsString(checked, "TriggerDistanceIndexBuild") {
		t.Fatalf("TriggerDistanceIndexBuild was not checked (checked: %v): the guard no longer sees the distance-build gate", checked)
	}
	if len(violations) > 0 {
		t.Fatalf("s.mu is taken while distLazyMu may be held (lock-order deadlock with the background-load completion, #149):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// The walker itself: a positive and a negative control on synthetic code.
func TestLockOrderWalkerControls(t *testing.T) {
	const src = `package p
func (s *PacketStore) direct() {
	s.distLazyMu.Lock()
	if s.distLazyBuilt {
		s.mu.RLock()
		s.mu.RUnlock()
	}
	s.distLazyMu.Unlock()
}
func (s *PacketStore) viaMethod() {
	s.distLazyMu.Lock()
	defer s.distLazyMu.Unlock()
	_ = s.reads()
}
func (s *PacketStore) reads() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalObs
}
func (s *PacketStore) okEarlyReturn() {
	s.distLazyMu.Lock()
	if s.distLazyBuilding {
		s.distLazyMu.Unlock()
		return
	}
	s.distLazyMu.Unlock()
	s.mu.RLock()
	s.mu.RUnlock()
}
func (s *PacketStore) okGoroutine() {
	s.distLazyMu.Lock()
	go s.reads()
	go func() { s.mu.Lock(); s.mu.Unlock() }()
	s.distLazyMu.Unlock()
}
func (s *PacketStore) okOtherOrder() {
	s.mu.Lock()
	s.distLazyMu.Lock()
	s.distLazyMu.Unlock()
	s.mu.Unlock()
}
func (s *PacketStore) loopHeldAcrossIterations() {
	for i := 0; i < 2; i++ {
		s.mu.RLock()
		s.mu.RUnlock()
		s.distLazyMu.Lock()
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	takers := storeMuTakers([]*ast.File{f})
	got := map[string]int{}
	for _, d := range f.Decls {
		fn := d.(*ast.FuncDecl)
		got[fn.Name.Name] = len(lockOrderViolations(fset, fn.Body, takers))
	}
	want := map[string]int{
		"direct": 1, "viaMethod": 1, "reads": 0, "okEarlyReturn": 0,
		"okGoroutine": 0, "okOtherOrder": 0, "loopHeldAcrossIterations": 1,
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: %d violation(s), want %d", name, got[name], n)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// usesField reports whether n selects a field or method named field, outside
// nested function literals.
func usesField(n ast.Node, field string) bool {
	found := false
	inspectSameGoroutine(n, func(n ast.Node) {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
			found = true
		}
	})
	return found
}

// inspectSameGoroutine visits n in source order, skipping function literals
// and the calls of go statements (their arguments are still visited).
func inspectSameGoroutine(n ast.Node, visit func(ast.Node)) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.GoStmt:
			for _, a := range n.Call.Args {
				inspectSameGoroutine(a, visit)
			}
			return false
		case nil:
			return false
		}
		visit(n)
		return true
	})
}

// callOn matches <x>.<field>.<method>(...) and returns true for it.
func callOn(call *ast.CallExpr, field string, methods ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != field {
		return false
	}
	for _, m := range methods {
		if sel.Sel.Name == m {
			return true
		}
	}
	return false
}

// storeMuTakers returns the PacketStore methods that take s.mu (Lock or
// RLock) on their own goroutine, directly or through PacketStore methods
// called on the same receiver.
func storeMuTakers(files []*ast.File) map[string]bool {
	takers := map[string]bool{}
	calls := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "PacketStore" || len(fn.Recv.List[0].Names) != 1 {
				continue
			}
			recv := fn.Recv.List[0].Names[0].Name
			name := fn.Name.Name
			inspectSameGoroutine(fn.Body, func(n ast.Node) {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return
				}
				if callOn(call, "mu", "Lock", "RLock") {
					if inner := sel.X.(*ast.SelectorExpr); isIdent(inner.X, recv) {
						takers[name] = true
					}
				}
				if isIdent(sel.X, recv) {
					calls[name] = append(calls[name], sel.Sel.Name)
				}
			})
		}
	}
	for changed := true; changed; {
		changed = false
		for m, callees := range calls {
			if takers[m] {
				continue
			}
			for _, c := range callees {
				if takers[c] {
					takers[m] = true
					changed = true
					break
				}
			}
		}
	}
	return takers
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// lockOrderViolations walks body and reports each place where s.mu is taken
// while distLazyMu may be held.
func lockOrderViolations(fset *token.FileSet, body *ast.BlockStmt, takers map[string]bool) []string {
	w := &lockOrderWalker{fset: fset, takers: takers, seen: map[token.Pos]bool{}}
	w.block(body.List, false)
	return w.violations
}

type lockOrderWalker struct {
	fset       *token.FileSet
	takers     map[string]bool
	seen       map[token.Pos]bool
	violations []string
	// branchHeld collects the state at break/continue/goto, which leave
	// the enclosing statement list; loops and switches merge it.
	branchHeld bool
}

// block walks stmts with distLazyMu possibly held on entry. It returns
// whether the lock may be held at the end and whether every path ends early
// (return, panic, break, continue, goto).
func (w *lockOrderWalker) block(stmts []ast.Stmt, held bool) (bool, bool) {
	for _, s := range stmts {
		var ends bool
		held, ends = w.stmt(s, held)
		if ends {
			return held, true
		}
	}
	return held, false
}

func (w *lockOrderWalker) stmt(s ast.Stmt, held bool) (bool, bool) {
	switch s := s.(type) {
	case *ast.BlockStmt:
		return w.block(s.List, held)
	case *ast.LabeledStmt:
		return w.stmt(s.Stmt, held)
	case *ast.IfStmt:
		if s.Init != nil {
			held, _ = w.stmt(s.Init, held)
		}
		held = w.expr(s.Cond, held)
		thenHeld, thenEnds := w.block(s.Body.List, held)
		elseHeld, elseEnds := held, false
		if s.Else != nil {
			elseHeld, elseEnds = w.stmt(s.Else, held)
		}
		return mergeLockState([]bool{thenHeld, elseHeld}, []bool{thenEnds, elseEnds})
	case *ast.ForStmt:
		if s.Init != nil {
			held, _ = w.stmt(s.Init, held)
		}
		return w.loop(held, func(h bool) (bool, bool) {
			h = w.expr(s.Cond, h)
			h, ends := w.block(s.Body.List, h)
			if s.Post != nil && !ends {
				h, _ = w.stmt(s.Post, h)
			}
			return h, ends
		}), false
	case *ast.RangeStmt:
		held = w.expr(s.X, held)
		return w.loop(held, func(h bool) (bool, bool) { return w.block(s.Body.List, h) }), false
	case *ast.SwitchStmt:
		if s.Init != nil {
			held, _ = w.stmt(s.Init, held)
		}
		held = w.expr(s.Tag, held)
		return w.clauses(held, s.Body)
	case *ast.TypeSwitchStmt:
		if s.Init != nil {
			held, _ = w.stmt(s.Init, held)
		}
		held, _ = w.stmt(s.Assign, held)
		return w.clauses(held, s.Body)
	case *ast.SelectStmt:
		return w.clauses(held, s.Body)
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			held = w.expr(r, held)
		}
		return held, true
	case *ast.BranchStmt:
		w.branchHeld = w.branchHeld || held
		return held, true
	case *ast.GoStmt:
		// The goroutine does not hold distLazyMu; only its arguments are
		// evaluated here.
		for _, a := range s.Call.Args {
			held = w.expr(a, held)
		}
		return held, false
	case *ast.DeferStmt:
		// A deferred call runs at return. defer distLazyMu.Unlock() keeps
		// the lock held until then, which is what "held" already says.
		for _, a := range s.Call.Args {
			held = w.expr(a, held)
		}
		return held, false
	case *ast.ExprStmt:
		held = w.expr(s.X, held)
		if call, ok := s.X.(*ast.CallExpr); ok && isIdent(call.Fun, "panic") {
			return held, true
		}
		return held, false
	default:
		return w.expr(s, held), false
	}
}

// loop runs body once with the entry state and, when the body can end with
// distLazyMu held, again with it held, as the next iteration would.
func (w *lockOrderWalker) loop(held bool, body func(bool) (bool, bool)) bool {
	saved := w.branchHeld
	w.branchHeld = false
	out, _ := body(held)
	if (out || w.branchHeld) && !held {
		out2, _ := body(true)
		out = out || out2
	}
	out = held || out || w.branchHeld
	w.branchHeld = saved
	return out
}

func (w *lockOrderWalker) clauses(held bool, body *ast.BlockStmt) (bool, bool) {
	saved := w.branchHeld
	w.branchHeld = false
	var outs, ends []bool
	hasDefault := false
	for _, c := range body.List {
		h := held
		var list []ast.Stmt
		switch c := c.(type) {
		case *ast.CaseClause:
			for _, e := range c.List {
				h = w.expr(e, h)
			}
			hasDefault = hasDefault || c.List == nil
			list = c.Body
		case *ast.CommClause:
			if c.Comm != nil {
				h, _ = w.stmt(c.Comm, h)
			}
			hasDefault = hasDefault || c.Comm == nil
			list = c.Body
		}
		h, e := w.block(list, h)
		outs, ends = append(outs, h), append(ends, e)
	}
	if !hasDefault {
		outs, ends = append(outs, held), append(ends, false)
	}
	out, allEnd := mergeLockState(outs, ends)
	// A break leaves the switch/select and continues after it.
	if w.branchHeld {
		out, allEnd = true, false
	}
	w.branchHeld = saved
	return out, allEnd
}

// mergeLockState joins branches: held if any branch that falls through may
// hold the lock; ends only if every branch ends early.
func mergeLockState(held, ends []bool) (bool, bool) {
	out, allEnd := false, true
	for i := range held {
		if !ends[i] {
			allEnd = false
			out = out || held[i]
		}
	}
	return out, allEnd
}

// expr applies the lock effects of the calls in n, in source order.
func (w *lockOrderWalker) expr(n ast.Node, held bool) bool {
	if n == nil {
		return held
	}
	inspectSameGoroutine(n, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		switch {
		case callOn(call, "distLazyMu", "Lock", "TryLock"):
			held = true
		case callOn(call, "distLazyMu", "Unlock"):
			held = false
		case held && callOn(call, "mu", "Lock", "RLock"):
			w.report(call, "takes "+exprString(call.Fun))
		case held:
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && w.takers[sel.Sel.Name] {
				w.report(call, "calls "+sel.Sel.Name+", which takes s.mu")
			}
		}
	})
	return held
}

func (w *lockOrderWalker) report(call *ast.CallExpr, what string) {
	if w.seen[call.Pos()] {
		return
	}
	w.seen[call.Pos()] = true
	w.violations = append(w.violations, fmt.Sprintf("%s: %s with distLazyMu held", w.fset.Position(call.Pos()), what))
}

func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return "?"
}
