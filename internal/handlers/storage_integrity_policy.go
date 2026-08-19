package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// rejectStorageIntegrityNamespaces applies one fail-closed policy to every AST
// surface that carries a table namespace outside ordinary table nodes. It
// covers table functions, IN/GLOBAL IN <table>, table-engine source args, and
// local CLICKHOUSE dictionary sources. Any unresolved recognized surface is
// rejected conservatively and annotated with SI metadata so downstream legacy
// fail-open gates cannot forward it.
func rejectStorageIntegrityNamespaces(resp *pb.RewriteSQLResponse, refs []engine.NamespaceRef, sel nameresolve.Selection, code pb.RewriteCode) bool {
	if sel.Mode != nameresolve.ModeDynamic || len(sel.Dynamic.GetStorageIntegrity().GetTables()) == 0 {
		return false
	}
	for _, ref := range refs {
		target := ref.Target
		if ref.UsesCurrentDatabase {
			// First retain logical-session semantics: an unqualified exact SI key
			// must remain protected even when ClickHouse executes in a different
			// physical database. Then resolve the actual physical execution context
			// for protocol-owned hg_safe/hg_unsafe namespace checks.
			if target.Table != "" {
				if _, key, ok := nameresolve.LookupStorageIntegrity("", target.Table, sel.Dynamic); ok {
					logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical("", sel.Dynamic)
					recordAccessedWriteUnique(resp, target, sel)
					if !authorized {
						resp.Code = pb.RewriteCode_InvalidRewriteRequest
						resp.Message = nameresolve.StorageIntegrityUnauthorizedMessage(logical)
						return true
					}
					resp.Code = code
					resp.Message = namespaceLogicalRejectMessage(ref, key)
					return true
				}
			}
			target.DB = tableFunctionExecutionDatabase(sel.Dynamic)
			ref.Resolved = target.DB != "" && target.Table != ""
		}
		if ref.Resolved {
			if nameresolve.IsStorageIntegrityPhysicalDatabase(target.DB, sel.Dynamic) {
				recordAccessedWriteUnique(resp, target, sel)
				resp.Code = code
				resp.Message = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(target.DB, target.Table))
				return true
			}
			if _, key, ok := nameresolve.LookupStorageIntegrity(target.DB, target.Table, sel.Dynamic); ok {
				logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical(target.DB, sel.Dynamic)
				recordAccessedWriteUnique(resp, target, sel)
				if !authorized {
					resp.Code = pb.RewriteCode_InvalidRewriteRequest
					resp.Message = nameresolve.StorageIntegrityUnauthorizedMessage(logical)
					return true
				}
				resp.Code = code
				resp.Message = namespaceLogicalRejectMessage(ref, key)
				return true
			}
			// Regex-bearing surfaces such as merge(db, regexp) may cover an SI
			// table even when the table text is not an exact key. Reserve the whole
			// logical SI database for every indirect namespace surface.
			if nameresolve.IsStorageIntegrityLogicalDatabase(target.DB, sel.Dynamic) {
				logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical(target.DB, sel.Dynamic)
				recordAccessedStorageIntegrityLogicalDatabaseUnique(resp, target.DB, sel.Dynamic)
				if !authorized {
					resp.Code = pb.RewriteCode_InvalidRewriteRequest
					resp.Message = nameresolve.StorageIntegrityUnauthorizedMessage(logical)
					return true
				}
				resp.Code = code
				resp.Message = namespaceLogicalDatabaseRejectMessage(ref, target.DB)
				return true
			}
			continue
		}
		if target.DB != "" {
			if nameresolve.IsStorageIntegrityPhysicalDatabase(target.DB, sel.Dynamic) {
				recordAccessedDatabaseUnique(resp, target.DB, sel.Dynamic)
				resp.Code = code
				resp.Message = namespaceDatabaseRejectMessage(ref, target.DB)
				return true
			}
			if nameresolve.IsStorageIntegrityLogicalDatabase(target.DB, sel.Dynamic) {
				logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical(target.DB, sel.Dynamic)
				recordAccessedStorageIntegrityLogicalDatabaseUnique(resp, target.DB, sel.Dynamic)
				if !authorized {
					resp.Code = pb.RewriteCode_InvalidRewriteRequest
					resp.Message = nameresolve.StorageIntegrityUnauthorizedMessage(logical)
					return true
				}
				resp.Code = code
				resp.Message = namespaceLogicalDatabaseRejectMessage(ref, target.DB)
				return true
			}
		}
		// Even a statically ordinary database with a dynamic table expression is
		// not a proof of the final execution namespace for every supported
		// surface (named collections and constant folding can intervene). The SI
		// contract requires proof before acknowledgement, so fail closed.
		for _, physicalDB := range nameresolve.StorageIntegrityPhysicalDatabases(sel.Dynamic) {
			recordAccessedDatabaseUnique(resp, physicalDB, sel.Dynamic)
		}
		resp.Code = code
		resp.Message = namespaceUnresolvedRejectMessage(ref)
		return true
	}
	return false
}

func namespaceLogicalRejectMessage(ref engine.NamespaceRef, key string) string {
	if ref.Source == engine.NamespaceRefTableEngine {
		return nameresolve.StorageIntegrityWriteRejectMessage(key)
	}
	return "storage-integrity table " + key + " is not directly addressable through " + namespaceSurface(ref)
}

func namespaceLogicalDatabaseRejectMessage(ref engine.NamespaceRef, db string) string {
	return "storage-integrity logical database " + db + " is not directly addressable through " + namespaceSurface(ref)
}

func namespaceDatabaseRejectMessage(ref engine.NamespaceRef, db string) string {
	if ref.Source == engine.NamespaceRefTableFunction {
		return nameresolve.StorageIntegrityTableFunctionNamespaceRejectMessage(db)
	}
	return "storage-integrity " + namespaceSurface(ref) + " namespace " + db + " is not directly addressable"
}

func namespaceUnresolvedRejectMessage(ref engine.NamespaceRef) string {
	if ref.Source == engine.NamespaceRefTableFunction {
		return nameresolve.StorageIntegrityUnresolvedTableFunctionNamespaceRejectMessage()
	}
	return "storage-integrity " + namespaceSurface(ref) + " namespace is not statically resolvable"
}

func namespaceSurface(ref engine.NamespaceRef) string {
	switch ref.Source {
	case engine.NamespaceRefInTable:
		return ref.Name + " table target"
	case engine.NamespaceRefTableEngine:
		return ref.Name + " table engine"
	case engine.NamespaceRefDictionarySource:
		return ref.Name + " dictionary source"
	default:
		if ref.Name != "" {
			return ref.Name + " table function"
		}
		return "table function"
	}
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

func recordAccessedStorageIntegrityLogicalDatabaseUnique(resp *pb.RewriteSQLResponse, db string, dyn *pb.RewriteTableDynamicArgs) {
	for _, accessed := range resp.GetOriginalAccessedTables() {
		if accessed.GetOriginalDatabase() == db && accessed.GetOriginalTable() == "" && accessed.GetIsStorageIntegrity() {
			return
		}
	}
	physical, _ := nameresolve.ResolvePhysicalDatabase(db, dyn)
	resp.OriginalAccessedTables = append(resp.OriginalAccessedTables, &pb.AccessedTable{
		OriginalDatabase:   db,
		LogicalDatabase:    db,
		PhysicalDatabase:   physical,
		IsStorageIntegrity: true,
	})
}
