package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

func TestRewriteWrite_StorageIntegrityMutationReads(t *testing.T) {
	e := newEngine(t)
	cases := []struct {
		name      string
		sql       string
		message   string
		accessDB  string
		accessTbl string
	}{
		{
			name:      "update_assignment_logical",
			sql:       "UPDATE other.u SET x = (SELECT count() FROM db1.t) WHERE id = 1",
			message:   "storage-integrity table db1.t accepts writes only through the signed statement lane",
			accessDB:  "db1",
			accessTbl: "t",
		},
		{
			name:      "update_predicate_safe",
			sql:       "UPDATE other.u SET x = 1 WHERE id IN (SELECT id FROM hg_safe.db1__t)",
			message:   "storage-integrity physical table hg_safe.db1__t is not directly addressable",
			accessDB:  "hg_safe",
			accessTbl: "db1__t",
		},
		{
			name:      "delete_predicate_unsafe",
			sql:       "DELETE FROM other.u WHERE id IN (SELECT id FROM hg_unsafe.db1__t)",
			message:   "storage-integrity physical table hg_unsafe.db1__t is not directly addressable",
			accessDB:  "hg_unsafe",
			accessTbl: "db1__t",
		},
		{
			name:      "alter_update_assignment_logical",
			sql:       "ALTER TABLE other.u UPDATE x = (SELECT count() FROM db1.t) WHERE id = 1",
			message:   "storage-integrity table db1.t accepts writes only through the signed statement lane",
			accessDB:  "db1",
			accessTbl: "t",
		},
		{
			name:      "alter_update_predicate_safe",
			sql:       "ALTER TABLE other.u UPDATE x = 1 WHERE id IN (SELECT id FROM hg_safe.db1__t)",
			message:   "storage-integrity physical table hg_safe.db1__t is not directly addressable",
			accessDB:  "hg_safe",
			accessTbl: "db1__t",
		},
		{
			name:      "alter_delete_predicate_logical",
			sql:       "ALTER TABLE other.u DELETE WHERE id IN (SELECT id FROM db1.t)",
			message:   "storage-integrity table db1.t accepts writes only through the signed statement lane",
			accessDB:  "db1",
			accessTbl: "t",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast := mustParse(t, e, tc.sql)
			resp, handled, err := RewriteWrite(e, ast, tc.sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
				resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED ||
				resp.GetSqlAfterRewrite() != tc.sql || resp.GetMessage() != tc.message {
				t.Fatalf("response = code=%v stmt=%v sql=%q message=%q",
					resp.GetCode(), resp.GetStatementType(), resp.GetSqlAfterRewrite(), resp.GetMessage())
			}
			accessed := resp.GetOriginalAccessedTables()
			if len(accessed) != 1 || accessed[0].GetOriginalDatabase() != tc.accessDB ||
				accessed[0].GetOriginalTable() != tc.accessTbl || !accessed[0].GetIsStorageIntegrity() {
				t.Fatalf("original_accessed_tables = %+v, want one SI-marked %s.%s",
					accessed, tc.accessDB, tc.accessTbl)
			}
		})
	}
}

func TestRewriteWrite_StorageIntegrityMutationAssignmentWinsPredicate(t *testing.T) {
	e := newEngine(t)
	sql := "UPDATE other.u SET x = (SELECT count() FROM db1.t) " +
		"WHERE id IN (SELECT id FROM hg_safe.db1__t)"
	resp, handled, err := RewriteWrite(e, mustParse(t, e, sql), sql,
		dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	wantMessage := "storage-integrity table db1.t accepts writes only through the signed statement lane"
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || resp.GetMessage() != wantMessage {
		t.Fatalf("code=%v message=%q, want assignment reject %q", resp.GetCode(), resp.GetMessage(), wantMessage)
	}
	accessed := resp.GetOriginalAccessedTables()
	if len(accessed) != 1 || accessed[0].GetOriginalDatabase() != "db1" ||
		accessed[0].GetOriginalTable() != "t" || !accessed[0].GetIsStorageIntegrity() {
		t.Fatalf("assignment must be the sole recorded first hit: %+v", accessed)
	}
}

type failingMutationProbeEngine struct{ engine.Engine }

func (e failingMutationProbeEngine) ParseOne(sql string) (engine.AST, error) {
	if strings.HasPrefix(sql, "UPDATE __hg_si_probe SET ") ||
		strings.HasPrefix(sql, "DELETE FROM __hg_si_probe ") {
		return nil, errors.New("injected mutation probe failure")
	}
	return e.Engine.ParseOne(sql)
}

func TestRewriteWrite_StorageIntegrityMutationAdaptationFailsClosed(t *testing.T) {
	base := newEngine(t)
	e := failingMutationProbeEngine{Engine: base}
	sql := "ALTER TABLE other.u UPDATE x = (SELECT count() FROM db1.t) WHERE id = 1"
	ast := mustParse(t, base, sql)

	resp, handled, err := RewriteWrite(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil || !handled {
		t.Fatalf("active SI adaptation failure must be a response reject: handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
		resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED ||
		resp.GetSqlAfterRewrite() != sql || resp.GetMessage() != "statement is not supported" {
		t.Fatalf("response = code=%v stmt=%v sql=%q message=%q",
			resp.GetCode(), resp.GetStatementType(), resp.GetSqlAfterRewrite(), resp.GetMessage())
	}

	// Without active SI, the same adapter failure is irrelevant and the
	// ordinary ALTER rewrite path remains available.
	resp, handled, err = RewriteWrite(e, ast, sql, nil)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("non-SI rewrite = handled=%v err=%v code=%v message=%q",
			handled, err, resp.GetCode(), resp.GetMessage())
	}
}
