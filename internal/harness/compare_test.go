package harness

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestCompareExactFields(t *testing.T) {
	a := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{"db.t": "p.db_t"},
	}
	b := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{"db.t": "p.db_t"},
	}
	if d := Compare(a, b, nil); !d.Equal() {
		t.Fatalf("identical responses should match, got: %v", d.Mismatches)
	}

	b.TableRewrites["db.t"] = "DIFFERENT"
	if d := Compare(a, b, nil); d.Equal() {
		t.Fatal("differing table_rewrites should mismatch")
	}
}

func TestCompareSQLSemantic(t *testing.T) {
	a := &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, SqlAfterRewrite: "SELECT a FROM t"}
	b := &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, SqlAfterRewrite: "SELECT  a  FROM  t"}
	// nil semanticEq -> exact string compare, so these differ (whitespace).
	if d := Compare(a, b, nil); d.Equal() {
		t.Fatal("without semantic compare, whitespace differences should mismatch")
	}
	// semantic compare ignoring whitespace -> match.
	eq := func(s1, s2 string) (bool, error) { return normalizeWS(s1) == normalizeWS(s2), nil }
	if d := Compare(a, b, eq); !d.Equal() {
		t.Fatalf("with semantic compare, should match; got %v", d.Mismatches)
	}
}

func TestCompare_privilegeDeltas(t *testing.T) {
	mk := func() *pb.RewriteSQLResponse {
		return &pb.RewriteSQLResponse{PrivilegesDeltas: []*pb.PrivilegeDelta{{
			Action: pb.PrivilegeDelta_ACTION_GRANT, Scope: pb.PrivilegeDelta_SCOPE_TABLE,
			LogicalDatabase: "l", PhysicalDatabase: "p", OriginalTable: "t", PhysicalTable: "l.t",
			Privileges: []string{"SELECT"},
			Grantees:   []*pb.PrivilegeDelta_Grantee{{Name: "u"}},
		}}}
	}
	if d := Compare(mk(), mk(), nil); !d.Equal() {
		t.Errorf("identical deltas should match: %v", d.Mismatches)
	}
	diff := mk()
	diff.PrivilegesDeltas[0].Privileges = []string{"INSERT"}
	if d := Compare(diff, mk(), nil); d.Equal() {
		t.Error("differing privileges should not match")
	}
}
