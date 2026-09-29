package rewriter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// A table alias never binds an IN operand (spec 2026-09-26 §5, IN row).
//
// Measured on ClickHouse 26.2.15.4, for `a IN N`, `a IN (N)`, `a IN "N"`,
// `a NOT IN N`, `a GLOBAL IN N`, `in` / `notIn` / `globalIn(a, N)`, with both
// enable_analyzer=1 and enable_analyzer=0:
//
//   - N is a table-source alias (FROM / JOIN table, subquery, table function)
//     of an enclosing SELECT, at any depth, including a scalar subquery and a
//     FROM / JOIN subquery: ClickHouse reads the table named N in the current
//     database under both analyzers.
//   - N is a table-source alias of the IN's own SELECT: the new analyzer reads
//     the aliased source, the old analyzer (enable_analyzer=0, which a tenant
//     sets per query or per session) reads the table named N.
//   - N is a projection alias of an enclosing SELECT: the new analyzer reads
//     the expression, the old analyzer reads the table named N.
//   - N is a CTE name or a WITH expression alias (any enclosing scope), or a
//     projection alias of the IN's own SELECT: both analyzers read the CTE or
//     the expression, even when a table alias of the same name shadows it.
//
// The current database is the physical database housegate selects, so an
// operand forwarded verbatim read `phys."other.secret"` (another tenant's
// table) or the ordinary physical table of an Active storage-integrity table,
// unreported. Only the last group may stay unrewritten; every other operand
// is a table in the session's logical database, rewritten, reported and
// storage-integrity-checked like `a IN b`.

// aliasInName is one alias spelling: decl is how the SQL writes it, gen how
// Polyglot generates it back.
type aliasInName struct{ decl, gen string }

var aliasInNames = []aliasInName{
	{`"other.secret"`, `"other.secret"`},
	{"`db2.x`", `"db2.x"`},
	{"t", "t"},
}

// aliasInDecls declare {N} as an alias and use it through the predicate {P}.
var aliasInDecls = []struct{ name, sql string }{
	{"outer table", "SELECT * FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"outer table two levels", "SELECT * FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE a IN (SELECT a FROM db1.p WHERE {P}))"},
	{"same scope table", "SELECT * FROM db1.o AS {N} WHERE {P}"},
	{"same scope table in select list", "SELECT {P} FROM db1.o AS {N}"},
	{"inner table", "SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p AS {N} WHERE {P})"},
	{"outer join table", "SELECT * FROM db1.o JOIN db1.p AS {N} USING (a) WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"outer subquery", "SELECT * FROM (SELECT a FROM db1.o) AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"outer table function", "SELECT * FROM numbers(3) AS {N} WHERE number IN (SELECT a FROM db1.p WHERE {P})"},
	{"scalar subquery", "SELECT (SELECT count() FROM db1.p WHERE {P}) FROM db1.o AS {N}"},
	{"join subquery", "SELECT * FROM db1.o AS {N} JOIN (SELECT a FROM db1.p WHERE {P}) AS j USING (a)"},
	{"union arm", "SELECT a FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P} UNION ALL SELECT 1)"},
	{"cte body", "SELECT * FROM db1.o AS {N} WHERE a IN (WITH w AS (SELECT a FROM db1.p WHERE {P}) SELECT a FROM w)"},
	{"outer projection", "SELECT a, 1 AS {N} FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"insert select", "INSERT INTO db1.q SELECT * FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"create view", "CREATE VIEW db1.v AS SELECT * FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"ctas", "CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.o AS {N} WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
}

var aliasInPredicates = []string{
	"a IN {R}",
	"a IN ({R})",
	"a NOT IN {R}",
	"a GLOBAL IN {R}",
	"(a, a) IN {R}",
	"in(a, {R})",
	"notIn(a, {R})",
	"globalIn(a, {R})",
}

// aliasInControl is an alias no operand names: the control statement declares
// it instead of N, so N is an ordinary unqualified IN operand there.
const aliasInControl = "zz_alias_control"

func aliasInContexts() map[string]func(si bool) []*pb.RewriteOption {
	phys := "phys"
	with := func(ctx string) func(si bool) []*pb.RewriteOption {
		return func(si bool) []*pb.RewriteOption {
			dyn := tablerefDynamic(si)
			dyn.UpstreamLogicalDatabaseInContext = ctx
			dyn.UpstreamPhysicalDatabaseInContext = &phys
			return []*pb.RewriteOption{tableRewriteDynamic(dyn)}
		}
	}
	return map[string]func(si bool) []*pb.RewriteOption{
		"mapped":   with("db1"),
		"unmapped": with("db9"),
		"empty":    with(""),
		"mapped without physical context": func(si bool) []*pb.RewriteOption {
			return tablerefOpts(si)
		},
	}
}

func accessedKeys(resp *pb.RewriteSQLResponse) []string {
	out := []string{}
	for _, a := range resp.GetOriginalAccessedTables() {
		out = append(out, a.GetOriginalDatabase()+"."+a.GetOriginalTable()+"@"+
			a.GetLogicalDatabase()+"/"+a.GetPhysicalDatabase())
	}
	return out
}

// TestTableRef_TableAliasIsNotAnInOperand checks that declaring N as a table
// alias (or as a projection alias of an enclosing SELECT) changes nothing about
// how an IN operand named N is handled: the answer equals the answer for the
// same statement with the alias renamed, in every context and both SI states.
func TestTableRef_TableAliasIsNotAnInOperand(t *testing.T) {
	e := newEngine(t)
	for ctxName, opts := range aliasInContexts() {
		for _, si := range []bool{false, true} {
			for _, decl := range aliasInDecls {
				for _, name := range aliasInNames {
					for _, pred := range aliasInPredicates {
						p := strings.ReplaceAll(pred, "{R}", name.decl)
						sql := strings.ReplaceAll(strings.ReplaceAll(decl.sql, "{P}", p), "{N}", name.decl)
						control := strings.ReplaceAll(strings.ReplaceAll(decl.sql, "{P}", p), "{N}", aliasInControl)
						label := ctxName + "/" + map[bool]string{false: "si off", true: "si v2"}[si] + "/" + decl.name + "/" + sql
						t.Run(label, func(t *testing.T) {
							got, err := doRewrite(e, sql, opts(si))
							if err != nil {
								t.Fatalf("doRewrite: %v", err)
							}
							want, err := doRewrite(e, control, opts(si))
							if err != nil {
								t.Fatalf("doRewrite control: %v", err)
							}
							if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
								t.Fatalf("code = %s (%s), want %s (%s)\n  sql: %s\n  out: %s",
									got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage(), sql, got.GetSqlAfterRewrite())
							}
							wantSQL := strings.ReplaceAll(want.GetSqlAfterRewrite(), aliasInControl, name.gen)
							if want.GetCode() != pb.RewriteCode_Success {
								wantSQL = sql
							}
							if got.GetSqlAfterRewrite() != wantSQL {
								t.Fatalf("sql = %s\nwant  %s", got.GetSqlAfterRewrite(), wantSQL)
							}
							if g, w := accessedKeys(got), accessedKeys(want); !reflect.DeepEqual(g, w) {
								t.Fatalf("accessed = %v, want %v", g, w)
							}
							if !reflect.DeepEqual(got.GetTableRewrites(), want.GetTableRewrites()) {
								t.Fatalf("table_rewrites = %v, want %v", got.GetTableRewrites(), want.GetTableRewrites())
							}
						})
					}
				}
			}
		}
	}
}

// TestTableRef_CorrelatedAliasInOperandPins pins the exact answer for the
// review's finding F1 and its storage-integrity twins with the request shape
// housegate sends (db1 → phys, physical context phys).
func TestTableRef_CorrelatedAliasInOperandPins(t *testing.T) {
	const (
		safeT = `(SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`
	)
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		tRead := `phys."db1.t"`
		if si {
			tRead = safeT
		}
		cases = append(cases,
			tablerefCase{name: "F1 other.secret", si: si,
				sql:      `SELECT * FROM db1.o AS "other.secret" WHERE a IN (SELECT a FROM db1.p WHERE a IN "other.secret")`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" AS "other.secret" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN phys."db1.other.secret")`,
				wantAcc:  []string{"db1.o", "db1.p", ".other.secret"}},
			tablerefCase{name: "F1 callable db2.x", si: si,
				sql:      "SELECT * FROM db1.o AS `db2.x` WHERE a IN (SELECT a FROM db1.p WHERE in(a, `db2.x`))",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" AS "db2.x" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE in(a, phys."db1.db2.x"))`,
				wantAcc:  []string{"db1.o", "db1.p", ".db2.x"}},
			tablerefCase{name: "F1 active table name", si: si,
				sql:      "SELECT * FROM db1.o AS t WHERE a IN (SELECT a FROM db1.p WHERE a IN t)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" AS t WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN ` + tRead + `)`,
				wantAcc:  []string{"db1.o", "db1.p", ".t"}},
			tablerefCase{name: "same scope alias", si: si,
				sql:      `SELECT * FROM db1.o AS "other.secret" WHERE a IN "other.secret" SETTINGS enable_analyzer = 0`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT * FROM phys."db1.o" AS "other.secret" WHERE a IN phys."db1.other.secret" SETTINGS enable_analyzer = 0`,
				wantAcc:  []string{"db1.o", ".other.secret"}},
			tablerefCase{name: "outer projection alias", si: si,
				sql:      `SELECT a, 1 AS t FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN t)`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a, 1 AS t FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN ` + tRead + `)`,
				wantAcc:  []string{"db1.o", "db1.p", ".t"}},
		)
		// Names ClickHouse binds before any table: CTEs and WITH expression
		// aliases in every enclosing scope (even under a same-named table
		// alias), and the IN's own SELECT's projection aliases. They stay
		// untouched and unreported.
		for _, c := range []struct{ sql, want string }{
			{"WITH c AS (SELECT 1 AS a) SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN c)",
				`WITH c AS (SELECT 1 AS a) SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN c)`},
			{"WITH 1 AS c SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN c)",
				`WITH 1 AS c SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN c)`},
			{"WITH (SELECT 1) AS c SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE in(a, c))",
				`WITH (SELECT 1) AS c SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE in(a, c))`},
			{"WITH c AS (SELECT 1 AS a) SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p AS c WHERE a IN c)",
				`WITH c AS (SELECT 1 AS a) SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" AS c WHERE a IN c)`},
			{"WITH 1 AS c SELECT * FROM db1.o AS c WHERE a IN c",
				`WITH 1 AS c SELECT * FROM phys."db1.o" AS c WHERE a IN c`},
			{"SELECT a, 1 AS c FROM db1.o WHERE a IN c",
				`SELECT a, 1 AS c FROM phys."db1.o" "db1.o" WHERE a IN c`},
			{"SELECT a, 1 AS c FROM db1.o AS c WHERE a GLOBAL IN (c)",
				`SELECT a, 1 AS c FROM phys."db1.o" AS c WHERE a GLOBAL IN (c)`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: []string{"db1.o", "db1.p"}})
		}
	}
	for i := range cases {
		if strings.Contains(cases[i].sql, "db1.p") {
			continue
		}
		if cases[i].wantAcc != nil && reflect.DeepEqual(cases[i].wantAcc, []string{"db1.o", "db1.p"}) {
			cases[i].wantAcc = []string{"db1.o"}
		}
	}
	runTablerefCases(t, cases)
}
