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
	// T2: an Identifier parameter in any database or table position — for an
	// opaque command node, anywhere in its text.
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
	// T7 before T3 (spec 2026-09-26 §5, R8): a command node of a class no
	// handler models is refused before its names are examined. With the SI
	// surface inactive the refusal is "statement is not supported" here; with
	// it active the SI pipeline owns it (an SI object is named by the SI
	// write preflight or the final-response annotation, everything else gets
	// the SI catch-all), so the rest of this preflight is skipped.
	if kind, kerr := engine.NodeKind(ast); kerr != nil {
		return nil, false, kerr
	} else if kind == engine.NodeCommand && !commandClassModelled(e, ast, sql, sel) {
		if nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
			return nil, false, nil
		}
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		if targets, _, rerr := engine.RawTableRefs(e, ast); rerr == nil {
			for _, tt := range targets {
				recordAccessedWrite(resp, tt, sel)
			}
		}
		rejectUnsupported(resp, engine.UnsupportedStatementMessage)
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
	// call, not a plain [db.]name (Task 7 fix round 1 finding 3), plus T6
	// mechanism (b) (Task 7 fix round 2): a `command` node's raw text is
	// tokenized exactly once and used for BOTH concerns. ParseObjectTarget's
	// own name-run extraction silently drops everything from "(" onward, so
	// e.g. `DESCRIBE TABLE mysql('h', ...)` reported Table="mysql" and fell
	// through RewriteDescribe's existing pass-through-unchanged branch —
	// neither the T5 allowlist nor the protected-database check ever saw it.
	// The verb/target detection reads the tokenizer's first token (comments
	// are stripped before the token stream, never a separate token) rather
	// than string-matching the raw text, so a leading comment can no longer
	// defeat it (fix round 2 finding 3). Every lookup-family call the same
	// tokenize pass finds (mechanism (b): an ALTER TABLE … UPDATE/DELETE
	// mutation, including a multi-command tail, has no structured "function"
	// node at all — it is either the command's whole text, as here, or an
	// embedded Raw action, handled separately below) is refused with the
	// generic "does not resolve" message; hasColumnInTable is refused here
	// too, since a command node's raw text never has a SELECT-body rewrite
	// pipeline of its own. A tokenizer error fails CLOSED regardless of SI
	// state (mechanism (b)'s own words), unlike an ordinary internal-error
	// propagation elsewhere in this file.
	//
	// Unconditional (both SI states): unlike SELECT/write dispatch, no later
	// handler defers either concern for an active SI surface, so there is no
	// other opportunity to catch them.
	if kind, kerr := engine.NodeKind(ast); kerr != nil {
		return nil, false, kerr
	} else if kind == engine.NodeCommand {
		text, cerr := engine.CommandSQL(ast)
		if cerr != nil {
			return nil, false, cerr
		}
		findings, cok := engine.CollectCommandTextFindings(e, text)
		if !cok {
			resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
			rejectUnsupported(resp, engine.UnsupportedStatementMessage)
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		}
		for _, call := range findings.LookupCalls {
			resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
			rejectInvalid(resp, stringLookupUnresolvedMessage(call))
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		}
		if findings.TargetIsFunctionCall {
			switch engine.ClassifyTableFunction(findings.TargetFunctionName) {
			case engine.TableFunctionRefused:
				resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
				rejectUnsupported(resp, engine.TableFunctionRefusedMessage(findings.TargetFunctionName))
				resp.SqlAfterRewrite = sql
				return resp, true, nil
			case engine.TableFunctionUnknown:
				resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
				rejectUnsupported(resp, engine.TableFunctionUnknownMessage(findings.TargetFunctionName))
				resp.SqlAfterRewrite = sql
				return resp, true, nil
			}
			for _, db := range findings.TargetArgDatabases {
				if nameresolve.ProtectedDatabase(db, sel.Dynamic) {
					resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
					recordAccessedDatabase(resp, db, sel.Dynamic)
					rejectInvalid(resp, nameresolve.ProtectedDatabaseRejectMessage(db))
					resp.SqlAfterRewrite = sql
					return resp, true, nil
				}
			}
		}
	}
	// T6 mechanism (b), continued: a {"Raw":{"sql":…}} action embedded
	// anywhere in the structured AST (e.g. an ALTER TABLE … MODIFY COLUMN …
	// DEFAULT … action) is likewise opaque text with no structured
	// "function" node — scanned the same way, independent of the command
	// node case above (a Raw action is never itself a command node).
	rawActionLookups, raOK := engine.CollectRawActionStringLookupCalls(e, ast)
	if !raOK {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, engine.UnsupportedStatementMessage)
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	for _, call := range rawActionLookups {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectInvalid(resp, stringLookupUnresolvedMessage(call))
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	// T6 mechanism (a) (spec 2026-09-26, controller ruling 2 / Task 7 fix
	// round 2): one generic, deterministic walk over the whole statement's
	// structured AST finds every string-lookup call reachable through an
	// actual "function" node — SELECT bodies, INSERT/CTAS/VIEW embedded
	// bodies, structured UPDATE/DELETE, IN subqueries, CREATE TABLE column
	// DEFAULT/MATERIALIZED/ALIAS/EPHEMERAL expressions, PARTITION BY/TTL/
	// CONSTRAINT CHECK expressions, and everywhere else a "function" node can
	// appear — with no per-position collector needed (this replaces fix
	// round 1's now-deleted CollectAlterMutationStringLookups/
	// CollectColumnDefinitionStringLookups). joinGet/dictGet-family calls are
	// refused wherever found (a strict superset of what rewriteSelectCore's
	// own, still-present, now largely redundant but harmless joinGet/dictGet
	// handling reaches). hasColumnInTable is refused everywhere EXCEPT when
	// StringLookupCalls marks it InSelectBody — reachable from a select/
	// union/intersect/except subtree — where rewriteSelectCore's own rewrite
	// pipeline owns it instead.
	astLookups, err := engine.StringLookupCalls(ast)
	if err != nil {
		return nil, false, err
	}
	for _, call := range astLookups {
		if isHasColumnInTable(call.Function) && call.InSelectBody {
			continue
		}
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectInvalid(resp, stringLookupUnresolvedMessage(call))
		resp.SqlAfterRewrite = sql
		return resp, true, nil
	}
	// SQL-bearing settings (R5) and reads in positions no rewrite pipeline
	// reaches (R2). While the SI surface is active the SI handlers run these
	// same checks after their own namespace policy, so an SI-owned message
	// keeps precedence.
	if !nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		if rejected, rerr := rejectUngovernedReads(e, ast, resp); rerr != nil {
			return nil, false, rerr
		} else if rejected {
			resp.SqlAfterRewrite = sql
			return resp, true, nil
		}
	}
	return nil, false, nil
}

// rejectUngovernedReads refuses, with UnsupportedStatement "statement is not
// supported", a statement that carries a read the rewriter cannot rewrite and
// report (spec 2026-09-26 R2): a table, IN-table operand, table function or
// parameter in a structured UPDATE / DELETE, INSERT VALUES, column,
// constraint, storage-property or ALTER-action expression, or ungoverned
// content in opaque ALTER text (a subquery, a table-operand IN, a cross-table
// partition action, ALTER … MODIFY QUERY). Runs after the T2 / T3 / T5 / T6
// checks, so a parameter, protected-database, allowlist or lookup message
// always wins. Shared, like rejectDisallowedCarriers, by
// PreflightTableReferences (SI surface inactive) and the SI-active handler
// paths.
func rejectUngovernedReads(e engine.Engine, ast engine.AST, resp *pb.RewriteSQLResponse) (bool, error) {
	if rejected, err := rejectSQLBearingSettings(e, ast, resp); err != nil || rejected {
		return rejected, err
	}
	reads, err := engine.ExpressionPositionHasReads(ast)
	if err != nil {
		return false, err
	}
	if reads {
		resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.UnsupportedStatementMessage
		return true, nil
	}
	texts, err := engine.OpaqueAlterTexts(ast)
	if err != nil {
		return false, err
	}
	for _, text := range texts {
		if engine.OpaqueTextIsUngoverned(e, text) {
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.UnsupportedStatementMessage
			return true, nil
		}
	}
	return false, nil
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
	if name != "" {
		switch engine.ClassifyTableEngine(name, argc) {
		case engine.TableEngineRefused:
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableEngineRefusedMessage(name)
			return true, nil
		case engine.TableEngineUnknown:
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableEngineUnknownMessage(name)
			return true, nil
		}
	}
	for _, s := range settings {
		if engine.RefusedTableSetting(s) {
			resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, engine.TableSettingRefusedMessage(s)
			return true, nil
		}
	}
	return false, nil
}

// rejectSQLBearingSettings applies spec 2026-09-26 R5 to every query-level
// SETTINGS clause of a statement — structured (a SELECT / set operation /
// INSERT, including an embedded body) or opaque (a command node's text, a Raw
// ALTER action): a SQL-bearing setting name (engine.SQLBearingSetting) is
// refused with the T5 table-setting message, and any value other than a
// numeric literal, a string literal or a bare identifier / keyword with
// "statement is not supported". A session SET statement is checked by the
// SET carve-out instead (CheckSessionSet).
func rejectSQLBearingSettings(e engine.Engine, ast engine.AST, resp *pb.RewriteSQLResponse) (bool, error) {
	assignments, err := engine.QuerySettings(ast)
	if err != nil {
		return false, err
	}
	var texts []string
	if kind, kerr := engine.NodeKind(ast); kerr != nil {
		return false, kerr
	} else if kind == engine.NodeCommand {
		text, cerr := engine.CommandSQL(ast)
		if cerr != nil {
			return false, cerr
		}
		if _, isSet, _ := engine.SessionSettingAssignments(e, text); !isSet {
			texts = append(texts, text)
		}
	} else {
		opaque, oerr := engine.OpaqueAlterTexts(ast)
		if oerr != nil {
			return false, oerr
		}
		texts = append(texts, opaque...)
	}
	for _, text := range texts {
		raw, ok := engine.RawSettingsClauses(e, text)
		assignments = append(assignments, raw...)
		if !ok {
			assignments = append(assignments, engine.SettingAssignment{})
		}
	}
	if code, msg, bad := settingsVerdict(assignments); bad {
		resp.Code, resp.Message = code, msg
		return true, nil
	}
	return false, nil
}

// settingsVerdict returns the first refusal among assignments, in order: a
// SQL-bearing name wins over a non-plain value of the same assignment.
func settingsVerdict(assignments []engine.SettingAssignment) (pb.RewriteCode, string, bool) {
	for _, a := range assignments {
		if engine.SQLBearingSetting(a.Name) {
			return pb.RewriteCode_UnsupportedStatement, engine.TableSettingRefusedMessage(a.Name), true
		}
		if !a.PlainValue {
			return pb.RewriteCode_UnsupportedStatement, engine.UnsupportedStatementMessage, true
		}
	}
	return pb.RewriteCode_Success, "", false
}

// CheckSessionSet applies the SET carve-out rule (spec 2026-09-26 T7, R5) to
// a command node's text. isSet=false means the text is not a session
// settings assignment (not SET, or SET ROLE / SET DEFAULT ROLE) and the
// carve-out does not apply. Otherwise refused reports a SET the carve-out
// does not admit, with its code and message: a SQL-bearing setting name, a
// value that is not a numeric literal, string literal or bare identifier /
// keyword, or an assignment list that does not parse.
func CheckSessionSet(e engine.Engine, text string) (isSet, refused bool, code pb.RewriteCode, msg string) {
	assignments, isSet, wellFormed := engine.SessionSettingAssignments(e, text)
	if !isSet {
		return false, false, pb.RewriteCode_Success, ""
	}
	if code, msg, bad := settingsVerdict(assignments); bad {
		return true, true, code, msg
	}
	if !wellFormed {
		return true, true, pb.RewriteCode_UnsupportedStatement, engine.UnsupportedStatementMessage
	}
	return true, false, pb.RewriteCode_Success, ""
}

// commandClassModelled reports whether a `command` node belongs to a class a
// handler models (spec 2026-09-26 T7, R8): EXISTS / SHOW CREATE / DESCRIBE,
// USE / SHOW …, RENAME / EXCHANGE TABLE, ALTER TABLE … UPDATE, GRANT / REVOKE
// / ATTACH GRANT, and — only while the SI surface is inactive — a session
// SET. Every other class (DETACH, ATTACH, OPTIMIZE, KILL, EXPLAIN, SYSTEM,
// CHECK, CREATE USER, …) is unmodelled. A text the tokenizer cannot read is
// unmodelled too (fail closed).
func commandClassModelled(e engine.Engine, ast engine.AST, sql string, sel nameresolve.Selection) bool {
	target, err := engine.ParseObjectTarget(e, sql)
	if err != nil {
		return false
	}
	if target.Verb != engine.VerbNone {
		return true
	}
	if info, err := engine.ParseDBLevel(e, sql); err != nil {
		return false
	} else if info.Kind != engine.DBNone {
		return true
	}
	if info, err := engine.InspectWrite(ast); err != nil {
		return false
	} else if info.Sub == engine.CmdRename || info.Sub == engine.CmdExchange || info.Sub == engine.CmdAlterUpdate {
		return true
	}
	if gp, err := engine.ParseGrant(e, sql); err != nil {
		return false
	} else if gp.IsGrantVerb {
		return true
	}
	if nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
		return false
	}
	_, isSet, _ := engine.SessionSettingAssignments(e, sql)
	return isSet
}
