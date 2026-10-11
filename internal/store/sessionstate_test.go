package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// kiln runs behind transaction-mode poolers (PgBouncer, RDS Proxy), where two
// statements outside one transaction may land on different server connections.
// Anything that leaves state on a session -- a session advisory lock, a plain
// SET, LISTEN, a temp table, a named PREPARE -- is then silently lost, or worse,
// leaks onto a connection another client gets next. The audit that found none
// of these is only true until the next change, so it is pinned here.

// sessionStateAnywhere matches session state wherever it appears in a
// statement. pg_advisory_xact_lock is transaction-scoped and deliberately not
// matched.
var sessionStateAnywhere = []struct {
	what string
	re   *regexp.Regexp
}{
	{"session advisory lock", regexp.MustCompile(`(?i)\bpg_(try_)?advisory_lock(_shared)?\s*\(`)},
	{"temporary table", regexp.MustCompile(`(?i)\bCREATE\s+((GLOBAL|LOCAL)\s+)?TEMP(ORARY)?\s+TABLE\b`)},
	{"session set_config", regexp.MustCompile(`(?is)\bset_config\s*\([^)]*,\s*false\s*\)`)},
}

// sessionStateStatement matches statements that are session state when they
// open a statement. Anchored to the statement start so `UPDATE t SET ...` is not
// mistaken for a session SET, and the SET pattern must match the whole
// statement, shaped like SQL, so help text such as "...; set auth.mode = none
// to opt out" is not either.
var sessionStateStatement = []struct {
	what string
	re   *regexp.Regexp
}{
	{"session SET", regexp.MustCompile(`(?i)^SET\s+(SESSION\s+)?[a-z_][a-z0-9_.]*\s*(=|\bTO\b)\s*` +
		setValue + `(\s*,\s*` + setValue + `)*$`)},
	{"LISTEN", regexp.MustCompile(`(?i)^LISTEN\s+"?[a-z_][a-z0-9_]*"?\s*$`)},
	{"named PREPARE", regexp.MustCompile(`(?i)^PREPARE\s+[a-z_][a-z0-9_]*\s*(\(|\bAS\b)`)},
}

// setValue is one value of a SET: a quoted string, a bare word or number, or a
// $n placeholder.
const setValue = `('[^']*'|"[^"]*"|[a-z0-9_.$-]+)`

var leadingSQLComments = regexp.MustCompile(`^(\s*--[^\n]*\n)*\s*`)

// sessionStateViolations reports every kind of session state found in sql.
func sessionStateViolations(sql string) []string {
	var found []string
	for _, p := range sessionStateAnywhere {
		if p.re.MatchString(sql) {
			found = append(found, p.what)
		}
	}
	for _, stmt := range strings.Split(sql, ";") {
		stmt = strings.TrimSpace(leadingSQLComments.ReplaceAllString(stmt, ""))
		for _, p := range sessionStateStatement {
			if p.re.MatchString(stmt) {
				found = append(found, p.what)
			}
		}
	}
	return found
}

// The scanner is only as good as its patterns, so each one is proven against a
// sample it must catch and the look-alikes it must not.
func TestSessionStateScanner(t *testing.T) {
	bad := []string{
		`SELECT pg_advisory_lock($1)`,
		`SELECT pg_try_advisory_lock(42)`,
		`select pg_advisory_lock_shared($1)`,
		`CREATE TEMP TABLE scratch (id int)`,
		`CREATE TEMPORARY TABLE scratch (id int)`,
		`create local temporary table scratch (id int)`,
		`SELECT set_config('app.user', $1, false)`,
		`SET statement_timeout = '5s'`,
		`SET SESSION search_path TO kiln`,
		`set search_path to kiln, public`,
		`SET application_name = $1`,
		"UPDATE runs SET status = 'queued'; SET lock_timeout = 0",
		"-- leading comment\nSET search_path = kiln",
		`LISTEN run_events`,
		`PREPARE claim AS SELECT 1`,
		`PREPARE claim(int) AS SELECT $1`,
	}
	for _, s := range bad {
		if len(sessionStateViolations(s)) == 0 {
			t.Errorf("scanner missed session state in %q", s)
		}
	}

	good := []string{
		`SELECT pg_advisory_xact_lock($1)`,
		`SET LOCAL lock_timeout = 0`,
		`set local lock_timeout = '5min'`,
		"UPDATE runs\n\t\tSET status = 'queued', claimed_by = NULL",
		`SELECT set_config('app.user', $1, true)`,
		`ALTER TABLE review_items ALTER COLUMN unit SET DEFAULT ''`,
		`set the configuration first`,
		`absent (credentials unavailable; set KILN_MASTER_KEY to enable)`,
		`a token; set auth.mode = "none" to opt out on a single-user deployment.`,
		`listen on :9090 failed`,
		`prepare the staging directory`,
		`SELECT * FROM runs FOR UPDATE SKIP LOCKED`,
	}
	for _, s := range good {
		if v := sessionStateViolations(s); len(v) != 0 {
			t.Errorf("scanner flagged %q as %v", s, v)
		}
	}
}

// TestNoSessionStateInSource scans every string literal in non-test Go source
// and every migration for session state.
func TestNoSessionStateInSource(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			switch {
			case strings.HasSuffix(path, "_test.go"):
				return nil
			case strings.HasSuffix(path, ".go"):
				scanned++
				for _, lit := range goStringLiterals(t, path) {
					for _, v := range sessionStateViolations(lit.text) {
						t.Errorf("%s:%d: %s in %q", rel, lit.line, v, lit.text)
					}
				}
			case strings.HasSuffix(path, ".sql"):
				scanned++
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, v := range sessionStateViolations(string(body)) {
					t.Errorf("%s: %s", rel, v)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	// A walk that silently found nothing would pass vacuously.
	if scanned < 50 {
		t.Fatalf("scanned only %d files under %s; the walk is not reaching the source", scanned, root)
	}
}

type literal struct {
	line int
	text string
}

func goStringLiterals(t *testing.T, path string) []literal {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []literal
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, literal{line: fset.Position(lit.Pos()).Line, text: s})
		return true
	})
	return out
}
