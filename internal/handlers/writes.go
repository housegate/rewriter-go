package handlers

import (
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteWrite ports handleWriteQuery. Returns (resp, handled, err):
//
//	handled=true  → resp is final (Success or a reject code).
//	handled=false → not a write this phase handles; caller falls through to SELECT.
//	err != nil    → unexpected/internal engine failure → native fail-opens.
//
// sql (original source) is threaded for the INSERT payload splice (Task 9) and
// tier-C raw splice (Task 10); structured kinds ignore it.
func RewriteWrite(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	info, err := engine.InspectWrite(ast)
	if err != nil {
		return nil, false, err
	}
	sel := nameresolve.FindActive(opts)

	switch info.Kind {
	case engine.NodeCreateTable:
		return dispatchCreateTable(e, ast, info, sel)
	case engine.NodeDropTable, engine.NodeDropView, engine.NodeTruncate:
		return dispatchDropLike(e, ast, info, sel)
	case engine.NodeAlterTable:
		return dispatchAlter(e, ast, info, sel)
	case engine.NodeUpdate:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_UPDATE)
	case engine.NodeDelete:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_DELETE)
	// case engine.NodeCreateView:           → Task 8
	// case engine.NodeInsert:               → Task 9
	// case engine.NodeCommand:              → Task 10
	// case engine.NodeCreateDB, NodeDropDB: → Task 10
	default:
		return nil, false, nil // not handled this task → caller falls through to SELECT
	}
}

func newWriteResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{
		Code: pb.RewriteCode_Success, Message: "success",
		StatementType: stmt, TableRewrites: map[string]string{},
	}
}

// rejectUnsupported/rejectInvalid set the reject code+message, but ONLY when the
// response is still Success — so the FIRST reject in a multi-slot statement wins
// and a later slot can't clobber it.
func rejectUnsupported(resp *pb.RewriteSQLResponse, msg string) {
	if resp.Code == pb.RewriteCode_Success {
		resp.Code = pb.RewriteCode_UnsupportedStatement
		resp.Message = msg
	}
}

func rejectInvalid(resp *pb.RewriteSQLResponse, msg string) {
	if resp.Code == pb.RewriteCode_Success {
		resp.Code = pb.RewriteCode_InvalidRewriteRequest
		resp.Message = msg
	}
}

// decideWriteTarget is the STRICT per-target policy (C++ rewriteOneTarget): records
// the access + table_rewrites, and on a remote/invalid hit populates resp with the
// reject code and returns ok=false so the caller short-circuits.
func decideWriteTarget(tt engine.TableTarget, kind string, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (engine.TableDecision, bool) {
	recordAccessedWrite(resp, tt, sel) // record BEFORE any reject (C++ writes.cc:118)
	o := nameresolve.Resolve(tt.DB, tt.Table, sel)
	switch o.Status {
	case nameresolve.StatusRewrite:
		recordRewrite(resp.TableRewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRename, NewDB: o.PhysicalDB, NewTable: o.NewTable}, true
	case nameresolve.StatusRemote, nameresolve.StatusRemoteUnsupported:
		rejectUnsupported(resp, kind+" target maps to a remote upstream; remote() can only appear as a SELECT-side table function")
		return engine.TableDecision{}, false
	case nameresolve.StatusInvalid:
		rejectInvalid(resp, o.RejectReason)
		return engine.TableDecision{}, false
	default: // StatusPassthrough
		return engine.TableDecision{Action: engine.ActionSkip}, true
	}
}

// recordAccessedWrite appends one AccessedTable for a write target (skip Table=="").
// Appends in encounter order (no dedup/sort — writes have 1-2 targets). Mirrors C++
// recordAccessedTable; reuses nameresolve.ResolveAccessed.
func recordAccessedWrite(resp *pb.RewriteSQLResponse, tt engine.TableTarget, sel nameresolve.Selection) {
	if tt.Table == "" {
		return
	}
	a := nameresolve.ResolveAccessed(tt.DB, tt.Table, sel)
	resp.OriginalAccessedTables = append(resp.OriginalAccessedTables, &pb.AccessedTable{
		OriginalDatabase: tt.DB, OriginalTable: tt.Table,
		LogicalDatabase: a.LogicalDB, PhysicalDatabase: a.PhysicalDB, IsRemote: a.IsRemote,
	})
}

// finishStructured strict-decides each slot in document order, rewrites, regenerates.
// Shared by the structured kinds (create_table name+clone_source, drop, alter,
// update, delete).
//
// It decides slots HERE (not inside the RewriteWriteTargets callback) so it can
// SHORT-CIRCUIT on the first reject — exactly like C++ handleWriteQuery, which
// returns Rejected the instant a rewriteOneTarget fails, never recording a later
// slot's access/table_rewrites. RewriteWriteTargets has no early-exit, so feeding
// it a decide callback that records side-effects would over-record on a multi-slot
// reject (e.g. CREATE TABLE db.t AS db2.src where db.t rejects must yield 1
// accessed table + 0 rewrites, not 2 + 1). Surviving renames are keyed by
// WriteRole, which is unique per statement (create/clone_source/view_to/drop/...).
func finishStructured(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (*pb.RewriteSQLResponse, bool, error) {
	decisions := make(map[engine.WriteRole]engine.TableDecision, len(info.Slots))
	for _, s := range info.Slots {
		d, ok := decideWriteTarget(s.Target, info.Kind, sel, resp)
		if !ok {
			return resp, true, nil // reject populated; stop here (C++ short-circuit)
		}
		if d.Action == engine.ActionRename {
			decisions[s.Role] = d
		}
	}
	rewritten, err := engine.RewriteWriteTargets(ast, func(s engine.WriteSlot) engine.TableDecision {
		if d, ok := decisions[s.Role]; ok {
			return d
		}
		return engine.TableDecision{Action: engine.ActionSkip}
	})
	if err != nil {
		return nil, false, err
	}
	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = sql
	return resp, true, nil
}

func dispatchSingle(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, stmt pb.StatementType) (*pb.RewriteSQLResponse, bool, error) {
	return finishStructured(e, ast, info, sel, newWriteResp(stmt))
}

func dispatchCreateTable(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_CREATE_TABLE)
	if info.AsTableFunction {
		rejectUnsupported(resp, "CREATE TABLE AS table_function(...) is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

func dispatchDropLike(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_DROP_TABLE
	switch info.Kind {
	case engine.NodeDropView:
		stmt = pb.StatementType_STATEMENT_TYPE_DROP_VIEW
	case engine.NodeTruncate:
		stmt = pb.StatementType_STATEMENT_TYPE_TRUNCATE_TABLE
	}
	resp := newWriteResp(stmt)
	if info.Multi {
		rejectUnsupported(resp, "multi-table DROP/TRUNCATE is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

func dispatchAlter(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_ALTER_TABLE)
	if info.CrossTable {
		rejectUnsupported(resp, "ALTER TABLE with cross-table reference (ATTACH/REPLACE/FETCH PARTITION FROM, MOVE PARTITION TO TABLE) is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}
