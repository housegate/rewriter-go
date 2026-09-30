package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// An EXPLAIN statement nested in another statement is refused wherever it
// appears (fix round 1, M1). Polyglot parses `(EXPLAIN …)` as a subquery
// wrapping an opaque command node, which the ordered walker cannot see into;
// ClickHouse 25.8 rewrites it to viewExplain(…) and starts executing it over
// the named table at CREATE time in an ENGINE argument, PARTITION BY or ORDER
// BY. The oracle here is absolute — every combination must be refused — so a
// blind spot shared by two positions cannot hide it the way the DEFAULT
// metamorphic reference did.

var explainKinds = []string{
	"EXPLAIN", "EXPLAIN AST", "EXPLAIN SYNTAX", "EXPLAIN PLAN", "EXPLAIN PIPELINE",
	"EXPLAIN ESTIMATE", "EXPLAIN QUERY TREE", "EXPLAIN PLAN actions = 1,", "explain ast",
}

var explainInner = []string{
	"SELECT 1 FROM phys.`db2.x`",
	"SELECT n FROM db1.o",
	"SELECT 1",
}

// explainPositions place an `(EXPLAIN …)` expression (X) in every position
// class: engine arguments, storage expressions, column expressions, ALTER
// actions, mutations, INSERT, SELECT and every view / CTAS body.
var explainPositions = []string{
	"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, X, 8192)",
	"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, n IN X, 8192)",
	"CREATE TABLE db1.n (k UInt8, v String) ENGINE = Join(ANY, LEFT, X)",
	"CREATE TABLE db1.n (n UInt8) ENGINE = ReplacingMergeTree(X) ORDER BY n",
	"CREATE MATERIALIZED VIEW db1.mv ENGINE = SummingMergeTree(X) ORDER BY n AS SELECT a AS n FROM db1.o",
	"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY n PARTITION BY X",
	"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY (n, X)",
	"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree PRIMARY KEY X ORDER BY n",
	"CREATE TABLE db1.n (n UInt8, d DateTime) ENGINE = MergeTree ORDER BY n TTL d + X",
	"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY n SETTINGS index_granularity = X",
	"CREATE TABLE db1.n (n UInt8 DEFAULT X) ENGINE = Memory",
	"CREATE TABLE db1.n (n UInt8 MATERIALIZED X) ENGINE = Memory",
	"CREATE TABLE db1.n (n UInt8, CONSTRAINT c CHECK n = X) ENGINE = Memory",
	"CREATE TABLE db1.n (n UInt8, INDEX i X TYPE minmax) ENGINE = MergeTree ORDER BY n",
	"CREATE TABLE db1.n (n FixedString(X)) ENGINE = Memory",
	"CREATE TABLE db1.n (n UInt8 CODEC(ZSTD(X))) ENGINE = Memory",
	"CREATE VIEW db1.v (n UInt8 DEFAULT X) AS SELECT 1 AS n",
	"CREATE TABLE db1.n ENGINE = Memory AS SELECT X AS a",
	"CREATE VIEW db1.v AS SELECT X AS a",
	"CREATE MATERIALIZED VIEW db1.mv TO db1.o AS SELECT X AS a FROM db1.o",
	"ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT X",
	"ALTER TABLE db1.o MODIFY COLUMN c UInt8 DEFAULT X",
	"ALTER TABLE db1.o ADD CONSTRAINT c CHECK a = X",
	"ALTER TABLE db1.o UPDATE a = X WHERE 1",
	"ALTER TABLE db1.o DELETE WHERE a = X",
	"ALTER TABLE db1.o MODIFY TTL d + X",
	"DELETE FROM db1.o WHERE a = X",
	"UPDATE db1.o SET a = X WHERE 1",
	"INSERT INTO db1.o VALUES (X, 1)",
	"INSERT INTO db1.o SELECT X, 1",
	"INSERT INTO db1.o SELECT * FROM X",
	"SELECT X",
	"SELECT * FROM X",
	"SELECT * FROM db1.o WHERE a = X",
	"SELECT * FROM db1.o WHERE a IN X",
	"WITH w AS X SELECT * FROM w",
	"SELECT * FROM db1.o UNION ALL SELECT X, 1",
	"SELECT * FROM view(SELECT X)",
}

func TestExplainSubquery_RefusedInEveryPosition(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, pos := range explainPositions {
			for _, kind := range explainKinds {
				for _, inner := range explainInner {
					sql := strings.Replace(pos, "X", "("+kind+" "+inner+")", 1)
					resp, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					if resp.GetCode() == pb.RewriteCode_Success {
						t.Errorf("si=%v: %q answered Success: %q", si, sql, resp.GetSqlAfterRewrite())
					}
				}
			}
		}
	}
}

func TestExplainSubquery_ExactPins(t *testing.T) {
	const unsupported = "statement is not supported"
	const siCatchAll = "storage-integrity is configured; statement class is not modelled by the rewriter and cannot be forwarded"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		sealed := unsupported
		if si {
			sealed = siCatchAll
		}
		for _, sql := range []string{
			"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (EXPLAIN SELECT 1 FROM phys.`db2.x`), 8192)",
			"CREATE TABLE db1.n (d Date, n UInt8) ENGINE = MergeTree(d, (EXPLAIN AST SELECT 1 FROM db1.o), 8192)",
			"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY n PARTITION BY (EXPLAIN SELECT 1 FROM phys.`db2.x`)",
			"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY (n, (EXPLAIN PIPELINE SELECT 1 FROM phys.`db2.x`))",
			"CREATE TABLE db1.n (n UInt8 DEFAULT (EXPLAIN SELECT 1 FROM db1.o)) ENGINE = Memory",
			"ALTER TABLE db1.o ADD COLUMN c String DEFAULT (EXPLAIN SELECT 1 FROM db1.o)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: sealed, wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

// Column data-type parameters and CODEC arguments are opaque text in the
// Polyglot AST (fix round 1, L1); ClickHouse rejects a query there before
// evaluation, so any read-bearing text is refused (fail closed). Table
// SETTINGS values are walked like a column DEFAULT.
func TestTypeAndCodecTexts_ReadsAreRefused(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"CREATE TABLE db1.n (n FixedString((SELECT 1 FROM db1.o))) ENGINE = MergeTree ORDER BY tuple()",
			"CREATE TABLE db1.n (n FixedString((SELECT 1 FROM `db2.x`))) ENGINE = MergeTree ORDER BY tuple()",
			"CREATE TABLE db1.n (n FixedString((SELECT 1 FROM phys.`db2.x`))) ENGINE = MergeTree ORDER BY tuple()",
			"CREATE TABLE db1.n (n FixedString((SELECT 1))) ENGINE = Memory",
			"CREATE TABLE db1.n (n Enum8('a' = (SELECT 1 FROM db1.o))) ENGINE = Memory",
			"CREATE TABLE db1.n (n AggregateFunction(quantile((SELECT 0.5 FROM db1.o)), UInt8)) ENGINE = Memory",
			"CREATE TABLE db1.n (n JSON(max_dynamic_paths = (SELECT 1 FROM db1.o))) ENGINE = Memory",
			"CREATE TABLE db1.n (n DateTime64(3, (SELECT 'UTC' FROM db1.o))) ENGINE = Memory",
			"CREATE TABLE db1.n (n Tuple(a FixedString((SELECT 1 FROM db1.o)))) ENGINE = Memory",
			"CREATE TABLE db1.n (n FixedString(1 IN db1.o)) ENGINE = Memory",
			"CREATE TABLE db1.n (n UInt8 CODEC(ZSTD((SELECT 1 FROM db1.o)))) ENGINE = Memory",
			"CREATE TABLE db1.n (n UInt8 CODEC(ZSTD((EXPLAIN AST SELECT 1)))) ENGINE = Memory",
			"ALTER TABLE db1.o ADD COLUMN c Enum8('a' = (SELECT 1 FROM db1.o))",
			"ALTER TABLE db1.o ADD COLUMN c UInt8 CODEC(ZSTD((SELECT 1 FROM db1.o)))",
			"CREATE MATERIALIZED VIEW db1.mv (n FixedString((SELECT 1 FROM db1.o))) ENGINE = Memory AS SELECT 'a' AS n",
			"CREATE VIEW db1.v (n FixedString((SELECT 1 FROM db1.o))) AS SELECT 'a' AS n",
			"SELECT CAST('a' AS FixedString((SELECT 1 FROM db1.o)))",
			"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY n SETTINGS index_granularity = (SELECT 8192 FROM db1.o)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: sql})
		}
		for _, c := range []struct{ sql, want string }{
			{"CREATE TABLE db1.n (n FixedString(1)) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n FixedString(1)) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n Enum8('a' = 1, 'b' = 2)) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n Enum8('a' = 1, 'b' = 2)) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n UInt8 CODEC(ZSTD(3))) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n UInt8 CODEC(ZSTD(3))) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n UInt8 CODEC(Delta, ZSTD)) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n UInt8 CODEC(Delta, ZSTD)) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n DateTime64(3, 'UTC')) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n DateTime64(3, 'UTC')) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n Tuple(a UInt8, b String)) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n Tuple(a UInt8, b String)) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n Nullable(Decimal(10, 2))) ENGINE = Memory", `CREATE TABLE phys."db1.n" (n Nullable(Decimal(10, 2))) ENGINE=Memory`},
			{"CREATE TABLE db1.n (n UInt8) ENGINE = MergeTree ORDER BY n SETTINGS index_granularity = (SELECT 8192)",
				`CREATE TABLE phys."db1.n" (n UInt8) ENGINE=MergeTree ORDER BY n SETTINGS index_granularity = (SELECT 8192)`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	// Refused statements must not name a message family other than T7 for
	// the ordinary-table cases: pin the message on the plain one.
	cases = append(cases, tablerefCase{name: "message",
		sql:      "CREATE TABLE db1.n (n FixedString((SELECT 1 FROM db1.o))) ENGINE = MergeTree ORDER BY tuple()",
		wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported})
	runTablerefCases(t, cases)
}
