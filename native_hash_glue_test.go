package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestTableRef_HashGluedKeywordRefused pins round 3, N1: the pinned Polyglot
// lexes a '#' glued to a word (`TABLE#`, `IN#`, `TO#`) as one identifier token,
// while ClickHouse ends the word there and reads '# …' / '#!…' as a line
// comment. Every statement carrying such a token is refused in both SI states,
// so a keyword can no longer vanish from Polyglot's view and let the statement
// forward verbatim (which renamed / read another tenant's table on ClickHouse
// 26.2). A real comment after whitespace, and '#' inside a string / quoted
// identifier / FORMAT payload, are unaffected.
func TestTableRef_HashGluedKeywordRefused(t *testing.T) {
	// The glued forms: a keyword with '#' stuck to it, then a comment body and a
	// newline, then the rest. `# `, `#!` and `#\n` bodies are all covered (a
	// bare `#\n` is a ClickHouse syntax error, still safe to refuse).
	glued := []string{
		"RENAME TABLE# c\n`db2.x` TO `db1.stolen`",
		"RENAME TABLE#!c\n`db2.x` TO db1.y",
		"RENAME TABLES# c\ndb1.y AND phys.`db2.x`",
		"EXCHANGE TABLES# c\ndb1.y AND phys.`db2.x`",
		"EXCHANGE TABLES#!c\ndb1.y AND phys.`db2.x`",
		"RENAME TABLE db1.a TO# c\nphys.`db2.x`",
		"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN# c\n`db2.x`",
		"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN#!c\n`db2.x`",
		"ALTER TABLE db1.o DELETE WHERE a IN# c\n`db2.x`",
		"SELECT * FROM# c\ndb1.o",
		"SELECT * FROM#!c\ndb1.o",
		"SELECT * FROM db1.o JOIN# c\nphys.`db2.x` USING (a)",
		"SELECT * FROM db1.o WHERE a AND# c\nb",
		"INSERT INTO db1.o SELECT * FROM# c\nphys.`db2.x`",
		"INSERT INTO db1.o SELECT a FROM db1.p WHERE a IN# c\n`db2.x`",
	}
	// Both a Polyglot SyntaxError (the tokenizer refusing during ParseOne) and an
	// UnsupportedStatement (the whole-statement gate refusing) are valid: the
	// statement is not forwarded either way.
	// Refused, either as a parse SyntaxError or an UnsupportedStatement: both
	// mean the statement is not forwarded.
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, sql := range glued {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if resp.GetCode() == pb.RewriteCode_Success {
				t.Errorf("si=%v %q: Success, forwarded %q; want refused", si, sql, resp.GetSqlAfterRewrite())
			}
		}
	}
}

// TestTableRef_HashCommentsStillAccepted pins that the glue rule does not touch
// a real comment, or a '#' that is data.
func TestTableRef_HashCommentsStillAccepted(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "line_comment", si: si, sql: "SELECT count() # a comment",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "bang_comment", si: si, sql: "SELECT count() #! a comment",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "comment_before_from", si: si, sql: "SELECT a #c\nFROM db1.o",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "hash_in_quoted_ident", si: si, sql: "SELECT `a#b` FROM db1.o",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "hash_in_string", si: si, sql: "SELECT 'c#d' FROM db1.o",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "hash_in_where_string", si: si, sql: "SELECT * FROM db1.o WHERE s = 'a # b'",
				wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "format_payload_hash", si: si, sql: "INSERT INTO db1.o FORMAT CSV\n1,#notacomment\n",
				wantCode: pb.RewriteCode_Success},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_HashGlueWithFormatIdentifier pins round 4, R3-1: Polyglot types
// a bare identifier `format` as a FORMAT token, but only a genuine INSERT …
// FORMAT data clause exempts the inline payload from the '#' rule. So a
// statement that merely names `format` before a glued keyword — as a WHERE
// column, an UPDATE target or value, a function name, or a table name, and in
// RENAME / EXCHANGE — is scanned in full and refused, in both SI modes.
// Verified on ClickHouse 26.2 (EXPLAIN AST reads each glued form as in(a,
// db2.x) with the '# c' as a comment).
func TestTableRef_HashGlueWithFormatIdentifier(t *testing.T) {
	glued := []string{
		"ALTER TABLE db1.o UPDATE b = 1 WHERE format = 1 OR a IN# c\n`db2.x`",
		"ALTER TABLE db1.o UPDATE format = 1 WHERE a IN# c\n`db2.x`",
		"ALTER TABLE db1.o UPDATE s = format WHERE a IN# c\n`db2.x`",
		"ALTER TABLE db1.o UPDATE b = format('{}', 'x') = '' WHERE a IN# c\n`db2.x`",
		"ALTER TABLE db1.format UPDATE b = 1 WHERE a IN# c\n`db2.x`",
		"ALTER TABLE db1.o DELETE WHERE format = 1 OR a IN# c\n`db2.x`",
		"RENAME TABLE db1.format TO db1.y, db1.a TO# c\nphys.`db2.x`",
		"EXCHANGE TABLES db1.format AND db1.a, db1.b AND# c\nphys.`db2.x`",
		"SELECT format, a IN# c\n`db2.x` FROM db1.o",
	}
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, sql := range glued {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if resp.GetCode() == pb.RewriteCode_Success {
				t.Errorf("si=%v %q: Success, forwarded %q; want refused", si, sql, resp.GetSqlAfterRewrite())
			}
		}
	}
	// A genuine INSERT … FORMAT payload that contains '#' data is still accepted,
	// and a real trailing '# comment' after the payload is a comment.
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"INSERT INTO db1.o FORMAT CSV\n1,#notacomment\n",
			"INSERT INTO db1.o FORMAT CSV 1,2,3 # c",
		} {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success {
				t.Errorf("si=%v %q: %s %q; want Success", si, sql, resp.GetCode(), resp.GetMessage())
			}
		}
	}
}
