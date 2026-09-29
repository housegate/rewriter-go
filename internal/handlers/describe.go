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
	// system.columns exposes default_kind; alias it to the native DESCRIBE
	// result's default_type column. Protocol-owned SI DDL never declares a
	// column CODEC or TTL, so the two remaining native DESCRIBE fields are
	// static empty strings (and ttl_expression is not a system.columns field
	// on ClickHouse 25.8). This preserves the executable seven-field shape.
	return "SELECT name, type, default_kind AS default_type, default_expression, comment, '' AS codec_expression, '' AS ttl_expression FROM system.columns WHERE database = '" +
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
	if sel.Mode == nameresolve.ModeDynamic && (t.Shape == engine.ObjectTargetSubquery || t.Shape == engine.ObjectTargetNone) {
		// Spec 2026-09-26 R7: DESCRIBE (SELECT …) is not run through the
		// SELECT pipeline, so its subquery is refused rather than forwarded
		// unrewritten; so is a DESCRIBE with no target at all.
		msg := engine.UnsupportedStatementMessage
		if nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
			msg = nameresolve.StorageIntegrityUnmodelledMessage
			if t.Shape == engine.ObjectTargetSubquery {
				if tt, ok := describeSubqueryStorageIntegrityTable(e, sql[t.SubqueryStart:], sel); ok {
					msg = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(tt.DB, tt.Table))
				}
			}
		}
		rejectUnsupported(resp, msg)
		return resp, true, nil
	}
	if sel.Mode == nameresolve.ModeDynamic {
		if t.ObjType == "DATABASE" && nameresolve.IsStorageIntegrityPhysicalDatabase(t.Table, sel.Dynamic) {
			recordAccessedDatabase(resp, t.Table, sel.Dynamic)
			rejectUnsupported(resp, nameresolve.StorageIntegrityPhysicalDatabaseRejectMessage(t.Table))
			return resp, true, nil
		}
		effectiveDB := t.DB
		if effectiveDB == "" {
			effectiveDB = sel.Dynamic.GetUpstreamLogicalDatabaseInContext()
		}
		if t.ObjType != "DATABASE" && nameresolve.IsStorageIntegrityPhysicalDatabase(effectiveDB, sel.Dynamic) {
			tt := engine.TableTarget{DB: t.DB, Table: t.Table}
			recordAccessedWrite(resp, tt, sel)
			rejectUnsupported(resp, nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(effectiveDB, t.Table)))
			return resp, true, nil
		}
	}
	if t.ObjType != "TABLE" {
		rejectUnsupported(resp, "DESCRIBE "+t.ObjType+" is not supported; only DESCRIBE TABLE is allowed")
		return resp, true, nil
	}
	tt := engine.TableTarget{DB: t.DB, Table: t.Table}
	recordAccessedWrite(resp, tt, sel)
	if sel.Mode == nameresolve.ModeDynamic {
		if _, ok := nameresolve.LookupStorageIntegrityPhysical(tt.DB, tt.Table, sel.Dynamic); ok {
			rejectUnsupported(resp, nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(tt.DB, tt.Table)))
			return resp, true, nil
		}
		if tbl, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
			logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical(tt.DB, sel.Dynamic)
			if !authorized {
				rejectInvalid(resp, nameresolve.StorageIntegrityUnauthorizedMessage(logical))
				return resp, true, nil
			}
			resp.SqlAfterRewrite = describeMetadataSQL(tbl.GetSafeTable(), nameresolve.ReservedRowIDColumn(sel.Dynamic))
			return resp, true, nil
		}
	}
	if sel.Mode == nameresolve.ModeDynamic && t.Shape == engine.ObjectTargetName && tt.DB == "" {
		// Spec 2026-09-26 R7: an unqualified target (dotted or not) resolves
		// exactly like FROM, in the session's logical database, as EXISTS /
		// SHOW CREATE already do — passed through, ClickHouse would bind it
		// in the physical database. A qualified ordinary target still passes
		// through unchanged (it names a logical database ClickHouse does not
		// have); a protected one was refused by the preflight.
		resp.OriginalAccessedTables = nil
		d, ok := decideWriteTarget(tt, "DESCRIBE TABLE", sel, resp)
		if !ok {
			return resp, true, nil
		}
		db, table := t.DB, t.Table
		if d.Action == engine.ActionRename {
			db, table = d.NewDB, d.NewTable
		}
		resp.SqlAfterRewrite = buildObjectSQL("DESCRIBE", t.Temporary, db, table)
		return resp, true, nil
	}
	resp.SqlAfterRewrite = sql // a table-function target (T5-classified by the preflight) or static mode
	return resp, true, nil
}

// describeSubqueryStorageIntegrityTable reports the first table of a
// DESCRIBE (SELECT …) body that lives in a storage-integrity physical
// database, so the SI-surface refusal names the object (spec 2026-09-26 §5).
func describeSubqueryStorageIntegrityTable(e engine.Engine, body string, sel nameresolve.Selection) (engine.TableTarget, bool) {
	ast, err := e.ParseOne(body)
	if err != nil {
		return engine.TableTarget{}, false
	}
	tables, err := engine.CollectSelectTables(ast)
	if err != nil {
		return engine.TableTarget{}, false
	}
	for _, tt := range tables {
		if tt.DB != "" && nameresolve.IsStorageIntegrityPhysicalDatabase(tt.DB, sel.Dynamic) {
			return tt, true
		}
	}
	return engine.TableTarget{}, false
}
