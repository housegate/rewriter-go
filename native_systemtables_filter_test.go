package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// System tables, step 2 (housegate/housegate#218): in dynamic mode a read of
// an allowed class (c) table — system.tables, columns, data_skipping_indices,
// projections, parts, mutations, databases — is served from a derived table
// that keeps only the rows of the caller's own logical databases, in
// ClickHouse's physical shape:
//
//	FROM system.tables  →  FROM (SELECT * FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.')) AS tables
//
// The filter is inside the derived table, out of reach of the caller's
// aliases; the derived table is aliased with the caller's alias, else the
// short table name, so `tables.name` keeps resolving.

// filteredTables is the derived table for system.<table> under
// tablerefDynamic: database_map {db1: phys}, delim "_".
func filteredTables(table string) string {
	col := "table"
	if table == "tables" {
		col = "name"
	}
	return "(SELECT * FROM system." + table + " WHERE database = 'phys' AND startsWith(" + col + ", 'db1.'))"
}

func TestSystemTablesFilter_EveryClassCTable(t *testing.T) {
	var cases []tablerefCase
	for _, table := range []string{"tables", "columns", "data_skipping_indices", "projections", "parts", "mutations"} {
		for _, si := range []bool{false, true} {
			cases = append(cases, tablerefCase{name: table, sql: "SELECT * FROM system." + table, si: si,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT * FROM " + filteredTables(table) + " AS " + table,
				wantAcc:  []string{"system." + table}})
		}
	}
	for _, si := range []bool{false, true} {
		cases = append(cases, tablerefCase{name: "databases", sql: "SELECT name FROM system.databases", si: si,
			wantCode: pb.RewriteCode_Success,
			wantSQL:  "SELECT name FROM (SELECT * FROM system.databases WHERE name IN ('phys')) AS databases"})
	}
	runTablerefCases(t, cases)
}

func TestSystemTablesFilter_Positions(t *testing.T) {
	ft := filteredTables("tables")
	fc := filteredTables("columns")
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "caller WHERE stays outside", si: si,
				sql:      "SELECT name FROM system.tables WHERE database = 'phys' AND name LIKE 'db1.%'",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT name FROM " + ft + " AS tables WHERE database = 'phys' AND name LIKE 'db1.%'"},
			tablerefCase{name: "user alias kept", si: si,
				sql:      "SELECT t.name FROM system.tables AS t",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT t.name FROM " + ft + " AS t"},
			tablerefCase{name: "short-name qualifier keeps resolving", si: si,
				sql:      "SELECT tables.name FROM system.tables",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT tables.name FROM " + ft + " AS tables"},
			tablerefCase{name: "backtick spelling", si: si,
				sql:      "SELECT name FROM `system`.`tables`",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT name FROM " + ft + " AS tables"},
			tablerefCase{name: "escaped name is the same table", si: si,
				sql:      "SELECT name FROM system.`tables\\N`",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT name FROM " + ft + " AS tables"},
			tablerefCase{name: "join with tenant table", si: si,
				sql:      "SELECT * FROM db1.o JOIN system.columns AS c ON c.name = o.a",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" "db1.o" JOIN ` + fc + ` AS c ON c.name = o.a`},
			tablerefCase{name: "join of two class (c) tables", si: si,
				sql:      "SELECT t.name, c.name FROM system.tables AS t JOIN system.columns AS c ON c.table = t.name",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT t.name, c.name FROM " + ft + " AS t JOIN " + fc + " AS c ON c.table = t.name"},
			tablerefCase{name: "FROM subquery", si: si,
				sql:      "SELECT n FROM (SELECT name AS n FROM system.tables)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT n FROM (SELECT name AS n FROM " + ft + " AS tables)"},
			tablerefCase{name: "CTE body", si: si,
				sql:      "WITH x AS (SELECT name FROM system.tables) SELECT * FROM x",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "WITH x AS (SELECT name FROM " + ft + " AS tables) SELECT * FROM x"},
			tablerefCase{name: "UNION arm", si: si,
				sql:      "SELECT name FROM system.tables UNION ALL SELECT name FROM system.columns",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT name FROM " + ft + " AS tables UNION ALL SELECT name FROM " + fc + " AS columns"},
			tablerefCase{name: "scalar subquery", si: si,
				sql:      "SELECT (SELECT count() FROM system.tables) AS n",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT (SELECT count() FROM " + ft + " AS tables) AS n"},
			tablerefCase{name: "IN subquery", si: si,
				sql:      "SELECT * FROM db1.o WHERE a IN (SELECT name FROM system.tables)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT name FROM ` + ft + ` AS tables)`},
			tablerefCase{name: "IN table operand", si: si,
				sql:      "SELECT * FROM db1.o WHERE a IN system.tables",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT * FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.'))`,
				wantAcc:  []string{"db1.o", "system.tables"}},
			tablerefCase{name: "view() body", si: si,
				sql:      "SELECT * FROM view(SELECT name FROM system.tables)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "SELECT * FROM view(SELECT name FROM " + ft + " AS tables)"},
		)
	}
	// Write statements' read bodies run the SELECT pipeline (SI off: the SI
	// surface refuses every write shape the corpus does not model).
	cases = append(cases,
		tablerefCase{name: "INSERT … SELECT", sql: "INSERT INTO db1.o SELECT name FROM system.tables",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `INSERT INTO phys."db1.o" SELECT name FROM ` + ft + ` AS tables`},
		tablerefCase{name: "CREATE VIEW body", sql: "CREATE VIEW db1.v AS SELECT name FROM system.tables",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `CREATE VIEW phys."db1.v" AS SELECT name FROM ` + ft + ` AS tables`},
	)
	runTablerefCases(t, cases)
}

// TestSystemTablesFilter_DatabaseMap pins the filter for the sessions
// HouseGate builds: several logical databases, several physical databases,
// an unauthenticated session (empty map: nothing is shown).
func TestSystemTablesFilter_DatabaseMap(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		name string
		dbs  map[string]string
		sql  string
		want string
	}{
		{"two logicals", map[string]string{"db2": "phys", "db1": "phys"}, "SELECT name FROM system.tables",
			"SELECT name FROM (SELECT * FROM system.tables WHERE (database = 'phys' AND startsWith(name, 'db1.')) OR (database = 'phys' AND startsWith(name, 'db2.'))) AS tables"},
		{"two physicals", map[string]string{"db1": "phys", "a": "other"}, "SELECT name FROM system.databases",
			"SELECT name FROM (SELECT * FROM system.databases WHERE name IN ('other', 'phys')) AS databases"},
		{"no database", map[string]string{}, "SELECT name FROM system.tables",
			"SELECT name FROM (SELECT * FROM system.tables WHERE 0) AS tables"},
		{"no database, databases", nil, "SELECT name FROM system.databases",
			"SELECT name FROM (SELECT * FROM system.databases WHERE 0) AS databases"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := tablerefDynamic(false)
			dyn.DatabaseMap = tc.dbs
			dyn.UpstreamLogicalDatabaseInContext = ""
			resp, err := doRewrite(e, tc.sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != tc.want {
				t.Fatalf("code = %s (%s)\n got %s\nwant %s", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite(), tc.want)
			}
		})
	}
}

// TestSystemTablesFilter_RefusedShapes: a shape the derived table cannot
// carry is refused rather than forwarded unfiltered, and the table
// functions that would read a class (c) table directly stay refused by T5.
func TestSystemTablesFilter_RefusedShapes(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT * FROM system.parts FINAL",
			"SELECT * FROM system.parts SAMPLE 0.5",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: engine.UnsupportedStatementMessage})
		}
		for _, tc := range []struct{ sql, fn string }{
			{"SELECT * FROM remote('127.0.0.1', system.tables)", "remote"},
			{"SELECT * FROM remote('127.0.0.1', system, tables)", "remote"},
			{"SELECT * FROM cluster('default', system.parts)", "cluster"},
			{"SELECT * FROM clusterAllReplicas('default', system.mutations)", "clusterAllReplicas"},
			{"SELECT * FROM loop(system.tables) LIMIT 1", "loop"},
		} {
			cases = append(cases, tablerefCase{name: tc.sql, sql: tc.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function " + tc.fn + " is not accepted"})
		}
		// A merge() pattern is never an allowlist match: the system-table
		// step (3a) refuses it before T5.
		cases = append(cases, tablerefCase{name: "merge pattern", sql: "SELECT * FROM merge(system, '^tables$')", si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "system table system.^tables$ is not accessible"})
	}
	runTablerefCases(t, cases)
}

// TestSystemTablesFilter_StaticAndNoRewriteUntouched: like step 1, the
// filter applies to dynamic mode only. HouseGate always sends dynamic args;
// a static-mode or option-less request has no tenant to filter for.
func TestSystemTablesFilter_StaticAndNoRewriteUntouched(t *testing.T) {
	e := newEngine(t)
	sql := "SELECT name FROM system.tables WHERE database = 'phys'"
	for name, opts := range map[string][]*pb.RewriteOption{
		"static": {tableRewriteStatic()},
		"none":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != sql {
				t.Fatalf("code = %s (%s), sql = %s", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
			}
		})
	}
}

// TestSystemTablesFilter_DriverStatements runs every driver statement that
// reads a class (c) table (testdata/driver_statements.json) in the driver
// session: each keeps its own filter and gains the tenant filter for its one
// logical database, hw1kzqe3_0 → devnet2.
func TestSystemTablesFilter_DriverStatements(t *testing.T) {
	e := newEngine(t)
	n := 0
	for _, s := range loadDriverStatements(t) {
		if s.Expect != "success" {
			continue
		}
		var table string
		for _, tbl := range []string{"tables", "columns", "data_skipping_indices", "projections", "parts", "mutations", "databases"} {
			if strings.Contains(s.SQL, "FROM system."+tbl+" ") || strings.HasSuffix(s.SQL, "FROM system."+tbl) {
				table = tbl
			}
		}
		if table == "" {
			continue
		}
		n++
		col := "table"
		if table == "tables" {
			col = "name"
		}
		derived := "(SELECT * FROM system." + table + " WHERE database = 'devnet2' AND startsWith(" + col + ", 'hw1kzqe3_0.')) AS " + table
		for _, si := range []bool{false, true} {
			resp, err := doRewrite(e, s.SQL, []*pb.RewriteOption{tableRewriteDynamic(driverDynamic(si))})
			if err != nil {
				t.Fatal(err)
			}
			got := resp.GetSqlAfterRewrite()
			if resp.GetCode() != pb.RewriteCode_Success || !strings.Contains(got, "FROM "+derived+" WHERE database = 'devnet2' AND ") {
				t.Fatalf("si=%v %d %s: code = %s (%s)\nsql = %s\nwant FROM %s WHERE database = 'devnet2' AND …", si, s.ID, s.Shape, resp.GetCode(), resp.GetMessage(), got, derived)
			}
		}
	}
	if n != 14 {
		t.Fatalf("driver class (c) statements = %d, want 14", n)
	}
}
