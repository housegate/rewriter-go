package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// cteMutationPositions are the spec 2026-09-26 R2 expression positions: a
// lightweight DELETE / UPDATE, an ALTER … DELETE / UPDATE mutation (opaque
// text, whose storage-integrity read surface is re-parsed), a CHECK / ASSUME
// constraint (in CREATE and in ALTER ADD CONSTRAINT), a table or column TTL,
// an INDEX expression, a column DEFAULT / MATERIALIZED expression, a storage
// key, an ALTER ADD COLUMN default, INSERT VALUES rows, a materialized view's
// inner-table TTL, ORDER BY / PRIMARY KEY / SAMPLE BY, a PROJECTION, an ALIAS
// column, CTAS column / constraint expressions, and scalar-subquery / EXISTS
// operands. %S is the operand.
var cteMutationPositions = []struct{ name, sql string }{
	{"lightweight_delete", "DELETE FROM db1.o WHERE a IN %S"},
	{"update_where", "UPDATE db1.o SET b = 1 WHERE a IN %S"},
	{"update_set", "UPDATE db1.o SET b = a IN %S WHERE 1"},
	{"alter_delete", "ALTER TABLE db1.o DELETE WHERE a IN %S"},
	{"alter_update", "ALTER TABLE db1.o UPDATE b = 1 WHERE a IN %S"},
	{"create_check", "CREATE TABLE db1.n (a Int64, CONSTRAINT k CHECK a IN %S) ENGINE = MergeTree ORDER BY a"},
	{"create_assume", "CREATE TABLE db1.n (a Int64, CONSTRAINT k ASSUME a IN %S) ENGINE = MergeTree ORDER BY a"},
	{"alter_add_check", "ALTER TABLE db1.o ADD CONSTRAINT k CHECK a IN %S"},
	{"alter_add_assume", "ALTER TABLE db1.o ADD CONSTRAINT k ASSUME a IN %S"},
	{"table_ttl_where", "CREATE TABLE db1.n (a Int64, d DateTime) ENGINE = MergeTree ORDER BY a TTL d + INTERVAL 1 DAY DELETE WHERE a IN %S"},
	{"table_ttl_expr", "CREATE TABLE db1.n (a Int64, d DateTime) ENGINE = MergeTree ORDER BY a TTL d + toIntervalDay(a IN %S)"},
	{"column_ttl", "CREATE TABLE db1.n (a Int64, d DateTime, b Int64 TTL if(a IN %S, d, d + INTERVAL 100 YEAR)) ENGINE = MergeTree ORDER BY a"},
	{"create_index", "CREATE TABLE db1.n (a Int64, INDEX i (a IN %S) TYPE set(10) GRANULARITY 1) ENGINE = MergeTree ORDER BY a"},
	{"column_default", "CREATE TABLE db1.n (a Int64, b UInt8 DEFAULT a IN %S) ENGINE = MergeTree ORDER BY a"},
	{"column_materialized", "CREATE TABLE db1.n (a Int64, b UInt8 MATERIALIZED a IN %S) ENGINE = MergeTree ORDER BY a"},
	{"partition_by", "CREATE TABLE db1.n (a Int64) ENGINE = MergeTree PARTITION BY a IN %S ORDER BY a"},
	{"alter_add_column_default", "ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a IN %S"},
	{"insert_values", "INSERT INTO db1.o VALUES (1, 1 IN %S)"},
	{"mv_inner_ttl", "CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a TTL d + INTERVAL 1 DAY DELETE WHERE a IN %S AS SELECT a, d FROM db1.o"},
	// Storage keys: on 25.8 ClickHouse reads the table there, resolved
	// against the default database; 26.2 refuses a subquery in a key.
	{"order_by", "CREATE TABLE db1.n (a Int64) ENGINE = MergeTree ORDER BY a IN %S"},
	{"primary_key", "CREATE TABLE db1.n (a Int64) ENGINE = MergeTree PRIMARY KEY a IN %S ORDER BY a"},
	{"sample_by", "CREATE TABLE db1.n (a Int64) ENGINE = MergeTree ORDER BY a SAMPLE BY a IN %S"},
	{"create_projection", "CREATE TABLE db1.n (a Int64, PROJECTION p (SELECT a WHERE a IN %S)) ENGINE = MergeTree ORDER BY a"},
	{"alter_add_projection", "ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE a IN %S)"},
	{"alias_column", "CREATE TABLE db1.n (a Int64, b UInt8 ALIAS a IN %S) ENGINE = MergeTree ORDER BY a"},
	// A CREATE TABLE … AS SELECT keeps binding in its body, but its column
	// and constraint expressions are R2 positions.
	{"ctas_column_default", "CREATE TABLE db1.n (a Int64, b UInt8 DEFAULT a IN %S) ENGINE = MergeTree ORDER BY a AS SELECT a FROM db1.o"},
	{"ctas_check", "CREATE TABLE db1.n (a Int64, CONSTRAINT k CHECK a IN %S) ENGINE = MergeTree ORDER BY a AS SELECT a FROM db1.o"},
	// Scalar-subquery and EXISTS forms: the operand is the subquery itself
	// (UPDATE SET b = (WITH x … SELECT a FROM x) sets b from the table x).
	{"update_set_scalar", "UPDATE db1.o SET b = %S WHERE 1"},
	{"delete_where_scalar", "DELETE FROM db1.o WHERE a = %S"},
	{"delete_where_exists", "DELETE FROM db1.o WHERE EXISTS %S"},
	{"insert_values_scalar", "INSERT INTO db1.o VALUES (1, %S)"},
}

// cteOperandForms declare %D and read %N inside the IN operand: a CTE read
// from FROM, as a bare IN operand, recursively and one subquery level deeper,
// then a WITH expression alias and a projection alias read as an IN operand.
var cteOperandForms = []struct{ name, operand string }{
	{"from", "(WITH %D AS (SELECT 1 AS a) SELECT a FROM %N)"},
	{"in", "(WITH %D AS (SELECT 1 AS a) SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN %N)"},
	{"recursive", "(WITH RECURSIVE %D AS (SELECT 1 AS a UNION ALL SELECT a + 1 FROM %N WHERE a < 1) SELECT a FROM %N)"},
	{"nested", "(SELECT a FROM (WITH %D AS (SELECT 1 AS a) SELECT a FROM %N))"},
	// WITH expression aliases and projection aliases do not bind there
	// either: ClickHouse 26.2 and 25.8 read the table for a literal, a
	// function call, a tuple and a scalar subquery value, and for a
	// projection alias, in a DELETE and a CHECK constraint.
	{"with_literal_alias", "(WITH 1 AS %D SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN %N)"},
	{"with_function_alias", "(WITH (toInt64(1)) AS %D SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN %N)"},
	{"with_scalar_alias", "(WITH (SELECT 1) AS %D SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN %N)"},
	{"projection_alias", "(SELECT 1 AS %D FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN %N)"},
}

// cteNames are read as tables when the CTE does not bind: a plain name, a
// dotted quoted one-part name that names another tenant's physical table,
// and the Active storage-integrity table's own name.
var cteNames = []string{"x", "`db2.my-t`", "t"}

func cteSQL(position, form, declared, read string) string {
	operand := strings.ReplaceAll(strings.ReplaceAll(form, "%D", declared), "%N", read)
	return strings.ReplaceAll(position, "%S", operand)
}

// TestTableRef_CTEDoesNotBindInMutationPositions: ClickHouse 26.2 and 25.8
// qualify every table name in a mutation, constraint, TTL, index or column
// expression with the current database before a WITH clause is consulted, so
// `DELETE … WHERE a IN (WITH x AS (…) SELECT a FROM x)` reads the physical
// table x, under both analyzers. The engine must answer such a statement
// exactly as it answers the same statement whose CTE is declared under an
// unrelated name, so the operand is an ordinary table there, never a success.
func TestTableRef_CTEDoesNotBindInMutationPositions(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, p := range cteMutationPositions {
			for _, f := range cteOperandForms {
				for _, n := range cteNames {
					sql := cteSQL(p.sql, f.operand, n, n)
					twin := cteSQL(p.sql, f.operand, "zz_unrelated", n)
					name := map[bool]string{false: "off", true: "si"}[si] + "/" + p.name + "/" + f.name + "/" + n
					t.Run(name, func(t *testing.T) {
						got, err := doRewrite(e, sql, tablerefOpts(si))
						if err != nil {
							t.Fatalf("doRewrite(%q): %v", sql, err)
						}
						want, err := doRewrite(e, twin, tablerefOpts(si))
						if err != nil {
							t.Fatalf("doRewrite(%q): %v", twin, err)
						}
						if want.GetCode() == pb.RewriteCode_Success {
							t.Fatalf("twin %q answered Success; the position is not an R2 refusal", twin)
						}
						if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
							t.Fatalf("%q\n got  %s %q\n want %s %q (as %q)", sql,
								got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage(), twin)
						}
						if got.GetSqlAfterRewrite() != sql {
							t.Fatalf("rejection sql = %q, want the caller's %q", got.GetSqlAfterRewrite(), sql)
						}
					})
				}
			}
		}
	}
}

// TestTableRef_CTEInMutationPositionExactAnswers pins the reported shapes: a
// DELETE or CHECK constraint whose IN operand reads a CTE named after another
// tenant's table is refused with "statement is not supported" in both SI
// states, and a CTE named after the Active SI table gets the answer the
// Active table itself gets there.
func TestTableRef_CTEInMutationPositionExactAnswers(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"DELETE FROM db1.o WHERE a IN (WITH `db2.my-t` AS (SELECT 1 AS a) SELECT a FROM `db2.my-t`)",
			"DELETE FROM db1.o WHERE a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)",
			"UPDATE db1.o SET b = 1 WHERE a IN (WITH RECURSIVE x AS (SELECT 1 AS a UNION ALL SELECT a + 1 FROM x WHERE a < 1) SELECT a FROM x)",
			"CREATE TABLE db1.n (a Int64, CONSTRAINT k CHECK a IN (WITH `db2.my-t` AS (SELECT 1 AS a) SELECT a FROM `db2.my-t`)) ENGINE = MergeTree ORDER BY a",
			"ALTER TABLE db1.o ADD CONSTRAINT k CHECK a IN (WITH `db2.my-t` AS (SELECT 1 AS a) SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN `db2.my-t`)",
			"CREATE TABLE db1.n (a Int64, d DateTime) ENGINE = MergeTree ORDER BY a TTL d + INTERVAL 1 DAY DELETE WHERE a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)",
			"CREATE TABLE db1.n (a Int64, INDEX i (a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)) TYPE set(10) GRANULARITY 1) ENGINE = MergeTree ORDER BY a",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_CTEStillBindsInReadBodies: in a SELECT, an INSERT … SELECT
// body, a CREATE TABLE … AS SELECT body and a view body ClickHouse binds the
// CTE, both when the statement's own WITH declares it and when a subquery
// does, so the name is not a table there: the statement succeeds and the CTE
// name is neither rewritten nor reported.
func TestTableRef_CTEStillBindsInReadBodies(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct {
			sql, want string
			acc       []string
		}{
			{"SELECT a FROM db1.o WHERE a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)",
				`SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)`, []string{"db1.o"}},
			{"WITH x AS (SELECT 1 AS a) SELECT a FROM db1.o WHERE a IN x",
				`WITH x AS (SELECT 1 AS a) SELECT a FROM phys."db1.o" "db1.o" WHERE a IN x`, []string{"db1.o"}},
			{"INSERT INTO db1.o SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN (WITH x AS (SELECT 1 AS a) SELECT a FROM x)",
				"", []string{"db1.o"}},
			{"INSERT INTO db1.o WITH x AS (SELECT 1 AS a) SELECT a FROM db1.p WHERE a IN x",
				"", []string{"db1.o", "db1.p"}},
			{"CREATE TABLE db1.n ENGINE = MergeTree ORDER BY v AS SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN (WITH RECURSIVE x AS (SELECT 1 AS a UNION ALL SELECT a + 1 FROM x WHERE a < 1) SELECT a FROM x)",
				"", []string{"db1.n"}},
			{"CREATE VIEW db1.vv AS SELECT v FROM (SELECT arrayJoin([1, 7, 42]) AS v) WHERE v IN (WITH `db2.my-t` AS (SELECT 1 AS a) SELECT a FROM `db2.my-t`)",
				"", []string{"db1.vv"}},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: c.acc})
		}
	}
	runTablerefCases(t, cases)
}
