package handlers

import (
	"strings"

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
		return dispatchDropLike(e, ast, sql, info, sel)
	case engine.NodeAlterTable:
		return dispatchAlter(e, ast, info, sel)
	case engine.NodeUpdate:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_UPDATE)
	case engine.NodeDelete:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_DELETE)
	case engine.NodeCreateView:
		return dispatchView(e, ast, info, opts, sel)
	case engine.NodeInsert:
		return dispatchInsert(e, ast, sql, info, sel)
	case engine.NodeCommand:
		return dispatchCommand(e, ast, sql, info, sel)
	case engine.NodeRaw:
		// DDL polyglot couldn't structure (CREATE LIVE/WINDOW VIEW, ALTER
		// DATABASE, …). C++ rejects each via its structured guard; we reject by
		// inspecting the raw SQL (Task 13).
		return dispatchRawReject(e, ast)
	case engine.NodeCopy:
		// COPY … FROM/TO — C++ QueryKind::Copy reject (writes.cc).
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, "COPY is not supported")
		return resp, true, nil
	case engine.NodeCreateDB, engine.NodeDropDB:
		return dispatchDatabaseOutOfPhase(info)
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

// applyStructuredSlots strict-decides each slot in document order (SHORT-CIRCUIT on
// the first reject, recording access/rewrites incrementally — see the finishStructured
// note below), then applies the surviving renames to the AST. Returns
// (rewrittenAST, ok): when ok==false the reject is already populated in resp and the
// returned AST is unset. err is reserved for an unexpected engine failure.
//
// It decides slots HERE (not inside the RewriteWriteTargets callback) so it can
// SHORT-CIRCUIT on the first reject — exactly like C++ handleWriteQuery, which
// returns Rejected the instant a rewriteOneTarget fails, never recording a later
// slot's access/table_rewrites. RewriteWriteTargets has no early-exit, so feeding
// it a decide callback that records side-effects would over-record on a multi-slot
// reject (e.g. CREATE TABLE db.t AS db2.src where db.t rejects must yield 1
// accessed table + 0 rewrites, not 2 + 1; likewise a rejecting view name must not
// record its MV TO target). Surviving renames are keyed by WriteRole, which is
// unique per statement (create/clone_source/view_to/drop/...).
func applyStructuredSlots(ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (engine.AST, bool, error) {
	decisions := make(map[engine.WriteRole]engine.TableDecision, len(info.Slots))
	for _, s := range info.Slots {
		d, ok := decideWriteTarget(s.Target, info.Kind, sel, resp)
		if !ok {
			return nil, false, nil // reject populated; stop here (C++ short-circuit)
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
	return rewritten, err == nil, err
}

// finishStructured strict-decides each slot, rewrites, regenerates. Shared by the
// structured kinds (create_table name+clone_source, drop, alter, update, delete).
// The slot decide+apply (with its short-circuit) lives in applyStructuredSlots so
// the view handler can reuse the identical logic for its name+TO slots.
func finishStructured(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (*pb.RewriteSQLResponse, bool, error) {
	rewritten, ok, err := applyStructuredSlots(ast, info, sel, resp)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil // reject populated by applyStructuredSlots
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
	if info.IsDictionary {
		// CREATE DICTIONARY folds into a create_table node (table_modifier ==
		// "DICTIONARY"); C++ rejects via ASTCreateQuery::is_dictionary (writes.cc:270).
		rejectUnsupported(resp, "CREATE DICTIONARY is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

func dispatchDropLike(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_DROP_TABLE
	switch info.Kind {
	case engine.NodeDropView:
		stmt = pb.StatementType_STATEMENT_TYPE_DROP_VIEW
	case engine.NodeTruncate:
		stmt = pb.StatementType_STATEMENT_TYPE_TRUNCATE_TABLE
	}
	resp := newWriteResp(stmt)
	// TRUNCATE non-table reject (Task 13). Polyglot flattens TRUNCATE VIEW / ALL
	// TABLES FROM / DATABASE into a `truncate` node that LOSES the real object
	// (VIEW/ALL mis-parse as the table name, target stays "Table"; only DATABASE
	// flips target to "Database"). So we reject on EITHER signal: a non-"Table"
	// target (covers DATABASE) OR a raw-SQL object keyword that is not TABLE
	// (covers VIEW/ALL/DICTIONARY). Mirrors C++ writes.cc:387-412 which rejects
	// DROP/TRUNCATE on DICTIONARY, TRUNCATE on VIEW, and TRUNCATE DATABASE / ALL.
	if info.Kind == engine.NodeTruncate {
		if (info.TruncateTarget != "" && info.TruncateTarget != "Table") || !truncateObjectIsTable(sql) {
			rejectUnsupported(resp, "TRUNCATE VIEW / DATABASE / ALL TABLES FROM is not supported; only TRUNCATE TABLE is allowed")
			return resp, true, nil
		}
	}
	if info.Multi {
		rejectUnsupported(resp, "multi-table DROP/TRUNCATE is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

// truncateObjectIsTable reports whether the object keyword in a raw TRUNCATE
// statement is TABLE (the only accepted form). It keyword-scans the SQL: find the
// "TRUNCATE" token, skip an optional "TEMPORARY", and the next token is the object
// keyword — true ONLY when it equals "TABLE". This is needed because polyglot's
// `truncate` AST mis-parses VIEW/ALL as the table name (the structured `target`
// stays "Table"), so the raw text is the only faithful discriminator for those.
// A bare `TRUNCATE db.t` (no object keyword — ClickHouse allows implicit TABLE) is
// NOT emitted by polyglot for this phase; defensively, an absent/unknown next
// token is treated as not-TABLE so the caller rejects rather than corrupts.
func truncateObjectIsTable(sql string) bool {
	fields := strings.Fields(strings.ToUpper(sql))
	for i, f := range fields {
		if f != "TRUNCATE" {
			continue
		}
		j := i + 1
		if j < len(fields) && fields[j] == "TEMPORARY" {
			j++
		}
		return j < len(fields) && fields[j] == "TABLE"
	}
	return false
}

func dispatchAlter(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_ALTER_TABLE)
	if info.CrossTable {
		rejectUnsupported(resp, "ALTER TABLE with cross-table reference (ATTACH/REPLACE/FETCH PARTITION FROM, MOVE PARTITION TO TABLE) is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

// dispatchView ports C++ handleViewCreate. It rewrites three things, in order:
//  1. the view name (RoleCreate slot);
//  2. a materialized view's TO target (RoleViewTo slot), when present;
//  3. the view body SELECT, via the full Phase-1 pipeline (rewriteSelectCore),
//     merging the body's table_rewrites/accessed/failed-CTE bookkeeping into resp.
//
// Steps 1+2 go through applyStructuredSlots so a rejecting view name SHORT-CIRCUITS
// before the TO target's access is recorded and before the body is touched —
// matching C++, which returns Rejected the instant the name rewrite fails
// (writes.cc:205-229). LIVE/WINDOW views are not modeled here (the C++ caller
// rejects them upstream; Polyglot surfaces no such body for this phase).
func dispatchView(e engine.Engine, ast engine.AST, info engine.WriteInfo, opts []*pb.RewriteOption, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_CREATE_VIEW
	if info.Materialized {
		stmt = pb.StatementType_STATEMENT_TYPE_CREATE_MATERIALIZED_VIEW
	}
	resp := newWriteResp(stmt)

	// 1+2. View name + MV TO target — strict, short-circuiting (C++ writes.cc:205-229).
	rewritten, ok, err := applyStructuredSlots(ast, info, sel, resp)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil // reject populated; TO target/body NOT processed
	}

	// 3. Body SELECT — full Phase-1 pipeline; merge its bookkeeping into resp
	//    (C++ rewriteEmbeddedViewBody, writes.cc:231-247).
	if info.HasViewBody {
		body, has, err := engine.ExtractViewBody(rewritten)
		if err != nil {
			return nil, false, err
		}
		if has {
			newBody, bodyResp, err := rewriteSelectCore(e, body, opts)
			if err != nil {
				return nil, false, err
			}
			mergeViewBody(resp, bodyResp)
			if rewritten, err = engine.SetViewBody(rewritten, newBody); err != nil {
				return nil, false, err
			}
		}
	}

	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = sql
	return resp, true, nil
}

// dispatchInsert ports C++ handleWriteQuery's INSERT block (writes.cc:503-537).
// It rejects the two unrewriteable forms first — INSERT INTO FUNCTION(...) and a
// missing target table — then rewrites the SINGLE insert target via the shared
// applyStructuredSlots (which records access + table_rewrites and short-circuits
// on a remote/invalid reject). The embedded SELECT of an INSERT…SELECT is NOT
// walked: only insert.table is a slot, so its source stays as written (C++ only
// rewrites insert_query->table). GenerateInsert regenerates the prelude and, for
// a FORMAT data clause, splices the original inline payload back verbatim (a plain
// VALUES / INSERT…SELECT has no payload tail and just round-trips through Generate).
func dispatchInsert(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_INSERT)
	if info.AsTableFunction {
		rejectUnsupported(resp, "INSERT INTO FUNCTION(...) is not supported")
		return resp, true, nil
	}
	if info.MissingTable {
		rejectUnsupported(resp, "INSERT target table is missing")
		return resp, true, nil
	}
	rewritten, ok, err := applyStructuredSlots(ast, info, sel, resp)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil // reject populated by applyStructuredSlots
	}
	out, err := engine.GenerateInsert(e, sql, rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = out
	return resp, true, nil
}

// mergeViewBody folds the body SELECT's bookkeeping into the view response: the
// body's table_rewrites are added, and its accessed/failed-CTE lists are appended
// AFTER the view's own slots (so accessed order is name/TO first, then body —
// matching the write-slots-then-body order C++ produces).
func mergeViewBody(dst, body *pb.RewriteSQLResponse) {
	for k, v := range body.GetTableRewrites() {
		dst.TableRewrites[k] = v
	}
	dst.OriginalAccessedTables = append(dst.OriginalAccessedTables, body.GetOriginalAccessedTables()...)
	dst.FailedCteAliases = append(dst.FailedCteAliases, body.GetFailedCteAliases()...)
}

// dispatchCommand routes a tier-C `command` node by its sub-classification
// (CommandSub, set by InspectWrite). The table-bearing forms (RENAME/EXCHANGE/
// ALTER…UPDATE) go to the raw byte-span splice path; the bare rejects
// (OPTIMIZE/UNDROP/MOVE/BACKUP/RESTORE/KILL/…) reject as UnsupportedStatement
// (C++ writes.cc:590-621); everything else (USE/SHOW/GRANT/REVOKE/EXISTS) is not a
// write this phase handles → handled=false, caller falls through to SELECT.
func dispatchCommand(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	switch info.Sub {
	case engine.CmdRename, engine.CmdExchange, engine.CmdAlterUpdate:
		return dispatchRawTables(e, ast, sql, info, sel)
	case engine.CmdBareReject:
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, "statement is not supported")
		return resp, true, nil
	default: // CmdNone: USE/SHOW/GRANT/REVOKE/EXISTS — not a write this phase handles
		return nil, false, nil
	}
}

// dispatchRawTables ports the C++ RENAME/EXCHANGE block (writes.cc:563-587) plus
// ALTER…UPDATE, all of which parse to an opaque command node with NO structured
// table refs. It reads the table refs out of the raw SQL (RawTableRefs), strict-
// decides EACH in document order — recording accessed + table_rewrites and short-
// circuiting on the first reject (mirrors rewriteRenameSide→rewriteOneTarget) — and
// then splices the surviving renames back into the original byte spans. Splice map
// values are pre-QUOTED via QuoteQualified so a dotted dynamic name re-parses as a
// single identifier (the table_rewrites map, by contrast, keeps UNQUOTED names —
// decideWriteTarget records those). RENAME and EXCHANGE both surface as
// STATEMENT_TYPE_RENAME_TABLE (writes.cc:585); ALTER…UPDATE as ALTER_TABLE.
func dispatchRawTables(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_RENAME_TABLE
	kind := "RENAME TABLE"
	switch info.Sub {
	case engine.CmdExchange:
		kind = "EXCHANGE TABLES" // statement type stays RENAME_TABLE (C++ writes.cc:585)
	case engine.CmdAlterUpdate:
		stmt, kind = pb.StatementType_STATEMENT_TYPE_ALTER_TABLE, "ALTER TABLE"
	}
	resp := newWriteResp(stmt)

	targets, _, err := engine.RawTableRefs(e, ast)
	if err != nil {
		return nil, false, err
	}
	// Strict-decide each target IN ORDER (records accessed + table_rewrites, short-
	// circuits on the first reject — mirrors C++ rewriteRenameSide→rewriteOneTarget).
	// Build the splice map of qualify(orig) → QUOTED new qualified name.
	rewrites := map[string]string{}
	for _, tt := range targets {
		d, ok := decideWriteTarget(tt, kind, sel, resp)
		if !ok {
			return resp, true, nil // reject populated
		}
		if d.Action == engine.ActionRename {
			rewrites[qualify(tt.DB, tt.Table)] = engine.QuoteQualified(d.NewDB, d.NewTable)
		}
	}
	out, err := engine.SpliceRawTables(e, sql, rewrites)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = out
	return resp, true, nil
}

// dispatchRawReject inspects a `raw` node — DDL polyglot could not structure —
// and rejects ONLY the forms C++ writes.cc rejects via a structured guard,
// passing EVERYTHING ELSE through (handled=false) so native regenerates it to
// Success, matching C++.
//
// C++-rejected raw forms:
//   - CREATE LIVE VIEW / CREATE WINDOW VIEW → writes.cc:266-268
//   - ALTER DATABASE / ALTER LIVE VIEW (ASTAlterQuery with alter_object != TABLE)
//     → writes.cc:481-483 ("only ALTER TABLE is supported")
//
// Pass-through raw forms (a SEPARATE ClickHouse AST type writes.cc never matches
// → fall through to SELECT → Success): ACCESS-ENTITY DDL that polyglot routes to
// a raw node — ALTER ROLE / ALTER QUOTA / ALTER ROW POLICY / ALTER SETTINGS
// PROFILE / ALTER NAMED COLLECTION, and CREATE ROLE / CREATE QUOTA / CREATE ROW
// POLICY / CREATE SETTINGS PROFILE / CREATE NAMED COLLECTION (all probe-verified
// to land in `raw`). These round-trip cleanly through engine.Generate, so the
// pass-through regenerates the statement intact. Hence the ALTER case is
// CONSERVATIVE: it rejects raw ALTER only when the object is clearly DATABASE or
// LIVE VIEW; any other raw ALTER (i.e. access-entity) passes through.
//
// On a reject the StatementType is UNSPECIFIED — C++ sets no stmt type on these.
func dispatchRawReject(e engine.Engine, ast engine.AST) (*pb.RewriteSQLResponse, bool, error) {
	raw, err := engine.RawSQL(ast)
	if err != nil {
		return nil, false, err
	}
	u := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(u, "CREATE LIVE VIEW"), strings.HasPrefix(u, "CREATE WINDOW VIEW"),
		strings.HasPrefix(u, "CREATE OR REPLACE LIVE VIEW"), strings.HasPrefix(u, "CREATE OR REPLACE WINDOW VIEW"):
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, "CREATE LIVE VIEW / WINDOW VIEW is not supported")
		return resp, true, nil
	case strings.HasPrefix(u, "ALTER ") && rawAlterIsNonTableDDL(u):
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, "only ALTER TABLE is supported")
		return resp, true, nil
	default:
		// Access-entity DDL (ALTER/CREATE ROLE/QUOTA/ROW POLICY/…) and any other
		// raw form C++ doesn't reject: pass through → native regenerates → Success.
		return nil, false, nil
	}
}

// rawAlterIsNonTableDDL reports whether an uppercased raw ALTER statement targets
// DATABASE or a LIVE VIEW — the only raw ALTER objects C++ writes.cc rejects
// (ASTAlterQuery with alter_object == DATABASE, plus the live-view variant).
// Access-entity ALTERs (ALTER ROLE / QUOTA / ROW POLICY / SETTINGS PROFILE /
// NAMED COLLECTION) deliberately do NOT match, so they pass through. Matched on
// the object keyword right after ALTER (with an optional LIVE before VIEW).
func rawAlterIsNonTableDDL(u string) bool {
	fields := strings.Fields(u)
	if len(fields) < 2 || fields[0] != "ALTER" {
		return false
	}
	switch fields[1] {
	case "DATABASE":
		return true
	case "LIVE": // ALTER LIVE VIEW …
		return len(fields) >= 3 && fields[2] == "VIEW"
	default:
		return false
	}
}

// dispatchDatabaseOutOfPhase rejects CREATE/DROP DATABASE as not-yet-supported.
// Phase 3 replaces this with the synthetic-SELECT debug rewrite + db_map validation
// (C++ writes.cc:277-345 / 403-463).
func dispatchDatabaseOutOfPhase(info engine.WriteInfo) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE
	if info.Kind == engine.NodeDropDB {
		stmt = pb.StatementType_STATEMENT_TYPE_DROP_DATABASE
	}
	resp := newWriteResp(stmt)
	rejectUnsupported(resp, "database-level DDL (CREATE/DROP DATABASE) is handled in Phase 3")
	return resp, true, nil
}
