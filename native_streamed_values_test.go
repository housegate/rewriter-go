package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestStreamedFormatValues pins housegate/rewriter-go#55: clickhouse-client
// sends `INSERT … FORMAT Values` with the rows on stdin as the statement text
// alone and streams the rows as Data packets, so nothing follows the format
// name. It is answered like every other streamed format. A comment after the
// format name is refused, as rewriter-grpc refuses it (ClickHouse would read
// the comment as Values data). Inline rows keep their existing answers.
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
			tablerefCase{name: mode + "/inline_rows", si: si, sql: "INSERT INTO db1.o FORMAT Values (1)",
				wantCode: unsupported},
			tablerefCase{name: mode + "/inline_rows_next_line", si: si, sql: "INSERT INTO db1.o FORMAT Values\n(1)",
				wantCode: unsupported},
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
