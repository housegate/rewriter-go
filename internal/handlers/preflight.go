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
	// T5 + T3 for a DESCRIBE / EXISTS / SHOW CREATE target that is a function
	// call, not a plain [db.]name (Task 7 fix round 1 finding 3):
	// ParseObjectTarget's tokenizer-based name-run extraction silently drops
	// everything from "(" onward, so e.g. `DESCRIBE TABLE mysql('h', ...)`
	// reported Table="mysql" and fell through RewriteDescribe's existing
	// pass-through-unchanged branch — neither the T5 allowlist nor the
	// protected-database check ever saw it. Unconditional (both SI states):
	// unlike SELECT/write dispatch, no later handler defers this check for
	// an active SI surface, so there is no other opportunity to catch it.
	if _, fnName, argDBs, fok, ferr := engine.ParseObjectTargetFunctionCall(e, sql); ferr != nil {
		return nil, false, ferr
	} else if fok {
		switch engine.ClassifyTableFunction(fnName) {
		case engine.TableFunctionRefused:
			resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
			rejectUnsupported(resp, engine.TableFunctionRefusedMessage(fnName))
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		case engine.TableFunctionUnknown:
			resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
			rejectUnsupported(resp, engine.TableFunctionUnknownMessage(fnName))
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		}
		for _, db := range argDBs {
			if nameresolve.ProtectedDatabase(db, sel.Dynamic) {
				resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
				recordAccessedDatabase(resp, db, sel.Dynamic)
				rejectInvalid(resp, nameresolve.ProtectedDatabaseRejectMessage(db))
				resp.SqlAfterRewrite = sql
				return resp, true, nil
			}
		}
	}
	// T6 (spec 2026-09-26, controller ruling 2 / Task 7 fix round 1 finding
	// 2): joinGet/dictGet-family calls are refused statement-wide, wherever
	// they appear — SELECT bodies, INSERT/CTAS/VIEW embedded bodies,
	// structured UPDATE/DELETE, CREATE TABLE column DEFAULT/MATERIALIZED/
	// ALIAS/EPHEMERAL expressions, and the opaque ALTER TABLE …
	// UPDATE/DELETE mutation shape — run after the protected-database step
	// so a protected name still reports its own message first (e.g.
	// joinGet('phys.`x`', …)). This is a strict superset of what
	// rewriteSelectCore's own (still-present, now largely redundant but
	// harmless) joinGet/dictGet handling reaches, since StringLookupCalls
	// walks the whole original ast structurally, embedded SELECT bodies
	// included.
	//
	// hasColumnInTable is rewritten only inside a SELECT body
	// (rewriteSelectCore owns that, reached through StringLookupCalls too but
	// filtered out below); everywhere else — a CREATE TABLE column
	// expression, or an ALTER mutation — it is refused with the same
	// message, since neither position has a rewrite pipeline of its own.
	generalLookups, err := engine.StringLookupCalls(ast)
	if err != nil {
		return nil, false, err
	}
	for _, call := range generalLookups {
		if isHasColumnInTable(call.Function) {
			continue
		}
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectInvalid(resp, stringLookupUnresolvedMessage(call))
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	alterLookups, err := engine.CollectAlterMutationStringLookups(e, ast, sql)
	if err != nil {
		return nil, false, err
	}
	columnLookups, err := engine.CollectColumnDefinitionStringLookups(ast)
	if err != nil {
		return nil, false, err
	}
	for _, call := range append(alterLookups, columnLookups...) {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectInvalid(resp, stringLookupUnresolvedMessage(call))
		resp.SqlAfterRewrite = sql
		return resp, true, nil
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
