package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// Table-engine arguments are a governed expression position (spec 2026-09-26
// §5, amendment 2026-10-01). ClickHouse evaluates them: on 25.8 with
// allow_deprecated_syntax_for_merge_tree = 1 (a session SET, or the CREATE
// statement's own SETTINGS clause) the deprecated positional *MergeTree
// signature runs `MergeTree(d, (SELECT throwIf(n = 42, 'LEAK') FROM
// phys.`db2.x`), 8192)` at CREATE time and raises the other tenant's row;
// 26.2 rejects a subquery there but still accepts `n IN phys.`db2.x`` as a
// key. v0.15.0 answered Success and forwarded every such argument unrewritten
// and unreported, because only the T5 allowlist looked at an ENGINE clause.
// An engine argument is now walked exactly like a column DEFAULT: every
// collector (T2, T3, the system-table check, T5 table functions, T6, the SI
// handlers) sees it, and R2 refuses any read left over.

// engineArgReads are read-bearing expressions; each must be refused in an
// engine argument with the same code and message as in a column DEFAULT.
var engineArgReads = []string{
	"(SELECT throwIf(n = 42, 'LEAK') FROM phys.`db2.x`)",
	"(SELECT max(n) FROM db1.o)",
	"(SELECT 1 FROM `db2.x`)",
	"(SELECT 1 FROM db2.x)",
	"(SELECT 1 FROM o)",
	"(SELECT 1 FROM db1.t)",
	"(SELECT 1 FROM hg_safe.db1__t)",
	"(SELECT 1 FROM hg_promote.x)",
	"(SELECT 1 FROM system.processes)",
	"(SELECT 1 FROM {p:Identifier})",
	"(SELECT 1 FROM merge('db1', 'o'))",
	"(SELECT 1 FROM remote('h', 'db1', 'o'))",
	"(SELECT 1 FROM numbers(1))",
	"(WITH x AS (SELECT 1) SELECT * FROM x)",
	"n IN db1.o",
	"n IN (db1.o)",
	"n IN `db2.x`",
	"n IN phys.`db2.x`",
	"n GLOBAL NOT IN db1.o",
	"n IN (SELECT n FROM db1.o)",
	"in(n, db1.o)",
	"EXISTS (SELECT 1 FROM db1.o)",
	"arrayMap(x -> (SELECT 1 FROM db1.o), [1])",
	"joinGet('db1.j', 'v', n)",
	"dictGet('db1.dd', 'v', n)",
	"hasColumnInTable('db1', 'o', 'n')",
}

// engineArgPlacements put one expression in an engine argument (left) and in
// the column-expression position it must behave like (right).
var engineArgPlacements = []struct {
	name        string
	engine, ref func(expr string) string
}{
	{"deprecated MergeTree key",
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, " + r + ", 8192)"
		},
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8, z UInt8 DEFAULT " + r + ") ENGINE = MergeTree(d, n, 8192)"
		}},
	{"deprecated SummingMergeTree sampling slot",
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = SummingMergeTree(d, " + r + ", (d, n), 8192)"
		},
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8, z UInt8 DEFAULT " + r + ") ENGINE = SummingMergeTree(d, n, (d, n), 8192)"
		}},
	{"ReplacingMergeTree version argument",
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = ReplacingMergeTree(" + r + ") ORDER BY n"
		},
		func(r string) string {
			return "CREATE TABLE db1.n (d Date, n UInt8, z UInt8 DEFAULT " + r + ") ENGINE = ReplacingMergeTree ORDER BY n"
		}},
	{"Join key argument",
		func(r string) string {
			return "CREATE TABLE db1.n (k UInt8, n UInt8) ENGINE = Join(ANY, LEFT, " + r + ")"
		},
		func(r string) string {
			return "CREATE TABLE db1.n (k UInt8, n UInt8 DEFAULT " + r + ") ENGINE = Join(ANY, LEFT, k)"
		}},
	{"materialized view inner engine",
		func(r string) string {
			return "CREATE MATERIALIZED VIEW db1.mv ENGINE = SummingMergeTree(" + r + ") ORDER BY n AS SELECT a AS n FROM db1.o"
		},
		func(r string) string {
			return "CREATE MATERIALIZED VIEW db1.mv ENGINE = SummingMergeTree PARTITION BY " + r + " ORDER BY n AS SELECT a AS n FROM db1.o"
		}},
}

func TestEngineArgs_ReadsAreRefusedLikeAColumnExpression(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, p := range engineArgPlacements {
			for _, r := range engineArgReads {
				engineSQL, refSQL := p.engine(r), p.ref(r)
				t.Run(p.name+"/"+r, func(t *testing.T) {
					got, err := doRewrite(e, engineSQL, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(engine): %v", err)
					}
					want, err := doRewrite(e, refSQL, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(ref): %v", err)
					}
					if want.GetCode() == pb.RewriteCode_Success {
						t.Fatalf("si=%v: reference %q answers Success; the read list must hold reads only", si, refSQL)
					}
					if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
						t.Fatalf("si=%v: %q = %s %q, want %s %q (as %q)", si, engineSQL,
							got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage(), refSQL)
					}
					if got.GetSqlAfterRewrite() != engineSQL {
						t.Fatalf("si=%v: a refusal must echo the caller's SQL, got %q", si, got.GetSqlAfterRewrite())
					}
				})
			}
		}
	}
}

func TestEngineArgs_ReproducerAndExactPins(t *testing.T) {
	const (
		unsupported = "statement is not supported"
		paramMsg    = "query parameters are not supported in a database or table position"
	)
	protected := func(db string) string { return "protected database " + db + " is not addressable" }
	type row struct {
		sql     string
		code    pb.RewriteCode
		msgOff  string
		codeOn  pb.RewriteCode // zero value = same as code
		msgOn   string         // "" = same as msgOff
		setCode bool
	}
	rows := []row{
		// The reproducer (task-I3 ledger line 13), measured on 25.8.
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT throwIf(n=42,'LEAK') FROM phys.`db2.x`), 8192)",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT throwIf(n=42,'LEAK') FROM phys.`db2.x`), 8192) SETTINGS allow_deprecated_syntax_for_merge_tree = 1",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT throwIf(n=42,'LEAK') FROM `db2.x`), 8192)",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT max(n) FROM db1.o), 8192)",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, n IN db1.o, 8192)",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported,
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity logical database db1 is not directly addressable through IN table target"},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, throwIf(n IN phys.`db2.x`, 'LEAK'), 8192)",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1 FROM system.processes), 8192)",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: "system table system.processes is not accessible"},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1 FROM {p:Identifier}), 8192)",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: paramMsg},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1 FROM hg_safe.db1__t), 8192)",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_safe"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1 FROM merge('db1', 'o')), 8192)",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: "table function merge is not accepted",
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity logical database db1 is not directly addressable through merge table function"},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, joinGet('db1.j', 'v', n), 8192)",
			code: pb.RewriteCode_InvalidRewriteRequest, msgOff: `joinGet target "db1.j" does not resolve through the caller's databases`},
		// Every allowed engine: an argument is governed whatever the engine
		// does with it (most reject arguments at CREATE time; the policy does
		// not depend on that).
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = Memory((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = Set((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = Null((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = Log((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = TinyLog((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = StripeLog((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = View((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = MaterializedView((SELECT 1 FROM db1.o))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree((SELECT 1 FROM db1.o)) ORDER BY n", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = AggregatingMergeTree((SELECT 1 FROM db1.o)) ORDER BY n", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = CollapsingMergeTree((SELECT 1 FROM db1.o)) ORDER BY n", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = VersionedCollapsingMergeTree(n, (SELECT 1 FROM db1.o)) ORDER BY n", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (n UInt8) ENGINE = GraphiteMergeTree((SELECT 'x' FROM db1.o)) ORDER BY n", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		// Every ENGINE clause and every CREATE form that carries one.
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE MergeTree(d, (SELECT 1 FROM db1.o), 8192)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n AS db1.o ENGINE = MergeTree(a, (SELECT 1 FROM db1.o), 8192)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1 FROM db1.o), 8192) AS SELECT today() AS d, 1 AS n",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (d Date, n UInt8) ENGINE = Memory ENGINE = MergeTree(d, (SELECT 1 FROM db1.o), 8192)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree(d, (SELECT 1 FROM db1.o), 8192) AS SELECT today() AS d, a AS n FROM db1.o",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE MATERIALIZED VIEW db1.mv ENGINE = SummingMergeTree(n IN `db2.x`) ORDER BY n AS SELECT a AS n FROM db1.o",
			code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, r := range rows {
			code, msg := r.code, r.msgOff
			if si && r.setCode {
				code, msg = r.codeOn, r.msgOn
			}
			cases = append(cases, tablerefCase{name: r.sql, sql: r.sql, si: si, wantCode: code, wantMsg: msg, wantSQL: r.sql})
		}
	}
	runTablerefCases(t, cases)
}

// Refused engines stay refused whatever their arguments hold; walking the
// arguments may only change which refusal names the statement.
func TestEngineArgs_RefusedEnginesStayRefused(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, name := range []string{
			"Buffer", "Distributed", "Merge", "URL", "File", "GenerateRandom", "KeeperMap", "TimeSeries",
			"Dictionary", "S3", "MySQL", "PostgreSQL", "EmbeddedRocksDB", "Executable", "ReplicatedMergeTree",
			"CoalescingMergeTree", "Kafka", "SQLite", "Redis", "MongoDB",
		} {
			for _, arg := range []string{"(SELECT 1 FROM db1.o)", "(SELECT 1 FROM phys.`db2.x`)", "n IN db1.o", "1"} {
				sql := "CREATE TABLE db1.n (n UInt8) ENGINE = " + name + "(" + arg + ")"
				resp, err := doRewrite(e, sql, tablerefOpts(si))
				if err != nil {
					t.Fatalf("doRewrite(%q): %v", sql, err)
				}
				if resp.GetCode() == pb.RewriteCode_Success {
					t.Errorf("si=%v: %q answered Success", si, sql)
				}
			}
		}
	}
}

// Read-free engine arguments keep working, unchanged.
func TestEngineArgs_ReadFreeArgumentsKeepWorking(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, want string }{
			{"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, n, 8192)",
				`CREATE TABLE phys."db1.n" (d DATE, n UInt8) ENGINE=MergeTree(d, n, 8192)`},
			{"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (n, d), 8192)",
				`CREATE TABLE phys."db1.n" (d DATE, n UInt8) ENGINE=MergeTree(d, (n, d), 8192)`},
			{"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, intHash32(n), (d, intHash32(n)), 8192)",
				`CREATE TABLE phys."db1.n" (d DATE, n UInt8) ENGINE=MergeTree(d, intHash32(n), (d, intHash32(n)), 8192)`},
			{"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, n IN (1, 2), 8192)",
				`CREATE TABLE phys."db1.n" (d DATE, n UInt8) ENGINE=MergeTree(d, n IN (1, 2), 8192)`},
			{"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (SELECT 1), 8192)",
				`CREATE TABLE phys."db1.n" (d DATE, n UInt8) ENGINE=MergeTree(d, (SELECT 1), 8192)`},
			{"CREATE TABLE db1.n (k UInt8, v String) ENGINE = Join(ANY, LEFT, k)",
				`CREATE TABLE phys."db1.n" (k UInt8, v String) ENGINE=Join(ANY, LEFT, k)`},
			{"CREATE TABLE db1.n (n UInt8, v UInt8) ENGINE = ReplacingMergeTree(v) ORDER BY n",
				`CREATE TABLE phys."db1.n" (n UInt8, v UInt8) ENGINE=ReplacingMergeTree(v) ORDER BY n`},
			{"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree() ORDER BY n",
				`CREATE TABLE phys."db1.n" (n UInt8) ENGINE=MergeTree() ORDER BY n`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success,
				wantSQL: c.want, wantAcc: []string{"db1.n"}})
		}
	}
	runTablerefCases(t, cases)
}

// ALTER … MODIFY REFRESH … DEPENDS ON names tables ClickHouse resolves against
// the view's own database (the physical one) and never checks for existence
// (measured on 25.8 and 26.2: DEPENDS ON `db2.rmv`, nosuch.zz and
// system.processes are all accepted and stored verbatim), so a forwarded
// name couples the view to another tenant's refreshable view. The rewriter
// cannot rewrite the opaque action text, so the clause is refused, like a
// MOVE PARTITION … TO TABLE target; a parameter, protected or system name
// keeps its own earlier message. CREATE … REFRESH is already refused.
func TestDependsOn_TargetsAreRefused(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON db2.x",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv`",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON db1.rmv",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON rmv",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON db1.o",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON db1.t",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON db2.rmv, db1.o",
			"ALTER TABLE db1.mv MODIFY REFRESH AFTER 1 HOUR depends on `db2.rmv`",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS/**/ON `db2.rmv`",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR RANDOMIZE FOR 10 MINUTE DEPENDS ON `db2.rmv`",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv` SETTINGS refresh_retries = 1",
			"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv`, MODIFY COMMENT 'c'",
			"ALTER TABLE db1.mv MODIFY COMMENT 'c', MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv`",
			"ALTER TABLE db1.mv ON CLUSTER c MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv`",
			"ALTER TABLE db1.mv UPDATE a = 1 WHERE 1, MODIFY REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv`",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// Earlier refusals keep their messages.
		cases = append(cases,
			tablerefCase{name: "system", sql: "ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON system.processes", si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "system table system.processes is not accessible"},
			tablerefCase{name: "protected", sql: "ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON phys.x", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "parameter", sql: "ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR DEPENDS ON {p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "query parameters are not supported in a database or table position"},
		)
		// CREATE … REFRESH (with or without DEPENDS ON) stays refused.
		for _, sql := range []string{
			"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv` ENGINE = Memory AS SELECT 1 AS a",
			"CREATE MATERIALIZED VIEW db1.mv REFRESH AFTER 1 HOUR DEPENDS ON `db2.rmv` APPEND TO db1.o AS SELECT 1 AS a",
			"CREATE OR REPLACE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR DEPENDS ON db2.x ENGINE = Memory AS SELECT 1 AS a",
			"CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory REFRESH EVERY 1 HOUR DEPENDS ON `db2.rmv` AS SELECT 1 AS a",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// A REFRESH without DEPENDS ON names no table and keeps working.
		for _, c := range []struct{ sql, want string }{
			{"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR", `ALTER TABLE phys."db1.mv" MODIFY REFRESH EVERY 1 HOUR`},
			{"ALTER TABLE db1.mv MODIFY REFRESH EVERY 1 HOUR APPEND", `ALTER TABLE phys."db1.mv" MODIFY REFRESH EVERY 1 HOUR APPEND`},
			{"ALTER TABLE db1.mv MODIFY REFRESH AFTER 1 HOUR SETTINGS refresh_retries = 1",
				`ALTER TABLE phys."db1.mv" MODIFY REFRESH AFTER 1 HOUR SETTINGS refresh_retries=1`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}
