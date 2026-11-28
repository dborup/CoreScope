package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The graceful-shutdown handler must be registered for SIGINT/SIGTERM before
// the HTTP listener binds. main used to call signal.Notify inside the shutdown
// goroutine, so /api/healthz could answer 200 before that goroutine ran, and a
// SIGTERM in that window took Go's default action: exit -1, no shutdown (#567,
// TestServerProcess_NormalStartDoesNotWait in CI). The registration must be a
// statement of main itself, in program order before ListenAndServe; inside a
// func literal or a go statement it is not ordered before the bind.
func TestMainRegistersShutdownSignalsBeforeListening_567(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body.List
		}
	}
	if body == nil {
		t.Fatal("func main not found in main.go")
	}
	notify, listen := shutdownNotifyAndListen(body)
	if listen < 0 {
		t.Fatal("httpServer.ListenAndServe() not found among main's statements: the guard is broken")
	}
	if notify < 0 || notify > listen {
		t.Fatalf("main must call signal.Notify(…, SIGINT, SIGTERM) as its own statement before "+
			"httpServer.ListenAndServe() (line %d); a registration inside the shutdown goroutine "+
			"races the listener (#567)", fset.Position(body[listen].Pos()).Line)
	}
}

// shutdownNotifyAndListen returns the index in body of the first shutdown
// signal registration and of the first ListenAndServe call, or -1.
func shutdownNotifyAndListen(body []ast.Stmt) (notify, listen int) {
	notify, listen = -1, -1
	for i, stmt := range body {
		if notify < 0 && isShutdownNotify(stmt) {
			notify = i
		}
		if listen < 0 && callsListenAndServe(stmt) {
			listen = i
		}
	}
	return notify, listen
}

// isShutdownNotify reports whether stmt is itself a signal.Notify call that
// names SIGTERM (not one nested in a func literal or go statement).
func isShutdownNotify(stmt ast.Stmt) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || !isSelector(call.Fun, "signal", "Notify") {
		return false
	}
	for _, arg := range call.Args {
		if isSelector(arg, "syscall", "SIGTERM") {
			return true
		}
	}
	return false
}

// callsListenAndServe reports whether stmt contains a call to
// httpServer.ListenAndServe outside any func literal.
func callsListenAndServe(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if isSelector(n.Fun, "httpServer", "ListenAndServe") {
				found = true
			}
		}
		return !found
	})
	return found
}

func isSelector(e ast.Expr, x, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == x
}

// The guard itself: a registration inside the goroutine, inside a go
// statement, or after the listener is rejected; the fixed order passes.
func TestShutdownSignalGuardControls_567(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"before listen", `sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh }()
	if err := httpServer.ListenAndServe(); err != nil {}`, true},
		{"inside goroutine", `go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
	}()
	if err := httpServer.ListenAndServe(); err != nil {}`, false},
		{"go statement", `sigCh := make(chan os.Signal, 1)
	go signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	if err := httpServer.ListenAndServe(); err != nil {}`, false},
		{"SIGINT only", `sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	if err := httpServer.ListenAndServe(); err != nil {}`, false},
	}
	for _, c := range cases {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\nfunc main() {\n\t"+c.body+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		notify, listen := shutdownNotifyAndListen(f.Decls[0].(*ast.FuncDecl).Body.List)
		if got := listen >= 0 && notify >= 0 && notify < listen; got != c.ok {
			t.Errorf("%s: guard accepts = %v, want %v", c.name, got, c.ok)
		}
	}
}
