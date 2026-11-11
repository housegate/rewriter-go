package engine

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSnapshotClosedAnalysis(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	catalog := []SnapshotTable{{Database: "tenant", Name: "copy", ID: "target", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}, {Database: "tenant", Name: "events", ID: "source", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}}
	for _, tt := range []struct {
		sql string
		ok  bool
	}{
		{"INSERT INTO tenant.copy SELECT value FROM tenant.events", true},
		{"INSERT INTO tenant.copy SELECT (SELECT value FROM tenant.events)", true},
		{"INSERT INTO tenant.copy SELECT 7", true},
		{"INSERT INTO tenant.copy WITH unused AS (SELECT missing FROM tenant.events) SELECT 7", false},
		{"INSERT INTO tenant.copy SELECT (SELECT e.value) FROM tenant.events AS e", false},
		{"INSERT INTO tenant.copy SELECT -9223372036854775809", false},
		{"INSERT INTO tenant.copy SELECT +value FROM tenant.events", false},
		{"INSERT INTO tenant.copy SELECT +(SELECT value FROM tenant.events)", false},
		{"INSERT INTO tenant.copy SELECT + +7", false},
		{"INSERT INTO tenant.copy SELECT -(+7)", false},
		{"INSERT INTO tenant.copy SELECT -(-7)", false},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, tt.sql, SnapshotOptions{Database: "tenant", Catalog: catalog})
			if (err == nil) != tt.ok {
				t.Fatalf("plan=%v error=%v", p, err)
			}
		})
	}
}

func TestSnapshotPreparationBoundGraph(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	catalog := []SnapshotTable{{Database: "tenant", Name: "copy", ID: "target", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}, {Database: "tenant", Name: "events", ID: "source", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}, {Database: "tenant", Name: "ids", ID: "unused", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: false}}}}
	for _, tt := range []struct{ sql, want string }{
		{"WITH x AS (SELECT value FROM tenant.events), y AS (SELECT value FROM x) INSERT INTO tenant.copy WITH x AS (SELECT value FROM y) SELECT value FROM x", "WITH x AS (SELECT value FROM scratch.r), y AS (SELECT value FROM x) SELECT * FROM (WITH x AS (SELECT value FROM y) SELECT value FROM x) AS snapshot_output"},
		{"INSERT INTO tenant.copy SELECT events.value FROM tenant.events", "SELECT events.value FROM scratch.r AS events"},
		{"INSERT INTO tenant.copy WITH unused AS (SELECT value FROM tenant.ids) SELECT value FROM tenant.events", "SELECT value FROM scratch.r"},
		{"WITH x AS (SELECT value FROM tenant.events) INSERT INTO tenant.copy SELECT value FROM x", "WITH x AS (SELECT value FROM scratch.r) SELECT value FROM x"},
		{"INSERT INTO tenant.copy SELECT (SELECT (SELECT value FROM tenant.events))", "SELECT accurateCast((SELECT (SELECT value FROM scratch.r)), 'Int64')"},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, tt.sql, SnapshotOptions{Database: "tenant", Catalog: catalog})
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Prepare(e, map[string][2]string{"source": {"scratch", "r"}})
			if err != nil || got != tt.want {
				t.Fatalf("want %s; got %s; err %v", tt.want, got, err)
			}
		})
	}
}

func TestSnapshotUseClassification(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, tt := range []struct {
		sql      string
		ordinary bool
	}{{"USE tenant", true}, {"USE `tenant`", true}, {"USE tenant; SELECT 1", false}, {"USE tenant trailing", false}, {"USE DATABASE tenant", false}, {"USE {db:Identifier}", false}, {"USE [tenant]", false}, {"USE 'tenant'", false}, {"SYSTEM RELOAD CONFIG", false}, {"FUTURE SNAPSHOT", false}} {
		t.Run(tt.sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, tt.sql, SnapshotOptions{})
			got := err == nil && p.Ordinary
			if got != tt.ordinary {
				t.Fatalf("ordinary=%v err=%v plan=%+v", got, err, p)
			}
		})
	}
}

// TestSnapshotOrdinaryClassification pins which statement classes the snapshot
// analyzer acknowledges as ordinary (NOT_SNAPSHOT_QUERY) under the pinned
// polyglot ClickHouse dialect. Everything else must refuse with the
// INVALID_INPUT "requires one recognized statement" category; it never falls
// back to ordinary passthrough.
func TestSnapshotOrdinaryClassification(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ordinary := []string{
		"SELECT value FROM tenant.events",
		"SELECT 1 UNION ALL SELECT 2",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"CREATE TABLE tenant.t (value Int64) ENGINE = MergeTree ORDER BY value",
		"CREATE TABLE tenant.t AS tenant.events",
		"UPDATE tenant.events SET value = 1 WHERE 1",
		"DELETE FROM tenant.events WHERE 1",
		"TRUNCATE TABLE tenant.events",
		"USE tenant",
	}
	refused := []string{
		// The ClickHouse dialect parses these as generic command nodes; the C++
		// engine refuses them too, so both engines agree.
		"GRANT SELECT ON tenant.events TO reader",
		"GRANT reader TO u",
		"REVOKE SELECT ON tenant.events FROM reader",
		"DESCRIBE TABLE tenant.events",
		"DESC tenant.events",
		"SHOW TABLES FROM tenant",
		"SHOW DATABASES",
		"SHOW CREATE TABLE tenant.events",
		"SHOW TABLES FROM tenant FORMAT Native",
		"EXISTS TABLE tenant.events",
		"EXPLAIN SELECT 1",
		"SET max_threads = 1",
		"SYSTEM RELOAD CONFIG",
		"OPTIMIZE TABLE tenant.events",
		"CHECK TABLE tenant.events",
		"RENAME TABLE tenant.a TO tenant.b",
		"CREATE USER u",
		"CREATE FUNCTION f AS x -> x",
		// Known divergence: the C++ engine classifies these as ordinary
		// (ASTDropQuery / ASTAlterQuery / ASTCreateQuery / ASTSelectWithUnionQuery).
		// This engine refuses them, which is fail-closed. Reconcile the two
		// ordinary sets in both engines together, never in one alone.
		"DROP TABLE tenant.events",
		"DROP TABLE IF EXISTS tenant.events",
		"DROP VIEW tenant.v",
		"DROP DATABASE x",
		"DROP DICTIONARY tenant.d",
		"ALTER TABLE tenant.events ADD COLUMN c Int64",
		"ALTER TABLE tenant.events DELETE WHERE 1",
		"ALTER TABLE tenant.events UPDATE value = 1 WHERE 1",
		"CREATE VIEW tenant.v AS SELECT 1",
		"CREATE MATERIALIZED VIEW tenant.mv ENGINE = Memory AS SELECT 1",
		"CREATE DATABASE x",
		"SELECT 1 INTERSECT SELECT 1",
		"SELECT 1 EXCEPT SELECT 1",
		"(SELECT 1)",
	}
	for _, sql := range ordinary {
		t.Run("ordinary/"+sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, sql, SnapshotOptions{Database: "tenant"})
			if err != nil || p == nil || !p.Ordinary {
				t.Fatalf("plan=%+v err=%v; want ordinary", p, err)
			}
		})
	}
	for _, sql := range refused {
		t.Run("refused/"+sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, sql, SnapshotOptions{Database: "tenant"})
			if p != nil || !errors.Is(err, snapshotStatement) {
				t.Fatalf("plan=%+v err=%v; want the INVALID_INPUT statement refusal", p, err)
			}
		})
	}
}

func TestSnapshotLexicalAndMaterialization(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	catalog := []SnapshotTable{{Database: "t", Name: "dst", ID: "dst", Columns: []SnapshotColumn{{Name: "a", Type: "Int64", Ordinary: true}, {Name: "b", Type: "Int64", Ordinary: true}}}, {Database: "t", Name: "src", ID: "src", Columns: []SnapshotColumn{{Name: "a", Type: "Int64", Ordinary: true}, {Name: "b", Type: "Int64", Ordinary: true}}}}
	for _, tt := range []struct {
		sql    string
		random []uint64
		want   string
	}{
		{"INSERT INTO t.dst WITH x AS (SELECT rand() AS a, rand() AS b) SELECT a, b FROM x", []uint64{7, 8}, ""},
		{"INSERT INTO t.dst SELECT rand(), v FROM (SELECT rand() AS v)", []uint64{7, 8}, ""},
		{"INSERT INTO t.dst SELECT rand(), rand64()", []uint64{7, 8}, "INSERT INTO t.dst SELECT 7, 8"},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			p, err := AnalyzeSnapshot(e, tt.sql, SnapshotOptions{Database: "t", Catalog: catalog, Materialize: true, Random: tt.random})
			if tt.want == "" {
				if err == nil {
					t.Fatal("derived literal provenance was admitted")
				}
				return
			}
			if err != nil || p.SQL != tt.want {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
		})
	}
	ast, err := e.ParseOne("SELECT 'INNER ALL JOIN', `INNER` FROM t.src ALL INNER JOIN t.src AS r ON r.a = src.a")
	if err != nil {
		t.Fatal(err)
	}

	got, err := generateSnapshot(e, ast)
	if err != nil || !strings.Contains(got, "'INNER ALL JOIN'") || !strings.Contains(got, "`INNER`") || !strings.Contains(got, " ALL INNER JOIN ") {
		t.Fatalf("formatter changed nonkeywords: %s %v", got, err)
	}
}

func TestSnapshotAliasBindingRefusals(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	catalog := []SnapshotTable{{Database: "t", Name: "dst", ID: "dst", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}, {Database: "t", Name: "src", ID: "src", Columns: []SnapshotColumn{{Name: "a", Type: "Int64", Ordinary: true}, {Name: "b", Type: "Int64", Ordinary: true}}}, {Database: "t", Name: "other", ID: "other", Columns: []SnapshotColumn{{Name: "value", Type: "Int64", Ordinary: true}}}}
	for _, sql := range []string{"INSERT INTO t.dst SELECT b AS a FROM t.src WHERE a > 0", "INSERT INTO t.dst SELECT l.a AS a FROM t.src AS l ALL INNER JOIN t.src AS r ON l.a = r.a", "INSERT INTO t.dst SELECT (SELECT value FROM t.other) AS a FROM t.src WHERE a > 0"} {
		if p, err := AnalyzeSnapshot(e, sql, SnapshotOptions{Database: "t", Catalog: catalog}); err == nil {
			t.Fatalf("alias shadow admitted: %+v", p)
		}
	}
	p, err := AnalyzeSnapshot(e, "INSERT INTO t.dst SELECT r.value FROM t.other AS r ALL INNER JOIN t.src ON r.value = src.a", SnapshotOptions{Database: "t", Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	sql, err := p.Prepare(e, map[string][2]string{"src": {"scratch", "r"}, "other": {"scratch", "o"}})
	if err != nil || sql != "SELECT r.value FROM scratch.o AS r ALL INNER JOIN scratch.r AS src ON r.value = src.a" {
		t.Fatalf("scratch alias collision: %s %v", sql, err)
	}
	p, err = AnalyzeSnapshot(e, "INSERT INTO `t`.`dst` SELECT `a` FROM `t`.`src`", SnapshotOptions{Database: "t", Catalog: catalog})
	if err != nil || p.SQL != "INSERT INTO t.dst SELECT a FROM t.src" {
		t.Fatalf("quoted identity canonicalization: %+v %v", p, err)
	}
}
