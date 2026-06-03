// Package rewriter is a native Go ClickHouse SQL rewriter, an in-process
// alternative to the C++ rewriter-grpc service. Phase 0 ships a PASS-THROUGH
// implementation: it parses + classifies + regenerates, but applies no
// rewrites. Statement-specific rewriting arrives in later phases.
package rewriter

import (
	"context"

	"github.com/housegate/rewriter-go/gen/pb"
)

// RewriteResult mirrors pb.RewriteSQLResponse with interface-friendly names.
type RewriteResult struct {
	SQL                    string
	Code                   pb.RewriteCode
	Message                string
	StatementType          pb.StatementType
	TableRewrites          map[string]string
	DatabaseRewrites       map[string]string
	OriginalAccessedTables []*pb.AccessedTable
	PrivilegesDeltas       []*pb.PrivilegeDelta
	ExistenceClause        pb.ExistenceClause
	FailedCTEAliases       []string
}

// Rewriter rewrites Sentio-Network mode SQL into real SQL, bound to one client
// connection. Fail-open: a non-nil error means forward the original SQL.
type Rewriter interface {
	Rewrite(ctx context.Context, sql, effectiveAccount string) (RewriteResult, error)
	RewriteErrorMessage(ctx context.Context, message string) (string, error)
	Close() error
}

func resultFromPB(r *pb.RewriteSQLResponse) RewriteResult {
	return RewriteResult{
		SQL:                    r.GetSqlAfterRewrite(),
		Code:                   r.GetCode(),
		Message:                r.GetMessage(),
		StatementType:          r.GetStatementType(),
		TableRewrites:          r.GetTableRewrites(),
		DatabaseRewrites:       r.GetDatabaseRewrites(),
		OriginalAccessedTables: r.GetOriginalAccessedTables(),
		PrivilegesDeltas:       r.GetPrivilegesDeltas(),
		ExistenceClause:        r.GetExistenceClause(),
		FailedCTEAliases:       r.GetFailedCteAliases(),
	}
}
