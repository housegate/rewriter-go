package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// PreflightTableReferences applies the position-independent halves of the
// table-reference policy (spec 2026-09-26 §5) before any handler runs, in
// precedence order: identifier parameters (T2), then protected databases
// (T3, Task 4). Static mode and requests without dynamic args are untouched.
func PreflightTableReferences(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	sel := nameresolve.FindActive(opts)
	if sel.Mode != nameresolve.ModeDynamic {
		return nil, false, nil
	}
	hit, err := engine.TablePositionParameter(e, ast, sql)
	if err != nil {
		return nil, false, err
	}
	if hit {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectInvalid(resp, engine.IdentifierParameterMessage)
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	return nil, false, nil
}
