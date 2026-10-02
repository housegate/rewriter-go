package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/proto"
)

// TestViewCommentKeepsTheStatementsAnswer pins rewriteViewComment: a
// CREATE [OR REPLACE] [MATERIALIZED] VIEW … AS (…) COMMENT '…' gets exactly
// the answer the statement without its COMMENT clause gets — every gate and
// policy check runs on that statement — and a Success carries the comment
// back after the rewritten body. A rejection echoes the caller's SQL.
func TestViewCommentKeepsTheStatementsAnswer(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"CREATE OR REPLACE VIEW db1.v (a String COMMENT 'x') AS (SELECT a FROM db1.o) COMMENT 'c'",
		"CREATE VIEW IF NOT EXISTS db1.v AS (SELECT a FROM db1.o) COMMENT 'it\\'s a ''view'''",
		"CREATE MATERIALIZED VIEW db1.mv TO db1.o (a String) AS (SELECT a FROM db1.p) COMMENT 'c';",
		"CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory AS (SELECT 1 AS a) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT a FROM db1.o) COMMENT 'phys.x other.secret'",
		// Policy refusals keep their own answers.
		"CREATE VIEW db1.v AS (SELECT a FROM phys.x) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT a FROM db1.t) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT a FROM hg_safe.db1__t) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT a FROM {p:Identifier}) COMMENT 'c'",
		"CREATE VIEW phys.v AS (SELECT 1) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT * FROM system.processes) COMMENT 'c'",
		"CREATE VIEW db1.v AS (SELECT a FROM remote('h', db1.o)) COMMENT 'c'",
	} {
		i := strings.LastIndex(sql, " COMMENT ")
		stripped, comment := sql[:i], strings.TrimRight(sql[i+len(" COMMENT "):], ";")
		// The comment is re-emitted as a canonical literal built from its
		// decoded value: a doubled quote becomes \'.
		comment = strings.ReplaceAll(comment, "''", `\'`)
		for _, si := range []bool{false, true} {
			for name, opts := range map[string][]*pb.RewriteOption{
				"dynamic":    tablerefOpts(si),
				"driver":     {tableRewriteDynamic(driverDynamic(si))},
				"static":     {tableRewriteStatic()},
				"no rewrite": nil,
			} {
				if si && (name == "static" || name == "no rewrite") {
					continue
				}
				t.Run(name+"/"+sql, func(t *testing.T) {
					got, err := doRewrite(e, sql, opts)
					if err != nil {
						t.Fatal(err)
					}
					want, err := doRewrite(e, stripped, opts)
					if err != nil {
						t.Fatal(err)
					}
					if want.GetCode() == pb.RewriteCode_Success {
						want.SqlAfterRewrite += " COMMENT " + comment
					} else {
						want.SqlAfterRewrite = sql
					}
					if !proto.Equal(got, want) {
						t.Fatalf("\n got %v\nwant %v", got, want)
					}
				})
			}
		}
	}
}

// TestViewCommentFormsLeftToTheParseGate pins the forms StripViewComment does
// not take: each keeps the whole-statement parse gate's refusal.
func TestViewCommentFormsLeftToTheParseGate(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			// The query is not parenthesised: ClickHouse can read the comment
			// against the last expression (CAST(a AS String) COMMENT 'c' is a
			// syntax error on 25.8 and 26.2).
			"CREATE VIEW db1.v AS SELECT a FROM db1.o COMMENT 'c'",
			"CREATE VIEW db1.v AS (SELECT 1) COMMENT 'c' COMMENT 'd'",
			"CREATE VIEW db1.v AS (SELECT 1) COMMENT 'c' FORMAT JSON",
			"CREATE VIEW db1.v AS (SELECT 1) COMMENT 'c' -- note",
			"CREATE VIEW db1.v AS (SELECT 1) COMMENT $$c$$",
			"CREATE VIEW db1.v AS (SELECT 1) COMMENT 'a' 'b'",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}
