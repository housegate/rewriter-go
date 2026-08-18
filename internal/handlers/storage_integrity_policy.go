package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// rejectStorageIntegrityTableFunctions applies the protocol-owned physical
// namespace policy to recognized remote/cluster/merge functions. It preserves
// a statically known database even when the table argument is an expression;
// a wholly dynamic database is rejected conservatively and annotated with all
// configured SI physical namespaces so downstream fail-closed gates can prove
// the rejection belongs to the SI contract.
func rejectStorageIntegrityTableFunctions(resp *pb.RewriteSQLResponse, refs []engine.TableFunctionRef, sel nameresolve.Selection, code pb.RewriteCode) bool {
	if sel.Mode != nameresolve.ModeDynamic || len(sel.Dynamic.GetStorageIntegrity().GetTables()) == 0 {
		return false
	}
	for _, ref := range refs {
		if ref.UsesCurrentDatabase {
			ref.Target.DB = tableFunctionExecutionDatabase(sel.Dynamic)
			ref.Resolved = ref.Target.DB != "" && ref.Target.Table != ""
		}
		if ref.Resolved {
			if nameresolve.IsStorageIntegrityPhysicalDatabase(ref.Target.DB, sel.Dynamic) {
				recordAccessedWriteUnique(resp, ref.Target, sel)
				resp.Code = code
				resp.Message = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(ref.Target.DB, ref.Target.Table))
				return true
			}
			continue
		}
		if ref.Target.DB != "" {
			if !nameresolve.IsStorageIntegrityPhysicalDatabase(ref.Target.DB, sel.Dynamic) {
				continue // a statically ordinary database cannot cross into an SI namespace
			}
			recordAccessedDatabaseUnique(resp, ref.Target.DB, sel.Dynamic)
			resp.Code = code
			resp.Message = nameresolve.StorageIntegrityTableFunctionNamespaceRejectMessage(ref.Target.DB)
			return true
		}
		for _, physicalDB := range nameresolve.StorageIntegrityPhysicalDatabases(sel.Dynamic) {
			recordAccessedDatabaseUnique(resp, physicalDB, sel.Dynamic)
		}
		resp.Code = code
		resp.Message = nameresolve.StorageIntegrityUnresolvedTableFunctionNamespaceRejectMessage()
		return true
	}
	return false
}

// tableFunctionExecutionDatabase resolves ClickHouse's current *physical*
// database for one-argument merge(<table-regexp>). The logical session context
// is not necessarily the database where ClickHouse executes the function.
// Prefer the explicit physical context, then derive it through database_map;
// a context that is itself a configured SI physical namespace remains valid
// even when it is intentionally absent from known_physical_databases. Empty
// means indeterminate and the caller conservatively rejects every SI namespace.
func tableFunctionExecutionDatabase(dyn *pb.RewriteTableDynamicArgs) string {
	if physical := dyn.GetUpstreamPhysicalDatabaseInContext(); physical != "" {
		return physical
	}
	logical := dyn.GetUpstreamLogicalDatabaseInContext()
	if nameresolve.IsStorageIntegrityPhysicalDatabase(logical, dyn) {
		return logical
	}
	if physical, ok := nameresolve.ResolvePhysicalDatabase(logical, dyn); ok {
		return physical
	}
	return ""
}

func recordAccessedWriteUnique(resp *pb.RewriteSQLResponse, tt engine.TableTarget, sel nameresolve.Selection) {
	for _, accessed := range resp.GetOriginalAccessedTables() {
		if accessed.GetOriginalDatabase() == tt.DB && accessed.GetOriginalTable() == tt.Table {
			return
		}
	}
	recordAccessedWrite(resp, tt, sel)
}

func recordAccessedDatabaseUnique(resp *pb.RewriteSQLResponse, db string, dyn *pb.RewriteTableDynamicArgs) {
	for _, accessed := range resp.GetOriginalAccessedTables() {
		if accessed.GetOriginalDatabase() == db && accessed.GetOriginalTable() == "" {
			return
		}
	}
	recordAccessedDatabase(resp, db, dyn)
}
