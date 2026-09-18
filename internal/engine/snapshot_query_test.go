package engine

import (
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
