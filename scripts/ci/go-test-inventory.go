// go-test-inventory discovers runnable top-level tests without depending on
// textual grep matches in comments, strings, methods, or subtest names.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type entry struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Active bool   `json:"active"`
}

type inventory struct {
	Version int     `json:"version"`
	Package string  `json:"package"`
	Entries []entry `json:"entries"`
}

func testName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	if rest == "" {
		return true
	}
	for _, r := range rest {
		return !unicode.IsLower(r)
	}
	return false
}

func testingArgument(fn *ast.FuncDecl, alias, kind string) bool {
	if fn.Type.TypeParams != nil || fn.Type.Results != nil || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	field := fn.Type.Params.List[0]
	if len(field.Names) > 1 {
		return false
	}
	ptr, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := ptr.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != kind {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == alias
}

func collect(root string) (inventory, error) {
	out := inventory{Version: 1, Package: "github.com/corescope/server", Entries: []entry{}}
	dir := filepath.Join(root, "cmd", "server")
	var files []string
	err := filepath.WalkDir(dir, func(file string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if file != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(file, ".go") && filepath.Dir(file) != dir {
			return fmt.Errorf("cmd/server must remain one package; nested Go source: %s", file)
		}
		if strings.HasSuffix(file, "_test.go") {
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("test source is not a regular file: %s", file)
			}
			files = append(files, file)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	context := build.Default
	// These shards always use -race; matching the same build tag prevents a
	// race-only test from being mistaken for an inactive source assignment.
	context.BuildTags = append(append([]string{}, context.BuildTags...), "race")
	for _, file := range files {
		source, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			return out, err
		}
		active, err := context.MatchFile(dir, filepath.Base(file))
		if err != nil {
			return out, err
		}
		testingAlias := "testing"
		for _, imp := range source.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return out, err
			}
			if name == "testing" && imp.Name != nil {
				testingAlias = imp.Name.Name
			}
		}
		executableExamples := map[string]bool{}
		for _, example := range doc.Examples(source) {
			if example.Output != "" || example.EmptyOutput {
				executableExamples["Example"+example.Name] = true
			}
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return out, err
		}
		for _, declaration := range source.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name == "TestMain" {
				continue
			}
			kind := ""
			switch {
			case testName(fn.Name.Name, "Test"):
				if !testingArgument(fn, testingAlias, "T") {
					return out, fmt.Errorf("invalid test signature: %s in %s", fn.Name.Name, rel)
				}
				kind = "test"
			case testName(fn.Name.Name, "Fuzz"):
				if !testingArgument(fn, testingAlias, "F") {
					return out, fmt.Errorf("invalid fuzz signature: %s in %s", fn.Name.Name, rel)
				}
				kind = "fuzz"
			case executableExamples[fn.Name.Name]:
				kind = "example"
			}
			if kind != "" {
				out.Entries = append(out.Entries, entry{Name: fn.Name.Name, File: filepath.ToSlash(rel), Kind: kind, Active: active})
			}
		}
	}
	return out, nil
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected inventory arguments")
		os.Exit(1)
	}
	result, err := collect(*root)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
