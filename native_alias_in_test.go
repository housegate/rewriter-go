package rewriter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
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
//   - N is a WITH expression alias whose value is an identifier (bare, quoted,
//     parenthesised, qualified, unresolvable) or another alias: the new
//     analyzer reads the table the identifier names in the same scope, and
//     the table named N when nested or when the identifier does not resolve;
//     the old analyzer substitutes the identifier in a nested scope and reads
//     it as a table.
//   - N is a CTE name, a WITH expression alias whose value is a literal, a
//     tuple or array of literals, a function call or a subquery (any enclosing
//     scope), or a projection alias of the IN's own SELECT: both analyzers read
//     the CTE or the expression, even when a table alias of the same name
//     shadows it.
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
						runAliasInMetamorphic(t, e, ctxName, opts(si), si, decl.name, decl.sql, name, pred)
					}
				}
			}
		}
	}
}

// aliasInIdentifierValues are WITH values that are, or end in, an identifier:
// bare, quoted, the quoted twin of the Active SI table, parenthesised,
// qualified (a protected database), a dotted two-part name, and unresolvable.
var aliasInIdentifierValues = []string{
	"secret", `"other.secret"`, "`db1.t`", "(secret)", "phys.secret", "db2.x", "nosuch",
}

// aliasInWithDecls declare {N} as a WITH expression alias with value {V}.
var aliasInWithDecls = []struct{ name, sql string }{
	{"with same scope", "WITH {V} AS {N} SELECT * FROM db1.o WHERE {P}"},
	{"with enclosing scope", "WITH {V} AS {N} SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"with two levels up", "WITH {V} AS {N} SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN (SELECT a FROM db1.p WHERE {P}))"},
	{"with on the subquery", "SELECT * FROM db1.o WHERE a IN (WITH {V} AS {N} SELECT a FROM db1.p WHERE {P})"},
	{"with under a same-named table alias", "WITH {V} AS {N} SELECT * FROM db1.o AS {N} WHERE {P}"},
	{"with chain", "WITH {V} AS w, w AS {N} SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
}

// TestTableRef_IdentifierWithAliasIsNotAnInBinding checks that a WITH alias
// whose value is an identifier, or a chain of aliases ending in one (or in
// anything: a chain value is itself an identifier), does not bind an IN
// operand. Measured on ClickHouse 26.2: the new analyzer resolves the operand
// through such an alias to the table the identifier names (or, nested or when
// the identifier does not resolve, to the table named after the alias), and
// the old analyzer substitutes the identifier in a nested scope and reads it
// as a table. The answer must equal the answer with the alias renamed.
func TestTableRef_IdentifierWithAliasIsNotAnInBinding(t *testing.T) {
	e := newEngine(t)
	for ctxName, opts := range aliasInContexts() {
		for _, si := range []bool{false, true} {
			for _, decl := range aliasInWithDecls {
				values := aliasInIdentifierValues
				if decl.name == "with chain" {
					values = []string{"secret", "1"}
				}
				for _, v := range values {
					for _, name := range aliasInNames {
						for _, pred := range aliasInPredicates {
							sql := strings.ReplaceAll(decl.sql, "{V}", v)
							runAliasInMetamorphic(t, e, ctxName, opts(si), si, decl.name, sql, name, pred)
						}
					}
				}
			}
		}
	}
}

func runAliasInMetamorphic(t *testing.T, e engine.Engine, ctxName string, opts []*pb.RewriteOption,
	si bool, declName, declSQL string, name aliasInName, pred string) {
	t.Helper()
	p := strings.ReplaceAll(pred, "{R}", name.decl)
	sql := strings.ReplaceAll(strings.ReplaceAll(declSQL, "{P}", p), "{N}", name.decl)
	control := strings.ReplaceAll(strings.ReplaceAll(declSQL, "{P}", p), "{N}", aliasInControl)
	label := ctxName + "/" + map[bool]string{false: "si off", true: "si v2"}[si] + "/" + declName + "/" + sql
	t.Run(label, func(t *testing.T) {
		got, err := doRewrite(e, sql, opts)
		if err != nil {
			t.Fatalf("doRewrite: %v", err)
		}
		want, err := doRewrite(e, control, opts)
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
		// A WITH alias whose value is an identifier binds nothing (review round
		// 1, F1): ClickHouse reads a table through it. So does a chain, even one
		// that ends in a constant, and an identifier-valued WITH shadows an
		// enclosing constant one of the same name.
		cases = append(cases,
			tablerefCase{name: "with identifier value named like the operand", si: si,
				sql:      `WITH nosuch AS "other.secret" SELECT a FROM db1.o WHERE a IN "other.secret"`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH nosuch AS "other.secret" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.other.secret"`,
				wantAcc:  []string{"db1.o", ".other.secret"}},
			tablerefCase{name: "with quoted SI twin value", si: si,
				sql:      "WITH `db1.t` AS z SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN z)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH "db1.t" AS z SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN phys."db1.z")`,
				wantAcc:  []string{"db1.o", "db1.p", ".z"}},
			tablerefCase{name: "with chain to a constant", si: si,
				sql:      "WITH 1 AS w, w AS t SELECT a FROM db1.o WHERE a IN t",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH 1 AS w, w AS t SELECT a FROM phys."db1.o" "db1.o" WHERE a IN ` + tRead,
				wantAcc:  []string{"db1.o", ".t"}},
			tablerefCase{name: "identifier with shadows an enclosing constant with", si: si,
				sql:      "WITH 1 AS t SELECT a FROM db1.o WHERE a IN (WITH secret AS t SELECT a FROM db1.p WHERE in(a, t))",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH 1 AS t SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (WITH secret AS t SELECT a FROM phys."db1.p" "db1.p" WHERE in(a, ` + tRead + `))`,
				wantAcc:  []string{"db1.o", "db1.p", ".t"}},
		)
		// Names ClickHouse binds before any table: CTEs, WITH expression
		// aliases whose value is provably not a table reference (a literal, a
		// tuple or array of literals, a function call, a subquery) in every
		// enclosing scope, even under a same-named table alias, and the IN's
		// own SELECT's projection aliases. They stay untouched and unreported.
		for _, c := range []struct{ sql, want string }{
			{"WITH (1, 2) AS s SELECT * FROM db1.o WHERE a IN s",
				`WITH (1, 2) AS s SELECT * FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"WITH (1, 2) AS s SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN s)",
				`WITH (1, 2) AS s SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN s)`},
			{"WITH (SELECT 1) AS s SELECT * FROM db1.o WHERE a IN s",
				`WITH (SELECT 1) AS s SELECT * FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"WITH [1, 2] AS s SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a GLOBAL IN s)",
				`WITH [1, 2] AS s SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a GLOBAL IN s)`},
			{"WITH ((-1, 'x')) AS s SELECT * FROM db1.o WHERE (a, b) IN s",
				`WITH ((-1, 'x')) AS s SELECT * FROM phys."db1.o" "db1.o" WHERE (a, b) IN s`},
			{"WITH identity(secret) AS s SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE notIn(a, s))",
				`WITH identity(secret) AS s SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE notIn(a, s))`},
			{"WITH NULL AS s SELECT * FROM db1.o AS s WHERE a IN s",
				`WITH NULL AS s SELECT * FROM phys."db1.o" AS s WHERE a IN s`},
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

// aliasInKeywords are the bare SQL keywords Polyglot parses as a no-parens
// function call (`{"function": {"name": "NOW", "no_parens": true}}`) while
// ClickHouse 26.2 has no niladic-keyword syntax and reads each as an ordinary,
// usually unresolvable identifier (review round 2, N1).
var aliasInKeywords = []string{
	"CURDATE", "CURRENT_CATALOG", "CURRENT_DATE", "CURRENT_DATETIME", "CURRENT_ROLE",
	"CURRENT_SCHEMA", "CURRENT_TIME", "CURRENT_USER", "GETDATE", "LOCALTIME",
	"LOCALTIMESTAMP", "NOW", "PI", "SESSION_USER", "SYSDATE", "SYSDATETIME",
	"SYSTEM_USER", "SYSTIMESTAMP", "UTC_DATE", "UTC_TIME", "UTC_TIMESTAMP",
}

// TestTableRef_KeywordWithAliasIsNotAnInBinding checks that a WITH alias whose
// value is a bare keyword (upper or lower case, parenthesised or not) does not
// bind an IN operand. Measured on ClickHouse 26.2, `WITH NOW AS z … a IN z`
// reads the table named z under the new analyzer (same scope and nested), and
// the old analyzer substitutes the keyword in a nested scope and reads the
// table named after it. The answer must equal the answer with the alias
// renamed.
func TestTableRef_KeywordWithAliasIsNotAnInBinding(t *testing.T) {
	e := newEngine(t)
	contexts := aliasInContexts()
	decls := []struct{ name, sql string }{
		{"with same scope", "WITH {V} AS {N} SELECT * FROM db1.o WHERE {P}"},
		{"with enclosing scope", "WITH {V} AS {N} SELECT * FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
		{"with under a same-named table alias", "WITH {V} AS {N} SELECT * FROM db1.o AS {N} WHERE {P}"},
	}
	preds := []string{"a IN {R}", "a NOT IN ({R})", "globalIn(a, {R})"}
	for _, ctxName := range []string{"mapped", "empty"} {
		opts := contexts[ctxName]
		for _, si := range []bool{false, true} {
			for _, decl := range decls {
				for _, kw := range aliasInKeywords {
					for _, v := range []string{kw, strings.ToLower(kw), "(" + kw + ")", "((" + strings.ToLower(kw) + "))"} {
						for _, name := range aliasInNames {
							for _, pred := range preds {
								sql := strings.ReplaceAll(decl.sql, "{V}", v)
								runAliasInMetamorphic(t, e, ctxName, opts(si), si, decl.name, sql, name, pred)
							}
						}
					}
				}
			}
		}
	}
}

// TestTableRef_KeywordPins pins N1's storage-integrity twin, N2's direct
// keyword operand, and the parenthesised calls that still bind.
func TestTableRef_KeywordPins(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			// N1: the quoted one-part name `"db1.t"` is the table db1."db1.t",
			// phys."db1.db1.t" — never the Active db1.t, whose ordinary
			// physical table ClickHouse read before.
			tablerefCase{name: "keyword with named like the SI twin", si: si,
				sql:      `WITH NOW AS "db1.t" SELECT a FROM db1.o WHERE a IN "db1.t"`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH NOW AS "db1.t" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.db1.t"`,
				wantAcc:  []string{"db1.o", ".db1.t"}},
			tablerefCase{name: "keyword with nested", si: si,
				sql:      `WITH (current_date) AS "other.secret" SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE in(a, "other.secret"))`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH (current_date) AS "other.secret" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE in(a, phys."db1.other.secret"))`,
				wantAcc:  []string{"db1.o", "db1.p", ".other.secret"}},
			// N2: a bare keyword operand is an unqualified table; ClickHouse
			// reads phys.NOW for `a IN NOW` under both analyzers.
			tablerefCase{name: "direct keyword operand", si: si,
				sql:      "SELECT a FROM db1.o WHERE a IN NOW",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.NOW"`,
				wantAcc:  []string{".NOW", "db1.o"}},
			tablerefCase{name: "direct keyword operand parenthesised", si: si,
				sql:      "SELECT a FROM db1.o WHERE a IN (current_user)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (phys."db1.current_user")`,
				wantAcc:  []string{".current_user", "db1.o"}},
			tablerefCase{name: "direct keyword operand callable nested", si: si,
				sql:      "SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE globalIn(a, CURRENT_DATE))",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE globalIn(a, phys."db1.CURRENT_DATE"))`,
				wantAcc:  []string{".CURRENT_DATE", "db1.o", "db1.p"}},
		)
		// N4: `EXISTS` / `INTERVAL` are Polyglot identifier nodes in operand
		// position; ClickHouse 26.2 reads phys.EXISTS / phys.INTERVAL (any
		// case) under both analyzers. They are unqualified tables like `a IN b`.
		cases = append(cases,
			tablerefCase{name: "direct EXISTS operand", si: si,
				sql:      "SELECT a FROM db1.o WHERE a IN EXISTS",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.EXISTS"`,
				wantAcc:  []string{".EXISTS", "db1.o"}},
			tablerefCase{name: "direct interval operand parenthesised", si: si,
				sql:      "SELECT a FROM db1.o WHERE a NOT IN (interval)",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT a FROM phys."db1.o" "db1.o" WHERE a NOT IN (phys."db1.interval")`,
				wantAcc:  []string{"db1.o", ".interval"}},
			tablerefCase{name: "direct INTERVAL operand callable nested", si: si,
				sql:      "INSERT INTO db1.q SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE globalIn(a, INTERVAL))",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `INSERT INTO phys."db1.q" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE globalIn(a, phys."db1.INTERVAL"))`},
			tablerefCase{name: "direct EXISTS operand in DELETE is refused like a IN b", si: si,
				sql:      "DELETE FROM db1.o WHERE a IN EXISTS",
				wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg:  "statement is not supported",
				wantSQL:  "DELETE FROM db1.o WHERE a IN EXISTS"},
		)
		// N6: a compound over a bare keyword is a name reference too
		// (containsNameReference's no-parens branch): ClickHouse errors on
		// these, and the operand is rewritten rather than trusted.
		cases = append(cases,
			tablerefCase{name: "negated keyword with value", si: si,
				sql:      "WITH -NOW AS z SELECT a FROM db1.o WHERE a IN z",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH -NOW AS z SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.z"`,
				wantAcc:  []string{"db1.o", ".z"}},
			tablerefCase{name: "cast over keyword with value", si: si,
				sql:      "WITH CAST(NOW AS String) AS z SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE in(a, z))",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `WITH CAST(NOW AS String) AS z SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE in(a, phys."db1.z"))`,
				wantAcc:  []string{"db1.o", "db1.p", ".z"}},
		)
		// A parenthesised call binds as an expression under both analyzers
		// (NOW(), PI(), CURRENT_DATE(), current_user() measured): the operand
		// stays untouched and unreported, and a call operand is a value.
		for _, c := range []struct{ sql, want string }{
			{"WITH NOW() AS s SELECT a FROM db1.o WHERE a IN s",
				`WITH NOW() AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"WITH now() AS s SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN s)",
				`WITH now() AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN s)`},
			{"WITH PI() AS s SELECT a FROM db1.o WHERE in(a, s)",
				`WITH PI() AS s SELECT a FROM phys."db1.o" "db1.o" WHERE in(a, s)`},
			{"WITH (current_user()) AS s SELECT a FROM db1.o AS s WHERE a IN s",
				`WITH (current_user()) AS s SELECT a FROM phys."db1.o" AS s WHERE a IN s`},
			{"SELECT a FROM db1.o WHERE a IN NOW()",
				`SELECT a FROM phys."db1.o" "db1.o" WHERE a IN NOW()`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}

// Stored view bodies (review round 4). ClickHouse 26.2 and 25.8, under both
// analyzers, bind an unqualified IN operand in a CREATE VIEW / MATERIALIZED
// VIEW (TO, ENGINE, POPULATE, REFRESH) body differently from a plain SELECT:
// only a read-query CTE name and a WITH alias whose value is a single folded
// literal (a number, a string, NULL, a boolean, a negated number, a
// parenthesised one of those, or a flat tuple / bracket array of them) bind
// it. A WITH alias whose value is a function call, a cast, an operator, a
// scalar subquery, `tuple(…)` / `array(…)`, a nested tuple or a parenthesised
// tuple, and every projection alias (whatever its value), leave the operand a
// table: ClickHouse reads the table of that name, at the body's own level and
// in nested subqueries and set operations. A top-level `view()`, INSERT … SELECT and CTAS
// bodies follow the plain-SELECT rule.

var storedViewNonBindingValues = []string{
	"toUInt64(1)", "(toUInt64(1))", "(SELECT 1)", "1 + 1", "CAST(1 AS UInt64)",
	"((1, 2))", "[(1, 2)]", "tuple(1)",
}

var storedViewDecls = []struct{ name, sql string }{
	{"view same scope", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o WHERE {P}"},
	{"view nested", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"view with on the subquery", "CREATE OR REPLACE VIEW db1.v AS SELECT a FROM db1.o WHERE a IN (WITH {V} AS {N} SELECT a FROM db1.p WHERE {P})"},
	{"view over view()", "CREATE VIEW db1.v AS SELECT a FROM view(WITH {V} AS {N} SELECT a FROM db1.o WHERE {P})"},
	{"mv to", "CREATE MATERIALIZED VIEW db1.mv TO db1.q AS WITH {V} AS {N} SELECT a FROM db1.o WHERE {P}"},
	{"mv engine", "CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory AS WITH {V} AS {N} SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE {P})"},
	{"view projection alias", "CREATE VIEW db1.v AS SELECT a, {V} AS {N} FROM db1.o WHERE {P}"},
	// Set-operation bodies (review round 5, N7): Polyglot attaches the
	// leading WITH to the set node; the operand sits in either branch, and
	// the set node may be parenthesised or nested under another one.
	{"view union all left", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o WHERE {P} UNION ALL SELECT a FROM db1.o WHERE 0"},
	{"view union distinct right", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o WHERE 0 UNION DISTINCT SELECT a FROM db1.o WHERE {P}"},
	{"view intersect", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o WHERE {P} INTERSECT SELECT a FROM db1.o"},
	{"view except right", "CREATE VIEW db1.v AS WITH {V} AS {N} SELECT a FROM db1.o EXCEPT SELECT a FROM db1.o WHERE {P}"},
	{"view parenthesised union", "CREATE VIEW db1.v AS (WITH {V} AS {N} SELECT a FROM db1.o WHERE {P} UNION ALL SELECT a FROM db1.o WHERE 0)"},
	{"view nested set node", "CREATE VIEW db1.v AS SELECT a FROM db1.o WHERE 0 UNION ALL (WITH {V} AS {N} SELECT a FROM db1.o WHERE {P} UNION ALL SELECT a FROM db1.o WHERE 0)"},
	{"view union in nested subquery", "CREATE VIEW db1.v AS SELECT a FROM db1.o WHERE a IN (WITH {V} AS {N} SELECT a FROM db1.p WHERE {P} UNION ALL SELECT 1)"},
	{"mv populate union", "CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory POPULATE AS WITH {V} AS {N} SELECT a FROM db1.o WHERE {P} UNION ALL SELECT a FROM db1.o WHERE 0"},
	{"mv to union right", "CREATE MATERIALIZED VIEW db1.mv TO db1.q AS WITH {V} AS {N} SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM db1.o WHERE {P}"},
}

// TestTableRef_StoredViewAliasIsNotAnInBinding checks that, in a stored view
// body, a WITH alias with a non-literal value and a projection alias do not
// bind an IN operand: the answer equals the renamed-alias control.
func TestTableRef_StoredViewAliasIsNotAnInBinding(t *testing.T) {
	e := newEngine(t)
	contexts := aliasInContexts()
	preds := []string{"a IN {R}", "a NOT IN ({R})", "in(a, {R})"}
	for _, ctxName := range []string{"mapped", "empty"} {
		opts := contexts[ctxName]
		for _, si := range []bool{false, true} {
			for _, decl := range storedViewDecls {
				values := storedViewNonBindingValues
				if decl.name == "view projection alias" {
					// No bare tuple here: Polyglot drops the quotes of an alias on
					// a parenthesised tuple (review N5, handled by the mid-drop gate).
					values = []string{"1", "'x'", "toUInt64(1)", "(SELECT 1)"}
				}
				for _, v := range values {
					for _, name := range aliasInNames {
						for _, pred := range preds {
							sql := strings.ReplaceAll(decl.sql, "{V}", v)
							runAliasInMetamorphic(t, e, ctxName, opts(si), si, decl.name, sql, name, pred)
						}
					}
				}
			}
		}
	}
}

// TestTableRef_StoredViewAliasPins pins the finding's example and the names
// that still bind in a stored view body, plus the plain-SELECT-rule positions
// (view(), INSERT … SELECT, CTAS) where a function-valued alias still binds.
func TestTableRef_StoredViewAliasPins(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "finding example", si: si,
				sql:      "CREATE VIEW db1.vv AS WITH (SELECT 1) AS `db2.my-t` SELECT a FROM db1.o WHERE a IN `db2.my-t`",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE VIEW phys."db1.vv" AS WITH (SELECT 1) AS "db2.my-t" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.db2.my-t"`,
				wantAcc:  []string{"db1.vv", "db1.o", ".db2.my-t"}},
			tablerefCase{name: "mv to function alias", si: si,
				sql:      "CREATE MATERIALIZED VIEW db1.mv TO db1.q AS WITH toUInt64(1) AS x SELECT a FROM db1.o WHERE a IN x",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.q" AS WITH toUInt64(1) AS x SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.x"`,
				wantAcc:  []string{"db1.mv", "db1.q", "db1.o", ".x"}},
			tablerefCase{name: "view projection literal alias", si: si,
				sql:      "CREATE VIEW db1.v AS SELECT a, 1 AS c FROM db1.o WHERE a IN c",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE VIEW phys."db1.v" AS SELECT a, 1 AS c FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.c"`,
				wantAcc:  []string{"db1.v", ".c", "db1.o"}},
		)
		// N7: the finding's example with a set-operation body, and its SI twin.
		cases = append(cases,
			tablerefCase{name: "set-operation body cross-tenant", si: si,
				sql:      "CREATE VIEW db1.vv AS WITH (SELECT 1) AS `db2.my-t` SELECT a FROM db1.o WHERE a IN `db2.my-t` UNION ALL SELECT a FROM db1.o WHERE 0",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE VIEW phys."db1.vv" AS WITH (SELECT 1) AS "db2.my-t" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.db2.my-t" UNION ALL SELECT a FROM phys."db1.o" "db1.o" WHERE 0`,
				wantAcc:  []string{"db1.vv", "db1.o", ".db2.my-t"}},
			tablerefCase{name: "set-operation body SI twin", si: si,
				sql:      `CREATE VIEW db1.vv AS WITH toUInt64(1) AS "db1.t" SELECT a FROM db1.o WHERE a IN "db1.t" UNION ALL SELECT a FROM db1.o WHERE 0`,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE VIEW phys."db1.vv" AS WITH toUInt64(1) AS "db1.t" SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.db1.t" UNION ALL SELECT a FROM phys."db1.o" "db1.o" WHERE 0`,
				wantAcc:  []string{"db1.vv", "db1.o", ".db1.t"}},
		)
		// N8: literals ClickHouse folds that Polyglot types differently —
		// hex (`hex_number`), binary and inf / nan (a bare `column` to
		// Polyglot), hex strings and heredocs — bind in a stored view body.
		for _, v := range []string{"0x10", "-0x10", "0b101", "inf", "-INF", "NaN", "x'41'", "$$abc$$", "(0x10, inf)"} {
			sql := "CREATE VIEW db1.v AS WITH " + v + " AS s SELECT a FROM db1.o WHERE a IN s UNION ALL SELECT 1"
			cases = append(cases, tablerefCase{name: "stored literal " + v, si: si, sql: sql,
				wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.v", "db1.o"}})
		}
		// inf / nan / binary are literals in a plain SELECT too.
		for _, v := range []string{"inf", "-nan", "0b101"} {
			sql := "SELECT a FROM db1.o WHERE a IN (WITH " + v + " AS s SELECT a FROM db1.p WHERE a IN s)"
			cases = append(cases, tablerefCase{name: "plain literal " + v, si: si, sql: sql,
				wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o", "db1.p"}})
		}
		// Names that still bind in a stored view body: folded literals and a
		// read-query CTE.
		for _, c := range []struct{ sql, want string }{
			{"CREATE VIEW db1.v AS WITH 1 AS s SELECT a FROM db1.o WHERE a IN s",
				`CREATE VIEW phys."db1.v" AS WITH 1 AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"CREATE VIEW db1.v AS WITH (1, 2) AS s SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN s)",
				`CREATE VIEW phys."db1.v" AS WITH (1, 2) AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN s)`},
			{"CREATE VIEW db1.v AS WITH [-1, 2] AS s SELECT a FROM db1.o WHERE in(a, s)",
				`CREATE VIEW phys."db1.v" AS WITH [-1, 2] AS s SELECT a FROM phys."db1.o" "db1.o" WHERE in(a, s)`},
			{"CREATE VIEW db1.v AS WITH (-1) AS s SELECT a FROM db1.o WHERE a IN s",
				`CREATE VIEW phys."db1.v" AS WITH (-1) AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"CREATE MATERIALIZED VIEW db1.mv TO db1.q AS WITH 'x' AS s SELECT a FROM db1.o WHERE a IN s",
				`CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.q" AS WITH 'x' AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"CREATE VIEW db1.v AS WITH NULL AS s SELECT a FROM db1.o AS s WHERE a IN s",
				`CREATE VIEW phys."db1.v" AS WITH NULL AS s SELECT a FROM phys."db1.o" AS s WHERE a IN s`},
			{"CREATE VIEW db1.v AS WITH c AS (SELECT 1 AS a) SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN c)",
				`CREATE VIEW phys."db1.v" AS WITH c AS (SELECT 1 AS a) SELECT a FROM phys."db1.o" "db1.o" WHERE a IN (SELECT a FROM phys."db1.p" "db1.p" WHERE a IN c)`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
		// Plain-SELECT-rule positions: a function-valued WITH alias and an own
		// projection alias still bind (measured in view(), INSERT … SELECT and
		// CTAS bodies as in a plain SELECT).
		for _, c := range []struct{ sql, want string }{
			{"SELECT a FROM view(WITH toUInt64(1) AS s SELECT a FROM db1.o WHERE a IN s)",
				`SELECT a FROM view(WITH toUInt64(1) AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s)`},
			{"INSERT INTO db1.q WITH (SELECT 1) AS s SELECT a FROM db1.o WHERE a IN s",
				`INSERT INTO phys."db1.q" WITH (SELECT 1) AS s SELECT a FROM phys."db1.o" "db1.o" WHERE a IN s`},
			{"CREATE TABLE db1.n ENGINE = Memory AS SELECT a, 1 AS s FROM db1.o WHERE a IN s",
				`CREATE TABLE phys."db1.n" ENGINE=Memory AS (SELECT a, 1 AS s FROM phys."db1.o" "db1.o" WHERE a IN s)`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_NameResolutionSettingsAreRefused pins review round 6, N9.
// Measured on ClickHouse 26.2 and 25.8, both analyzers, by flipping every
// Bool setting (802 / 711) and eleven `compatibility` versions over 13
// binding shapes and 4 stored-view shapes: only enable_global_with_statement
// = 0, and a `compatibility` version (20.1, 20.8, 21.1) that restores it as
// the default, turn a name the binding rule trusts (a WITH alias or CTE name
// in a nested query or a later set arm) into a table read, in a query-level
// SETTINGS clause, a view body's SETTINGS and a session SET before CREATE
// VIEW. implicit_table_at_top_level names a table a FROM-less SELECT reads
// (`SELECT a SETTINGS implicit_table_at_top_level = 'z'` reads phys.z). Both
// are refused wherever dynamic mode governs settings, whatever the value.
func TestTableRef_NameResolutionSettingsAreRefused(t *testing.T) {
	msg := func(name string) string { return "table setting " + name + " is not accepted" }
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, name string }{
			// The reviewer's N9 reproducers.
			{`CREATE VIEW db1.v AS WITH 1 AS "other.secret" SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM db1.o WHERE a IN "other.secret" SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`CREATE VIEW db1.v AS WITH 'x' AS "db1.t" SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM db1.o WHERE a IN "db1.t" SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory POPULATE AS WITH 1 AS "other.secret" SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM db1.o WHERE a IN "other.secret" SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`CREATE VIEW db1.v AS WITH "other.secret" AS (SELECT toUInt64(1) AS a) SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM "other.secret" SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`WITH "other.secret" AS (SELECT toUInt64(1) AS a) SELECT a FROM db1.o WHERE a IN (SELECT a FROM "other.secret") SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`WITH 1 AS "other.secret" SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN "other.secret") SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`INSERT INTO db1.q WITH toUInt64(1) AS "other.secret" SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN "other.secret") SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			{`CREATE TABLE db1.n ENGINE = Memory AS WITH toUInt64(1) AS "other.secret" SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN "other.secret") SETTINGS enable_global_with_statement = 0`, "enable_global_with_statement"},
			// Every value and spelling, and the INSERT header list.
			{"SELECT a FROM db1.o SETTINGS enable_global_with_statement = 1", "enable_global_with_statement"},
			{"SELECT a FROM db1.o SETTINGS max_threads = 1, Enable_Global_With_Statement = false", "Enable_Global_With_Statement"},
			{"INSERT INTO db1.q SETTINGS enable_global_with_statement = 0 SELECT a FROM db1.o", "enable_global_with_statement"},
			// compatibility = '20.1' … '21.1' restores the old default
			// enable_global_with_statement = 0 (measured: the same reads).
			{`WITH 1 AS "other.secret" SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p WHERE a IN "other.secret") SETTINGS compatibility = '20.8'`, "compatibility"},
			{`CREATE VIEW db1.v AS WITH "other.secret" AS (SELECT toUInt64(1) AS a) SELECT a FROM db1.o WHERE 0 UNION ALL SELECT a FROM "other.secret" SETTINGS compatibility = '21.1'`, "compatibility"},
			{"SELECT a FROM db1.o SETTINGS compatibility = '25.1'", "compatibility"},
			// implicit_table_at_top_level names the table a FROM-less SELECT reads.
			{"SELECT a SETTINGS implicit_table_at_top_level = 'z'", "implicit_table_at_top_level"},
			{"SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.p) SETTINGS implicit_table_at_top_level = 'other.secret'", "implicit_table_at_top_level"},
			{"CREATE VIEW db1.v AS SELECT a SETTINGS implicit_table_at_top_level = 'z'", "implicit_table_at_top_level"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg(c.name), wantSQL: c.sql})
		}
	}
	// A session SET (the carve-out exists only while the SI surface is
	// inactive; with it active every SET is the SI catch-all).
	for _, c := range []struct{ sql, name string }{
		{"SET enable_global_with_statement = 0", "enable_global_with_statement"},
		{"SET max_threads = 1, enable_global_with_statement = 0", "enable_global_with_statement"},
		{"SET implicit_table_at_top_level = 'z'", "implicit_table_at_top_level"},
		{"SET compatibility = '20.1'", "compatibility"},
	} {
		cases = append(cases,
			tablerefCase{name: c.sql, sql: c.sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg(c.name), wantSQL: c.sql},
			tablerefCase{name: "si/" + c.sql, sql: c.sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
	}
	// Settings measured not to change binding stay accepted, including the
	// analyzer switch: every binding rule was measured under both analyzers.
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, want string }{
			{"SELECT a FROM db1.o SETTINGS enable_analyzer = 0",
				`SELECT a FROM phys."db1.o" "db1.o" SETTINGS enable_analyzer = 0`},
			{"SELECT a FROM db1.o SETTINGS allow_experimental_analyzer = 0, enable_scopes_for_with_statement = 0, prefer_column_name_to_alias = 1",
				`SELECT a FROM phys."db1.o" "db1.o" SETTINGS allow_experimental_analyzer = 0, enable_scopes_for_with_statement = 0, prefer_column_name_to_alias = 1`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}

// escapedRefusedSettingMsg is the refusal an escaped setting name gets once
// quoted names are decoded as ClickHouse decodes them (#54): a name that
// decodes to a refused setting is refused as that setting, before the
// verbatim-spelling rule; any other escaped name keeps fallback.
func escapedRefusedSettingMsg(sql, fallback string) string {
	decoded := strings.ReplaceAll(sql, `\N`, "")
	for _, n := range []string{"implicit_table_at_top_level", "enable_global_with_statement", "compatibility"} {
		if strings.Contains(decoded, n) {
			return "table setting " + n + " is not accepted"
		}
	}
	return fallback
}

// TestTableRef_EscapedSettingNamesAreRefused pins review round 7, N11.
// ClickHouse 26.2 / 25.8 decode `\N` in a quoted identifier to nothing, so
// `\Nenable_global_with_statement`, "\Ncompatibility" and
// `enable\N_global_with_statement` name the refused setting. The paths that
// forward setting text verbatim (a session SET, the opaque INSERT column-list
// header, a command's SETTINGS tail) therefore refuse any setting name whose
// spelling is not its plain text: one containing a backslash, or a quoted one
// with a doubled quote. Structured positions are fail-safe (Polyglot
// regenerates the backslash escaped and ClickHouse answers UNKNOWN_SETTING).
func TestTableRef_EscapedSettingNamesAreRefused(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	// Session SET (the carve-out exists only while the SI surface is
	// inactive; with it active every SET is the SI catch-all).
	for _, sql := range []string{
		"SET `\\Nenable_global_with_statement` = 0",
		"SET max_threads = 1, \"\\Ncompatibility\" = '20.8'",
		"SET `enable\\N_global_with_statement` = 0, max_threads = 1",
		"SET `impl\\Nicit_table_at_top_level` = 'other.z';",
		"SET `\\Nmax_threads` = 1",
		"SET `max``threads` = 1",
		"SET \"max\"\"threads\" = 1",
	} {
		cases = append(cases,
			tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: escapedRefusedSettingMsg(sql, unsupported), wantSQL: sql},
			tablerefCase{name: "si/" + sql, sql: sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
	}
	for _, si := range []bool{false, true} {
		// The opaque INSERT column-list header and a command's SETTINGS tail.
		for _, sql := range []string{
			"INSERT INTO db1.q (a) SETTINGS `\\Nimplicit_table_at_top_level` = 'hg_unsafe.db1__t' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS \"\\Nimplicit_table_at_top_level\" = 'hg_unsafe.db1__t' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1, `impl\\Nicit_table_at_top_level` = 'phys.z' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS `\\Nenable_global_with_statement` = 0 SELECT a",
			"DESCRIBE TABLE db1.o SETTINGS `\\Nenable_global_with_statement` = 0",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: escapedRefusedSettingMsg(sql, unsupported), wantSQL: sql})
		}
		// Plain and quoted names without escapes behave as before.
		for _, c := range []struct{ sql, want string }{
			{"INSERT INTO db1.q (a) SETTINGS `max_threads` = 1 SELECT a", `INSERT INTO phys."db1.q" (a) SETTINGS ` + "`max_threads`" + ` = 1 SELECT a`},
			{"INSERT INTO db1.q (a) SETTINGS \"max_threads\" = 1, max_block_size = 10 SELECT a", `INSERT INTO phys."db1.q" (a) SETTINGS "max_threads" = 1, max_block_size = 10 SELECT a`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
		// N12: the promql table names, defence in depth.
		for _, n := range []string{"promql_table", "promql_database"} {
			sql := "SELECT a FROM db1.o SETTINGS " + n + " = 'z'"
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting " + n + " is not accepted", wantSQL: sql})
		}
	}
	for _, sql := range []string{"SET max_threads = 1", "SET `max_threads` = 1", "SET \"max_threads\" = 1, max_block_size = 10"} {
		cases = append(cases, tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_Success, wantSQL: sql})
	}
	for _, n := range []string{"promql_table", "promql_database"} {
		sql := "SET " + n + " = 'z'"
		cases = append(cases, tablerefCase{name: sql, sql: sql,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting " + n + " is not accepted", wantSQL: sql})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_UnparseableSettingsClausesFailClosed pins review round 8, N14.
// ClickHouse accepts a compound (dotted) custom setting name (`SQL_a.b`,
// `SQL_a`.b, "SQL_a"."b"). The opaque-text scanner used to skip a whole
// SETTINGS clause whose first assignment was not `name =`, so an escaped
// refused name after it reached ClickHouse (INSERT … (a) SETTINGS SQL_a.b = 1,
// `\Nimplicit_table_at_top_level` = 'hg_unsafe.db1__t' SELECT a copied an
// hg_unsafe row). Every SETTINGS clause (and SET list) a verbatim path cannot
// fully parse into verbatim-safe (name, value) pairs is now refused; a dotted
// setting name is not verbatim-safe (N15, a deliberate over-refusal).
func TestTableRef_UnparseableSettingsClausesFailClosed(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			// The reviewer's reproducers.
			"INSERT INTO db1.q (a) SETTINGS SQL_a.b = 1, `\\Nimplicit_table_at_top_level` = 'hg_unsafe.db1__t' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS `SQL_a`.b = 1, `\\Nimplicit_table_at_top_level` = 'hg_unsafe.db1__t' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS \"SQL_a\".\"b\" = 1, \"\\Nimplicit_table_at_top_level\" = 'hg_unsafe.db1__t' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS SQL_a.b = 1, `impl\\Nicit_table_at_top_level` = 'other.z' SELECT a",
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1 SELECT a SETTINGS SQL_a.b = 1, `\\Nimplicit_table_at_top_level` = 'hg_unsafe.db1__t'",
			"DESCRIBE TABLE db1.o SETTINGS SQL_a.b = 1, `\\Nenable_global_with_statement` = 0",
			"INSERT INTO db1.q (a) SETTINGS SQL_a.b = 1, max_threads = [1] SELECT a",
			// A compound name alone, and later in the list.
			"INSERT INTO db1.q (a) SETTINGS SQL_a.b = 1 SELECT a",
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1, SQL_a . b = 1 SELECT a",
			"DESCRIBE TABLE db1.o SETTINGS SQL_a.b.c = 1",
			// A SETTINGS keyword that does not start a parseable list.
			"DESCRIBE TABLE db1.o SETTINGS",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: escapedRefusedSettingMsg(sql, unsupported), wantSQL: sql})
		}
		// Escape-free controls stay Success, including HouseGate's own keys.
		for _, c := range []struct{ sql, want string }{
			{"INSERT INTO db1.q (a) SETTINGS max_threads = 1, SQL_x_payer = 'p' SELECT a",
				`INSERT INTO phys."db1.q" (a) SETTINGS max_threads = 1, SQL_x_payer = 'p' SELECT a`},
			{"INSERT INTO db1.q (a) SETTINGS SQL_sentio_driver = 1 SELECT a",
				`INSERT INTO phys."db1.q" (a) SETTINGS SQL_sentio_driver = 1 SELECT a`},
			// A column named settings is not a SETTINGS clause.
			{"ALTER TABLE db1.o UPDATE settings = 1 WHERE 1", "ALTER TABLE phys.`db1.o` UPDATE settings = 1 WHERE 1"},
			{"INSERT INTO db1.q (a) SETTINGS `max_threads` = 1 SELECT a SETTINGS max_block_size = 10",
				`INSERT INTO phys."db1.q" (a) SETTINGS ` + "`max_threads`" + ` = 1 SELECT a SETTINGS max_block_size = 10`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	// ALTER … UPDATE tail (SI off; with the SI surface active the mutation
	// probe refuses a SETTINGS tail first, with the same message).
	// With the SI surface inactive the decoded name (#54) is refused as the
	// refused setting it names.
	for _, si := range []bool{false, true} {
		sql := "ALTER TABLE db1.o UPDATE a = 1 WHERE 1 SETTINGS SQL_a.b = 1, `\\Nenable_global_with_statement` = 0"
		msg := unsupported
		if !si {
			msg = escapedRefusedSettingMsg(sql, unsupported)
		}
		cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: sql})
	}
	// SET with a compound name is not a modelled SET (N15, fail closed); the
	// session keys HouseGate uses have no dot and stay Success.
	cases = append(cases,
		tablerefCase{name: "SET SQL_a.b = 1", sql: "SET SQL_a.b = 1", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: "SET SQL_a.b = 1"},
		tablerefCase{name: "SET SQL_x_read_mode", sql: "SET SQL_x_read_mode = 'safe', SQL_sentio_maintenance = 1", wantCode: pb.RewriteCode_Success,
			wantSQL: "SET SQL_x_read_mode = 'safe', SQL_sentio_maintenance = 1"},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_ShowSettingsSkipAndTableSettingNames pins review round 9.
//
// N16: only a SHOW [CHANGED] SETTINGS statement is exempt from the verbatim
// settings scan, not a column or alias named show in front of a real SETTINGS
// clause (`… WHERE show SETTINGS max_threads = [1]` used to skip the clause).
//
// N17: ClickHouse decodes an escaped name in ALTER … MODIFY / RESET SETTING
// (`\Nstorage_policy` applies storage_policy); the forwarded raw action is
// refused unless every setting name is verbatim-safe and simple.
func TestTableRef_ShowSettingsSkipAndTableSettingNames(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1 SELECT 1 AS show SETTINGS max_threads = (1)",
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1 SELECT show SETTINGS max_threads = [1]",
			"INSERT INTO db1.q (a) SETTINGS max_threads = 1 SELECT a AS show SETTINGS SQL_a.b = 1",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: escapedRefusedSettingMsg(sql, unsupported), wantSQL: sql})
		}
		// N17: escaped, doubled-quote and compound table-setting names.
		for _, sql := range []string{
			"ALTER TABLE db1.o MODIFY SETTING `\\Nstorage_policy` = 'default'",
			"ALTER TABLE db1.o MODIFY SETTING \"\\Ndisk\" = 'd'",
			"ALTER TABLE db1.o MODIFY SETTING max_parts_in_total = 100, `stor\\Nage_policy` = 'default'",
			"ALTER TABLE db1.o MODIFY SETTING `storage``policy` = 'default'",
			"ALTER TABLE db1.o MODIFY SETTING a.storage_policy = 'default'",
			"ALTER TABLE db1.o RESET SETTING `\\Nstorage_policy`",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: sql})
		}
		cases = append(cases, tablerefCase{name: "reset storage_policy", sql: "ALTER TABLE db1.o RESET SETTING storage_policy", si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting storage_policy is not accepted", wantSQL: "ALTER TABLE db1.o RESET SETTING storage_policy"})
		// Controls: plain and quoted names without escapes are unchanged.
		for _, c := range []struct{ sql, want string }{
			{"ALTER TABLE db1.o MODIFY SETTING max_parts_in_total = 100", `ALTER TABLE phys."db1.o" MODIFY SETTING max_parts_in_total=100`},
			{"ALTER TABLE db1.o MODIFY SETTING `max_parts_in_total` = 100, parts_to_throw_insert = 3000", `ALTER TABLE phys."db1.o" MODIFY SETTING "max_parts_in_total" = 100, parts_to_throw_insert=3000`},
			{"ALTER TABLE db1.o RESET SETTING max_parts_in_total", `ALTER TABLE phys."db1.o" RESET SETTING max_parts_in_total`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	// N16 on the ALTER … UPDATE tail (with the SI surface active the mutation
	// probe refuses a SETTINGS tail first, with the same message).
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"ALTER TABLE db1.o UPDATE a = 1 WHERE show SETTINGS max_threads = [1]",
			"ALTER TABLE db1.o UPDATE a = 1 WHERE show changed SETTINGS max_threads = [1]",
			"ALTER TABLE db1.o UPDATE a = 1 WHERE show SETTINGS max_threads = 1, implicit_table_at_top_level",
			"ALTER TABLE db1.o UPDATE a = 1 WHERE show SETTINGS SQL_a.b = 1",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: sql})
		}
	}
	// SHOW SETTINGS followed by an assignment list is scanned like a clause
	// (main refused these; they stay refused).
	for _, sql := range []string{"SHOW SETTINGS max_threads = [1]", "SHOW SETTINGS max_threads = 1, implicit_table_at_top_level", "SHOW CHANGED SETTINGS SQL_a.b = 1"} {
		cases = append(cases, tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: sql})
	}
	// A real SHOW [CHANGED] SETTINGS statement is still not a SETTINGS clause.
	for _, sql := range []string{"SHOW SETTINGS LIKE 'max%'", "SHOW CHANGED SETTINGS", "/* c */ SHOW CHANGED SETTINGS ILIKE 'max%'"} {
		cases = append(cases, tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_Success})
	}
	runTablerefCases(t, cases)
}
