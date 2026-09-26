package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// PreflightTableReferences applies the position-independent halves of the
// table-reference policy (spec 2026-09-26 §5) before any handler runs, in
// precedence order: identifier parameters (T2), then protected databases
// (T3, Task 4), then — only while the storage-integrity surface is inactive
// (controller ruling 1, Task 7) — the table-function/table-engine/
// table-setting allowlists (T5). While the surface is active,
// rewriteSelectCore and preflightStorageIntegrityWrite run the very same T5
// check themselves, immediately after their own SI namespace policy finds
// nothing to reject, so none of the SI-owned messages this corpus pins
// (merge('hg_safe', …), merge('db1', …) under contract V2, …) ever move.
// Static mode and requests without dynamic args are untouched.
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
	dbs, err := engine.CollectDatabaseReferences(e, ast, sql)
	if err != nil {
		return nil, false, err
	}
	if ctx := sel.Dynamic.GetUpstreamLogicalDatabaseInContext(); ctx != "" {
		dbs = append(dbs, ctx)
	}
	// A database qualifier discovered only via a position no SI handler
	// classifies as a table reference (a joinGet/dictGet/hasColumnInTable
	// string-lookup argument, or a parenthesized single-element IN-list) must
	// reject even while the storage-integrity surface is active -- unlike an
	// ordinary table position, which defers to those handlers below.
	blindDBs, err := engine.CollectSIHandlerBlindDatabaseReferences(ast)
	if err != nil {
		return nil, false, err
	}
	unconditional := make(map[string]bool, len(blindDBs))
	for _, db := range blindDBs {
		unconditional[db] = true
	}
	for _, db := range dbs {
		if !nameresolve.ProtectedDatabase(db, sel.Dynamic) {
			continue
		}
		if nameresolve.IsStorageIntegrityPhysicalDatabase(db, sel.Dynamic) && !unconditional[db] {
			// The SI handlers own this name while the surface is active and
			// their messages are pinned by the existing corpus; let them fire.
			continue
		}
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		recordAccessedDatabase(resp, db, sel.Dynamic)
		rejectInvalid(resp, nameresolve.ProtectedDatabaseRejectMessage(db))
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	if !nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		if rejected, cerr := rejectDisallowedCarriers(e, ast, resp); cerr != nil {
			return nil, false, cerr
		} else if rejected {
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		}
	}
	return nil, false, nil
}

// rejectDisallowedCarriers applies the T5 table-function / table-engine /
// table-setting allowlists (spec 2026-09-26 §5) to ast, setting resp's
// Code/Message when it finds a disallowed one. Shared by
// PreflightTableReferences (run only while the storage-integrity surface is
// inactive) and rewriteSelectCore / preflightStorageIntegrityWrite (run only
// while it is active, immediately after their own SI namespace policy finds
// nothing to reject) — controller ruling 1. A CREATE TABLE's engine name is
// checked only when CreateTableStorage found one: an ALTER TABLE … MODIFY
// SETTING has no engine of its own, and CreateTableStorage reports that shape
// with an empty engineName rather than a bare (refused) engine name.
func rejectDisallowedCarriers(e engine.Engine, ast engine.AST, resp *pb.RewriteSQLResponse) (bool, error) {
	names, err := engine.CollectSourceFunctionNames(ast)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		switch engine.ClassifyTableFunction(name) {
		case engine.TableFunctionRefused:
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableFunctionRefusedMessage(name)
			return true, nil
		case engine.TableFunctionUnknown:
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableFunctionUnknownMessage(name)
			return true, nil
		}
	}
	name, argc, settings, ok, err := engine.CreateTableStorage(e, ast)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if name != "" && !engine.TableEngineAllowed(name, argc) {
		resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableEngineRefusedMessage(name)
		return true, nil
	}
	for _, s := range settings {
		if engine.RefusedTableSetting(s) {
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableSettingRefusedMessage(s)
			return true, nil
		}
	}
	return false, nil
}
