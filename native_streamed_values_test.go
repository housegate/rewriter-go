package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestStreamedFormatValues pins housegate/rewriter-go#55: clickhouse-client
// sends `INSERT … FORMAT Values` with the rows on stdin as the statement text
// alone and streams the rows as Data packets, so nothing follows the format
// name. It is answered like every other streamed format. A comment after the
// format name is refused, as rewriter-grpc refuses it (ClickHouse would read
// the comment as Values data). Inline rows are answered like VALUES rows.
func TestStreamedFormatValues(t *testing.T) {
	unsupported := pb.RewriteCode_UnsupportedStatement
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		mode := "si_off"
		if si {
			mode = "si_v2"
		}
		cases = append(cases,
			tablerefCase{name: mode + "/bare", si: si, sql: "INSERT INTO db1.o FORMAT Values",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" FORMAT Values`, wantAcc: []string{"db1.o"}},
			tablerefCase{name: mode + "/columns", si: si, sql: "INSERT INTO db1.o (a, b) FORMAT Values",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" (a, b) FORMAT Values`, wantAcc: []string{"db1.o"}},
			tablerefCase{name: mode + "/lower", si: si, sql: "INSERT INTO db1.o FORMAT values",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" FORMAT values`},
			tablerefCase{name: mode + "/upper", si: si, sql: "INSERT INTO db1.o FORMAT VALUES",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" FORMAT VALUES`},
			tablerefCase{name: mode + "/settings", si: si, sql: "INSERT INTO db1.o SETTINGS max_threads = 1 FORMAT Values",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" SETTINGS max_threads = 1 FORMAT Values`},
			// Whitespace after the format name is spliced back verbatim, as
			// for every other streamed format (`FORMAT CSV\n`).
			tablerefCase{name: mode + "/newline", si: si, sql: "INSERT INTO db1.o FORMAT Values\n",
				wantCode: pb.RewriteCode_Success, wantSQL: "INSERT INTO phys.\"db1.o\" FORMAT Values\n"},
			tablerefCase{name: mode + "/crlf", si: si, sql: "INSERT INTO db1.o FORMAT Values\r\n",
				wantCode: pb.RewriteCode_Success, wantSQL: "INSERT INTO phys.\"db1.o\" FORMAT Values\r\n"},
			tablerefCase{name: mode + "/spaces", si: si, sql: "INSERT INTO db1.o FORMAT Values  \t",
				wantCode: pb.RewriteCode_Success, wantSQL: "INSERT INTO phys.\"db1.o\" FORMAT Values  \t"},
			tablerefCase{name: mode + "/si_table", si: si, sql: "INSERT INTO db1.t FORMAT Values",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.t" FORMAT Values`, wantAcc: []string{"db1.t"}},
			tablerefCase{name: mode + "/line_comment", si: si, sql: "INSERT INTO db1.o FORMAT Values -- c",
				wantCode: unsupported, wantMsg: "statement is not supported"},
			tablerefCase{name: mode + "/block_comment", si: si, sql: "INSERT INTO db1.o FORMAT Values /* c */",
				wantCode: unsupported, wantMsg: "statement is not supported"},
			tablerefCase{name: mode + "/comment_next_line", si: si, sql: "INSERT INTO db1.o FORMAT Values\n-- c",
				wantCode: unsupported, wantMsg: "statement is not supported"},
			// Unchanged answers.
			tablerefCase{name: mode + "/semicolon", si: si, sql: "INSERT INTO db1.o FORMAT Values;",
				wantCode: pb.RewriteCode_SyntaxError},
			// Inline rows are answered like VALUES rows (#75,
			// TestInlineFormatValues).
			tablerefCase{name: mode + "/inline_rows", si: si, sql: "INSERT INTO db1.o FORMAT Values (1)",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" VALUES (1)`},
			tablerefCase{name: mode + "/inline_rows_next_line", si: si, sql: "INSERT INTO db1.o FORMAT Values\n(1)",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" VALUES (1)`},
			tablerefCase{name: mode + "/csv", si: si, sql: "INSERT INTO db1.o FORMAT CSV",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" FORMAT CSV`},
			tablerefCase{name: mode + "/csv_comment", si: si, sql: "INSERT INTO db1.o FORMAT CSV -- c",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" FORMAT CSV -- c`},
			tablerefCase{name: mode + "/values_clause", si: si, sql: "INSERT INTO db1.o VALUES (1)",
				wantCode: pb.RewriteCode_Success},
		)
	}
	runTablerefCases(t, cases)
}

// TestInlineFormatValues pins housegate/rewriter-go#75: ClickHouse runs
// `INSERT … FORMAT Values <rows>` exactly like `INSERT … VALUES <rows>`
// (measured on 25.8 and 26.8, any case of the format name), and rewriter-grpc
// rewrites it to the VALUES form. Polyglot parses the rows into the same AST as
// VALUES rows, so every answer, row checks included, is the VALUES answer.
//
// The one difference is where ClickHouse starts reading the data: right after
// the format name, where after VALUES its lexer first skips comments. A comment
// anywhere in the text after the format name is therefore Values data to
// ClickHouse (a parse error on 25.8), and it is refused as rewriter-grpc
// refuses it.
func TestInlineFormatValues(t *testing.T) {
	e := newEngine(t)
	rows := []string{
		"(1)",
		"(1, 'x'), (2, 'y')",
		"(now())",
		"(1);",
		"((SELECT max(a) FROM db1.p))",
		"(1 IN `db2.x`)",
		"(1 IN phys.x)",
		"(dictGet('db2.d', 'v', 1))",
		"(dictGet('db1.d', 'v', 1))",
		"(1), ((SELECT a FROM db1.p))",
		"((SELECT count() FROM db1.t))",
		"(1 IN db1.t)",
		"('a -- not a comment', '/* nor this */')",
	}
	targets := []string{"db1.o", "db1.o (a, b)", "db1.t", "db1.o SETTINGS max_threads = 1"}
	names := []string{"Values", "values", "VALUES", "vAlUeS"}
	for _, si := range []bool{false, true} {
		for _, target := range targets {
			for _, r := range rows {
				want, err := doRewrite(e, "INSERT INTO "+target+" VALUES "+r, tablerefOpts(si))
				if err != nil {
					t.Fatalf("doRewrite(VALUES): %v", err)
				}
				for i, name := range names {
					sep := " "
					if i == 1 {
						sep = "\n"
					}
					sql := "INSERT INTO " + target + " FORMAT " + name + sep + r
					got, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					wantSQL := want.GetSqlAfterRewrite()
					if want.GetCode() != pb.RewriteCode_Success {
						// A refusal echoes the caller's SQL.
						wantSQL = sql
					}
					if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() ||
						got.GetSqlAfterRewrite() != wantSQL ||
						accessedNames(got) != accessedNames(want) {
						t.Errorf("si=%v %q = %s %q %q [%s]; VALUES form = %s %q %q [%s]", si, sql,
							got.GetCode(), got.GetMessage(), got.GetSqlAfterRewrite(), accessedNames(got),
							want.GetCode(), want.GetMessage(), wantSQL, accessedNames(want))
					}
				}
			}
		}
	}

	unsupported := pb.RewriteCode_UnsupportedStatement
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		mode := "si_off"
		if si {
			mode = "si_v2"
		}
		for _, c := range []struct{ name, sql string }{
			{"comment_after_rows", "INSERT INTO db1.o FORMAT Values (1) -- c"},
			{"block_comment_after_rows", "INSERT INTO db1.o FORMAT Values (1) /* c */"},
			{"comment_line_after_rows", "INSERT INTO db1.o FORMAT Values (1)\n-- c"},
			{"comment_before_rows", "INSERT INTO db1.o FORMAT Values /* c */ (1)"},
			{"comment_line_before_rows", "INSERT INTO db1.o FORMAT Values -- c\n(1)"},
			{"comment_in_row", "INSERT INTO db1.o FORMAT Values (1 /* c */)"},
			{"comment_between_rows", "INSERT INTO db1.o FORMAT Values (1), /* c */ (2)"},
			{"comment_after_semicolon", "INSERT INTO db1.o FORMAT Values (1); -- c"},
		} {
			cases = append(cases, tablerefCase{name: mode + "/" + c.name, si: si, sql: c.sql,
				wantCode: unsupported, wantMsg: "statement is not supported"})
		}
		cases = append(cases,
			tablerefCase{name: mode + "/blank_lines", si: si, sql: "INSERT INTO db1.o FORMAT Values\n\n(1),\n(2)\n",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" VALUES (1), (2)`, wantAcc: []string{"db1.o"}},
			tablerefCase{name: mode + "/no_space", si: si, sql: "INSERT INTO db1.o FORMAT Values(1)",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" VALUES (1)`},
			// A quoted format name is not the Values keyword to Polyglot;
			// it keeps its answer.
			tablerefCase{name: mode + "/quoted_name", si: si, sql: "INSERT INTO db1.o FORMAT `Values` (1)",
				wantCode: unsupported},
			// Columns named format / values are not a FORMAT clause.
			tablerefCase{name: mode + "/format_column", si: si, sql: "INSERT INTO db1.o (format, values) VALUES (1, 2)",
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" (format, values) VALUES (1, 2)`},
		)
	}
	runTablerefCases(t, cases)
}

func accessedNames(r *pb.RewriteSQLResponse) string {
	var out []string
	for _, a := range r.GetOriginalAccessedTables() {
		out = append(out, a.GetOriginalDatabase()+"."+a.GetOriginalTable()+"->"+a.GetPhysicalDatabase())
	}
	return strings.Join(out, ",")
}
