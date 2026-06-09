// Package harness runs the native rewriter and the C++ oracle over a corpus and
// diffs their responses. Comparison is exact for structured fields and
// semantic (caller-supplied) for sql_after_rewrite.
package harness

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
)

// SemanticEq reports whether two SQL strings are semantically equal.
type SemanticEq func(sql1, sql2 string) (bool, error)

// Diff is the set of field mismatches between two responses.
type Diff struct {
	Mismatches []string
}

func (d Diff) Equal() bool { return len(d.Mismatches) == 0 }

// Compare diffs two responses. If semanticEq is nil, sql_after_rewrite is
// compared as an exact string.
func Compare(got, want *pb.RewriteSQLResponse, semanticEq SemanticEq) Diff {
	var d Diff
	add := func(f string, a, b any) {
		d.Mismatches = append(d.Mismatches, fmt.Sprintf("%s: got %v, want %v", f, a, b))
	}

	if got.GetCode() != want.GetCode() {
		add("code", got.GetCode(), want.GetCode())
	}
	if got.GetStatementType() != want.GetStatementType() {
		add("statement_type", got.GetStatementType(), want.GetStatementType())
	}
	if got.GetExistenceClause() != want.GetExistenceClause() {
		add("existence_clause", got.GetExistenceClause(), want.GetExistenceClause())
	}
	if !mapEq(got.GetTableRewrites(), want.GetTableRewrites()) {
		add("table_rewrites", got.GetTableRewrites(), want.GetTableRewrites())
	}
	if !mapEq(got.GetDatabaseRewrites(), want.GetDatabaseRewrites()) {
		add("database_rewrites", got.GetDatabaseRewrites(), want.GetDatabaseRewrites())
	}
	if !reflect.DeepEqual(got.GetFailedCteAliases(), want.GetFailedCteAliases()) {
		add("failed_cte_aliases", got.GetFailedCteAliases(), want.GetFailedCteAliases())
	}
	if !privilegeDeltasEqual(got.GetPrivilegesDeltas(), want.GetPrivilegesDeltas()) {
		add("privileges_deltas", got.GetPrivilegesDeltas(), want.GetPrivilegesDeltas())
	}

	gs, ws := got.GetSqlAfterRewrite(), want.GetSqlAfterRewrite()
	if semanticEq == nil {
		if gs != ws {
			add("sql_after_rewrite(exact)", gs, ws)
		}
	} else if eq, err := semanticEq(gs, ws); err != nil {
		add("sql_after_rewrite(semantic-error)", err.Error(), "")
	} else if !eq {
		add("sql_after_rewrite(semantic)", gs, ws)
	}
	return d
}

// privilegeDeltasEqual compares two PrivilegeDelta lists field-by-field (proto
// messages can't be reflect.DeepEqual'd — they carry unexported state). Order is
// significant: the per-privilege fan-out preserves source order.
func privilegeDeltasEqual(a, b []*pb.PrivilegeDelta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.GetAction() != y.GetAction() || x.GetScope() != y.GetScope() ||
			x.GetOriginalDatabase() != y.GetOriginalDatabase() ||
			x.GetLogicalDatabase() != y.GetLogicalDatabase() ||
			x.GetPhysicalDatabase() != y.GetPhysicalDatabase() ||
			x.GetOriginalTable() != y.GetOriginalTable() ||
			x.GetPhysicalTable() != y.GetPhysicalTable() ||
			x.GetGrantOption() != y.GetGrantOption() ||
			!reflect.DeepEqual(x.GetPrivileges(), y.GetPrivileges()) ||
			!granteesEqual(x.GetGrantees(), y.GetGrantees()) {
			return false
		}
	}
	return true
}

func granteesEqual(a, b []*pb.PrivilegeDelta_Grantee) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetName() != b[i].GetName() || a[i].GetIsCurrentUser() != b[i].GetIsCurrentUser() {
			return false
		}
	}
	return true
}

// mapEq compares two string maps treating nil and empty as equal. proto3 emits a
// nil map for an empty map on the wire (the C++ oracle), while the native rewriter
// often inits an empty non-nil map (newWriteResp/newDBResp) — semantically
// identical, so reflect.DeepEqual (which distinguishes nil from {}) is wrong here.
func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// normalizeWS collapses runs of whitespace to single spaces (trimmed).
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }
