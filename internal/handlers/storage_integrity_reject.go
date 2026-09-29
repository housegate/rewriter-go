package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// AnnotateStorageIntegrityReject upgrades the message of an already-rejected
// response so it names the storage-integrity object the statement addressed
// (Spec I D2). It runs for every non-Success response while the v1 contract is
// active, which is what gives SYSTEM / CHECK / TRUNCATE DATABASE / ALTER
// DATABASE / DROP DICTIONARY / CREATE LIVE VIEW a useful message without
// teaching each handler about storage integrity.
//
// It is deliberately message-only:
//   - Success responses are never touched.
//   - A response that already carries an SI-flagged accessed table went through
//     real SI policy and owns its own (more precise) message.
//   - A response with SI-classified accessed metadata already went through
//     structured policy and keeps that handler's more precise message.
//   - When syntax proves no storage-integrity object, the caller's message
//     stands (the catch-all caller already supplies the generic D1 refusal).
func AnnotateStorageIntegrityReject(e engine.Engine, resp *pb.RewriteSQLResponse, sql string, sel nameresolve.Selection) {
	ast, _ := e.ParseOne(sql)
	AnnotateStorageIntegrityRejectAST(e, resp, ast, sql, sel)
}

// AnnotateStorageIntegrityRejectAST is the production entry point. The shared
// finalize path already owns the parsed statement, so structured D2 inspection
// reuses it instead of reparsing and risking a second source-order authority.
func AnnotateStorageIntegrityRejectAST(e engine.Engine, resp *pb.RewriteSQLResponse, ast engine.AST, sql string, sel nameresolve.Selection) {
	if resp.GetCode() == pb.RewriteCode_Success {
		return
	}
	if sel.Mode != nameresolve.ModeDynamic || !nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
		return
	}
	if touchesStorageIntegrity(resp.GetOriginalAccessedTables()) {
		return
	}
	refs, err := engine.NameRefsFromAST(e, ast, sql)
	if err != nil {
		return // inability to prove a target leaves the caller's message intact
	}
	for _, ref := range refs {
		switch ref.Kind {
		case engine.NameRefDatabase:
			if nameresolve.IsStorageIntegrityPhysicalDatabase(ref.DB, sel.Dynamic) {
				resp.Message = nameresolve.StorageIntegrityPhysicalDatabaseRejectMessage(ref.DB)
				return
			}
		case engine.NameRefTable:
			physicalDB := ref.DB
			if physicalDB == "" {
				physicalDB = sel.Dynamic.GetUpstreamLogicalDatabaseInContext()
			}
			if nameresolve.IsStorageIntegrityPhysicalDatabase(physicalDB, sel.Dynamic) {
				resp.Message = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(physicalDB, ref.Table))
				return
			}
			if _, key, ok := nameresolve.LookupStorageIntegrity(ref.DB, ref.Table, sel.Dynamic); ok {
				resp.Message = nameresolve.StorageIntegrityWriteRejectMessage(key)
				return
			}
		}
	}
	// A table the statement reads in a position no SI handler classifies
	// (a column DEFAULT / MATERIALIZED subquery, spec 2026-09-26 R2) is still
	// a proven table reference: spec 2026-09-26 §5 puts the SI message (T3)
	// ahead of the ordinary-stage refusal.
	if kind, kerr := engine.NodeKind(ast); kerr != nil || kind == engine.NodeCommand {
		return
	}
	tables, terr := engine.CollectSelectTables(ast)
	if terr != nil {
		return
	}
	for _, tt := range tables {
		if tt.DB == "" {
			continue
		}
		if nameresolve.IsStorageIntegrityPhysicalDatabase(tt.DB, sel.Dynamic) {
			resp.Message = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(tt.DB, tt.Table))
			return
		}
	}
}
