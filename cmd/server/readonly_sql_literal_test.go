package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// writeSQLPattern matches SQL that writes or changes a database. It is
// applied to the value of every string literal in non-test cmd/server
// source, so comments never match.
var writeSQLPattern = regexp.MustCompile(`(?is)\b(INSERT\s+(OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE\s+(OR\s+\w+\s+)?[\w"]+\s+SET|DELETE\s+FROM|CREATE\s+(TEMP\w*\s+|UNIQUE\s+|VIRTUAL\s+)*(TABLE|INDEX|TRIGGER|VIEW)|DROP\s+(TABLE|INDEX|TRIGGER|VIEW)|ALTER\s+TABLE|VACUUM|REINDEX|ATTACH\s+DATABASE)\b`)

// knownServerWriteSQL lists the write-SQL string literals cmd/server already
// had when this guard was added, per file. The count may only go down: a
// new write belongs in cmd/ingestor (#1283). Lower the number here when a
// site is removed.
//   - ping_score_history.go: its own separate history database, not the
//     shared one.
//   - backup.go: VACUUM INTO writes a snapshot file, not the database.
//   - openapi.go: prose mentioning VACUUM INTO.
//   - hash_migrate.go, routes.go: pre-existing writes to the shared
//     database, tracked separately.
var knownServerWriteSQL = map[string]int{
	"backup.go":             1,
	"hash_migrate.go":       3,
	"openapi.go":            1,
	"ping_score_history.go": 15,
	"routes.go":             3,
}

// TestServerSourceHasNoNewWriteSQL guards the read-only server contract
// (#1283) at the source level: no new INSERT/UPDATE/DELETE/CREATE/DROP/ALTER
// (and the like) may appear in cmd/server, for example a shortcut that
// writes channel_proposals directly instead of queueing a command for the
// ingestor.
func TestServerSourceHasNoNewWriteSQL(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	got := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if m := writeSQLPattern.FindString(v); m != "" {
				got[name] = append(got[name], fset.Position(lit.Pos()).String()+": "+strings.Join(strings.Fields(m), " "))
			}
			return true
		})
	}
	names := make([]string, 0, len(got)+len(knownServerWriteSQL))
	for name := range got {
		names = append(names, name)
	}
	for name := range knownServerWriteSQL {
		if _, ok := got[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		hits, allowed := got[name], knownServerWriteSQL[name]
		switch {
		case len(hits) > allowed:
			t.Errorf("%s has %d write-SQL literal(s), %d allowed — cmd/server is read-only, put writes in cmd/ingestor (#1283):\n  %s",
				name, len(hits), allowed, strings.Join(hits, "\n  "))
		case len(hits) < allowed:
			t.Errorf("%s now has %d write-SQL literal(s); lower knownServerWriteSQL[%q] from %d to %d", name, len(hits), name, allowed, len(hits))
		}
	}
}

// The pattern itself: writes match, reads and prose do not.
func TestWriteSQLPattern(t *testing.T) {
	for _, s := range []string{
		"INSERT INTO channel_proposals (id) VALUES (?)",
		"insert or replace into x values (1)",
		"REPLACE INTO x VALUES (1)",
		"UPDATE channel_proposals SET status = 'approved'",
		"DELETE FROM channel_proposals",
		"CREATE TABLE IF NOT EXISTS t (a)",
		"CREATE UNIQUE INDEX i ON t(a)",
		"CREATE TEMP TABLE t (a)",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN b",
		"VACUUM",
		"ATTACH DATABASE 'x' AS y",
	} {
		if !writeSQLPattern.MatchString(s) {
			t.Errorf("must match %q", s)
		}
	}
	for _, s := range []string{
		"SELECT id, name FROM channel_proposals WHERE status = ?",
		"SELECT COUNT(*) FROM observations WHERE updated_at > ?",
		"could not update the channel list",
		"deleted_at",
		"Update available",
	} {
		if writeSQLPattern.MatchString(s) {
			t.Errorf("must not match %q", s)
		}
	}
}
