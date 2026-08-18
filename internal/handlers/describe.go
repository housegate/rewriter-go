package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// describeMetadataSQL renders the metadata-shaped SELECT that stands in for
// DESCRIBE on a storage-integrity table (Spec G §4.3): the safe table's
// columns minus the reserved row-id column, in declaration order. Built as
// a string (not via the generator) so both engines emit byte-identical SQL.
func describeMetadataSQL(safeTable, rid string) string {
	db, table := splitPhysicalName(safeTable)
	return "SELECT name, type, default_type, default_expression, comment, codec_expression, ttl_expression FROM system.columns WHERE database = '" +
		escapeSQLLiteral(db) + "' AND table = '" + escapeSQLLiteral(table) + "' AND name != '" + escapeSQLLiteral(rid) + "' ORDER BY position"
}

// RewriteDescribe handles `DESCRIBE|DESC [TABLE] [db.]t` (an opaque command
// node under polyglot). G-minimal scope (plan deviation D-7): classify as
// STATEMENT_TYPE_DESCRIBE; a storage-integrity target becomes the
// system.columns metadata SELECT; any other target passes through unchanged
// (Spec E D6 adds the ordinary EXISTS-style physical resolution). Returns
// (resp, handled, err) with the RewriteWrite contract; native.go calls it
// before RewriteExistsShowCreate.
func RewriteDescribe(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	if kind != engine.NodeCommand {
		return nil, false, nil
	}
	t, err := engine.ParseObjectTarget(e, sql)
	if err != nil {
		return nil, false, err
	}
	if t.Verb != engine.VerbDescribe {
		return nil, false, nil
	}
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_DESCRIBE)
	sel := nameresolve.FindActive(opts)
	tt := engine.TableTarget{DB: t.DB, Table: t.Table}
	if sel.Mode == nameresolve.ModeDynamic {
		if tbl, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
			recordAccessedWrite(resp, tt, sel)
			resp.SqlAfterRewrite = describeMetadataSQL(tbl.GetSafeTable(), nameresolve.ReservedRowIDColumn(sel.Dynamic))
			return resp, true, nil
		}
	}
	recordAccessedWrite(resp, tt, sel)
	resp.SqlAfterRewrite = sql // pass through (Spec E D6 will resolve non-SI targets)
	return resp, true, nil
}
