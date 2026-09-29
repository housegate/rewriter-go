// Package handlers ports the C++ rewriter-grpc statement handlers. Phase 1: SELECT.
package handlers

import (
	"fmt"
	"sort"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// RewriteSelect ports handleSelectQuery: resolve every table, rewrite the AST,
// populate table_rewrites + original_accessed_tables, regenerate. (Options/CTE/
// GLOBAL are layered in Tasks 8-10.)
//
// It is a thin wrapper over rewriteSelectCore: run the pipeline, then Generate the
// rewritten AST into SqlAfterRewrite.
func RewriteSelect(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption, sourceSQL ...string) (*pb.RewriteSQLResponse, error) {
	rewritten, resp, err := rewriteSelectCore(e, ast, opts, sourceSQL...)
	if err != nil {
		return nil, err
	}
	if resp.Code != pb.RewriteCode_Success {
		clearOnUnresolved(resp)
		return resp, nil // reject: leave SqlAfterRewrite empty; native.finalize echoes the input
	}
	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, err
	}
	resp.SqlAfterRewrite = sql
	return resp, nil
}

// rewriteSelectCore runs the full SELECT rewrite pipeline and returns the rewritten
// AST + a response carrying table_rewrites/original_accessed_tables/failed_cte_aliases
// (SqlAfterRewrite left empty — the caller Generates or splices). Shared by
// RewriteSelect (top-level) and the view-body path (handlers/writes.go dispatchView,
// mirroring C++ rewriteEmbeddedViewBody which runs the same SELECT pipeline on a
// view's embedded body).
func rewriteSelectCore(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption, sourceSQL ...string) (engine.AST, *pb.RewriteSQLResponse, error) {
	resp := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		Message:       "success",
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{},
	}
	sel := nameresolve.FindActive(opts)
	// selectSQL is the source text the caller passes: the statement's own
	// text for a top-level SELECT, and the enclosing write statement's text
	// for an embedded INSERT … SELECT / CTAS / CREATE VIEW body
	// (dispatchView and rewriteEmbeddedBody pass it). It feeds only the
	// token-level checks in rejectUngovernedReads.
	selectSQL := ""
	if len(sourceSQL) > 0 {
		selectSQL = sourceSQL[0]
	}

	// CTE injection (CommonTableExprRewrite): parse bodies, then inject ONLY the
	// aliases actually referenced by the query (referenced-only, non-transitive).
	// Mirrors the C++ ASTRewriteCTETransformer: collectCTEKeysForSelect walks only
	// the main SELECT's AST (pre-injection), so body-to-body refs are NOT followed.
	// Parse failures are recorded for ALL aliases regardless of reference
	// (mirrors select.cc:775 — parseCTEMapToAST records all failures upfront).
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_CommonTableExprRewrite {
			continue
		}
		// Step 1: parse all bodies; record all parse failures (not just referenced ones).
		allBodies := map[string]engine.AST{}
		for alias, cte := range o.GetCommonTableExprArgs().GetCteMap() {
			body, perr := e.ParseOne(cte.GetSql())
			if perr != nil {
				resp.FailedCteAliases = append(resp.FailedCteAliases, alias)
				continue
			}
			allBodies[alias] = body
		}

		// Step 2: collect bare table names referenced by the current AST.
		// These are the candidate CTE-alias references.
		bareNames, berr := engine.BareTableNames(ast)
		if berr != nil {
			return nil, nil, berr
		}

		// Step 3: seed referenced set — only aliases that appear as bare refs.
		referenced := map[string]bool{}
		for _, name := range bareNames {
			if _, ok := allBodies[name]; ok {
				referenced[name] = true
			}
		}

		// Step 4: build the referenced-only bodies map for injection.
		bodies := map[string]engine.AST{}
		for alias := range referenced {
			bodies[alias] = allBodies[alias]
		}

		if len(bodies) > 0 {
			var ierr error
			if ast, ierr = engine.InjectCTEs(ast, bodies); ierr != nil {
				return nil, nil, ierr
			}
		}
	}
	sort.Strings(resp.FailedCteAliases) // deterministic order

	originals, err := engine.CollectSelectTables(ast)
	if err != nil {
		return nil, nil, err
	}
	storageIntegrityActive := sel.Mode == nameresolve.ModeDynamic && nameresolve.StorageIntegritySurfaceActive(sel.Dynamic)
	if storageIntegrityActive {
		for i, target := range originals {
			semantic, ok := engine.SemanticTableTarget(e, target)
			if !ok {
				return nil, nil, fmt.Errorf("decode storage-integrity table target %q", qualify(target.DB, target.Table))
			}
			originals[i] = semantic
		}
	}
	var namespaceRefs []engine.NamespaceRef
	if sel.Mode == nameresolve.ModeDynamic {
		var ferr error
		namespaceRefs, ferr = engine.CollectNamespaceRefs(ast)
		if ferr != nil {
			return nil, nil, ferr
		}
	}
	resp.OriginalAccessedTables = buildAccessed(originals, sel)

	if sel.Mode == nameresolve.ModeDynamic {
		if rejectStorageIntegrityNamespaces(e, resp, namespaceRefs, sel, pb.RewriteCode_RewriteError) {
			return ast, resp, nil
		}
		// Controller ruling 1 (spec 2026-09-26 T5, Task 7): while the
		// storage-integrity surface is active, PreflightTableReferences does
		// NOT run the table-function/table-engine/table-setting allowlists
		// (that would risk pre-empting an SI-owned message this corpus
		// pins) — run the same check here instead, now that the SI
		// namespace policy above has already had first refusal.
		if nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
			if rejected, cerr := rejectDisallowedCarriers(e, ast, resp); cerr != nil {
				return nil, nil, cerr
			} else if rejected {
				return ast, resp, nil
			}
			if rejected, rerr := rejectUngovernedReads(e, ast, selectSQL, sel, resp); rerr != nil {
				return nil, nil, rerr
			} else if rejected {
				return ast, resp, nil
			}
		}
		for _, tt := range originals {
			if _, ok := nameresolve.LookupStorageIntegrityPhysical(tt.DB, tt.Table, sel.Dynamic); ok {
				resp.Code = pb.RewriteCode_RewriteError
				resp.Message = nameresolve.StorageIntegrityPhysicalRejectMessage(qualify(tt.DB, tt.Table))
				return ast, resp, nil
			}
			if _, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
				logical, authorized := nameresolve.AuthorizeStorageIntegrityLogical(tt.DB, sel.Dynamic)
				if !authorized {
					resp.Code = pb.RewriteCode_InvalidRewriteRequest
					resp.Message = nameresolve.StorageIntegrityUnauthorizedMessage(logical)
					return ast, resp, nil
				}
			}
		}
	}

	// Spec G D3: a statement that touches at least one SI table must not
	// address the reserved row-id column anywhere. Checked on the ORIGINAL
	// AST (the substituted bodies legitimately mention it in EXCEPT). FINAL
	// and SAMPLE are rejected at the same pre-rewrite boundary because a
	// derived-table substitution cannot silently discard their semantics.
	if sel.Mode == nameresolve.ModeDynamic && touchesStorageIntegrity(resp.OriginalAccessedTables) {
		semanticSITarget := func(tt engine.TableTarget) (engine.TableTarget, error) {
			semantic, ok := engine.SemanticTableTarget(e, tt)
			if !ok {
				return engine.TableTarget{}, fmt.Errorf("decode storage-integrity table target %q", qualify(tt.DB, tt.Table))
			}
			return semantic, nil
		}
		wrapperTargets, merr := engine.UnsupportedTableWrapperTargets(ast)
		if merr != nil {
			return nil, nil, merr
		}
		modified := false
		for _, tt := range wrapperTargets {
			tt, merr = semanticSITarget(tt)
			if merr != nil {
				return nil, nil, merr
			}
			if _, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
				modified = true
				break
			}
		}
		if !modified && len(sourceSQL) > 0 {
			withOffsetTargets, oerr := engine.WithOffsetTargets(e, sourceSQL[0])
			if oerr != nil {
				return nil, nil, oerr
			}
			prewhereTargets, perr := engine.PrewhereTargets(e, sourceSQL[0])
			if perr != nil {
				return nil, nil, perr
			}
			for _, tt := range withOffsetTargets {
				tt, oerr = semanticSITarget(tt)
				if oerr != nil {
					return nil, nil, oerr
				}
				if _, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
					modified = true
					break
				}
			}
			for _, tt := range prewhereTargets {
				if _, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
					modified = true
					break
				}
			}
		}
		if modified {
			resp.Code = pb.RewriteCode_RewriteError
			resp.Message = "FINAL/SAMPLE/PREWHERE/WITH OFFSET/column aliases on storage-integrity tables are not supported"
			return ast, resp, nil
		}
		rid := nameresolve.ReservedRowIDColumn(sel.Dynamic)
		var scopeTargetErr error
		hit, herr := engine.ReferencesIdentifierInScope(ast, rid, func(tt engine.TableTarget) bool {
			semantic, err := semanticSITarget(tt)
			if err != nil {
				scopeTargetErr = err
				return false
			}
			_, _, ok := nameresolve.LookupStorageIntegrity(semantic.DB, semantic.Table, sel.Dynamic)
			return ok
		})
		if herr != nil {
			return nil, nil, herr
		}
		if scopeTargetErr != nil {
			return nil, nil, scopeTargetErr
		}
		if hit {
			resp.Code = pb.RewriteCode_RewriteError
			resp.Message = fmt.Sprintf(reservedColumnRejectFmt, rid)
			return ast, resp, nil
		}
	}

	// T6 (spec 2026-09-26): string-form lookups (joinGet/dictGet-family,
	// hasColumnInTable) run before RewriteSelectTables so an embedded
	// view/INSERT/CTAS body gets the same treatment as a top-level SELECT.
	var lookupHandled bool
	ast, lookupHandled, err = rewriteStringLookups(ast, sel, resp)
	if err != nil {
		return nil, nil, err
	}
	if lookupHandled {
		return ast, resp, nil
	}

	var siErr error
	// unresolved is the first unqualified table (DB == "", which includes the
	// one-part dotted quoted form `db1.t`) that does not resolve through the
	// session's logical database in dynamic mode: the logical context is
	// empty, unmapped, or maps to a missing remote upstream. ClickHouse would
	// resolve the verbatim name in the session's current database — the
	// physical database, where `db1.t` is an Active SI table's ordinary
	// physical table — so it is refused, not skipped (spec 2026-09-26 §5).
	// CTE names never reach this callback, and a qualified unmapped name
	// (db2.x) stays a lenient skip: ClickHouse has no physical db2, and the
	// host's permission check owns it.
	var unresolved string
	var haveUnresolved bool
	rewritten, err := engine.RewriteSelectTables(ast, func(tt engine.TableTarget) engine.TableDecision {
		if storageIntegrityActive {
			semantic, ok := engine.SemanticTableTarget(e, tt)
			if !ok {
				siErr = fmt.Errorf("decode storage-integrity table target %q", qualify(tt.DB, tt.Table))
				return engine.TableDecision{Action: engine.ActionSkip}
			}
			tt = semantic
		}
		if sel.Mode == nameresolve.ModeDynamic {
			if tbl, _, ok := nameresolve.LookupStorageIntegrity(tt.DB, tt.Table, sel.Dynamic); ok {
				d, derr := storageIntegrityDecision(e, tt, tbl, sel.Dynamic.GetStorageIntegrity(), resp.TableRewrites)
				if derr != nil {
					siErr = derr
					return engine.TableDecision{Action: engine.ActionSkip}
				}
				return d
			}
		}
		o := nameresolve.Resolve(tt.DB, tt.Table, sel)
		if sel.Mode == nameresolve.ModeDynamic && tt.DB == "" && o.Status == nameresolve.StatusInvalid {
			if !haveUnresolved {
				unresolved, haveUnresolved = tt.Table, true
			}
			return engine.TableDecision{Action: engine.ActionSkip}
		}
		return decideTable(tt, o, resp.TableRewrites)
	})
	if err != nil {
		return nil, nil, err
	}
	if siErr != nil {
		return nil, nil, siErr
	}
	if haveUnresolved {
		// The partial table_rewrites map is kept here: each caller empties the
		// map only when this refusal is its FINAL answer (clearOnUnresolved),
		// so a different refusal that outranks it (the SI write refusal of a
		// view body) keeps the map it would have without the unresolved name.
		resp.Code = pb.RewriteCode_InvalidRewriteRequest
		resp.Message = nameresolve.UnresolvedUnqualifiedTableMessage(unresolved)
		return ast, resp, nil
	}

	rewritten, err = applyOptions(rewritten, opts)
	if err != nil {
		return nil, nil, err
	}

	rewritten, err = engine.ForceGlobalForRemoteAsymmetry(rewritten)
	if err != nil {
		return nil, nil, err
	}

	return rewritten, resp, nil
}

// decideTable maps the nameresolve.Outcome already resolved for tt to an
// engine.TableDecision and records the table_rewrites entry. SELECT is lenient:
// StatusInvalid → skip (no error). The caller refuses an unqualified
// dynamic-mode StatusInvalid before reaching it, so the lenient skip covers
// qualified unmapped names only.
func decideTable(tt engine.TableTarget, o nameresolve.Outcome, rewrites map[string]string) engine.TableDecision {
	switch o.Status {
	case nameresolve.StatusRewrite:
		recordRewrite(rewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRename, NewDB: o.PhysicalDB, NewTable: o.NewTable}
	case nameresolve.StatusRemote:
		recordRewrite(rewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRemote, Remote: &engine.RemoteSpec{
			Addr: o.RemoteAddr, DB: o.PhysicalDB, Table: o.NewTable, User: o.RemoteUser, Password: o.RemotePassword,
		}}
	default: // StatusPassthrough, StatusInvalid (lenient skip), StatusRemoteUnsupported
		return engine.TableDecision{Action: engine.ActionSkip}
	}
}

// recordRewrite adds a table_rewrites entry unless the name is unchanged.
// Key/value are "db.table" (or bare "table").
func recordRewrite(rewrites map[string]string, tt engine.TableTarget, newDB, newTable string) {
	from := qualify(tt.DB, tt.Table)
	to := qualify(newDB, newTable)
	if from != to {
		rewrites[from] = to
	}
}

// buildAccessed produces AccessedTable entries deduped by
// engine.TableTarget.Identity() — never by the written "db.table" string,
// which the quoted twin `db1.t` shares with the qualified db1.t — and sorted
// by the written name, which keeps the order the corpus pins. Two distinct
// tables that share a written name are ordered by database, then table, so
// the unqualified twin precedes the qualified name.
func buildAccessed(targets []engine.TableTarget, sel nameresolve.Selection) []*pb.AccessedTable {
	seen := map[engine.TableTarget]bool{}
	unique := make([]engine.TableTarget, 0, len(targets))
	for _, tt := range targets {
		id := tt.Identity()
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, tt)
	}
	sort.SliceStable(unique, func(i, j int) bool {
		ki, kj := qualify(unique[i].DB, unique[i].Table), qualify(unique[j].DB, unique[j].Table)
		if ki != kj {
			return ki < kj
		}
		if unique[i].DB != unique[j].DB {
			return unique[i].DB < unique[j].DB
		}
		return unique[i].Table < unique[j].Table
	})
	out := make([]*pb.AccessedTable, 0, len(unique))
	for _, tt := range unique {
		a := nameresolve.ResolveAccessed(tt.DB, tt.Table, sel)
		out = append(out, &pb.AccessedTable{
			OriginalDatabase: tt.DB, OriginalTable: tt.Table,
			LogicalDatabase: a.LogicalDB, PhysicalDatabase: a.PhysicalDB, IsRemote: a.IsRemote,
			IsStorageIntegrity: a.IsStorageIntegrity,
		})
	}
	return out
}

// clearOnUnresolved empties resp's table_rewrites when its FINAL message is
// the unresolved-unqualified refusal (whatever code the caller gave it: an
// embedded INSERT … SELECT / CTAS body answers UnsupportedStatement), so that
// rejection carries no partial map — neither the tables the walk rewrote
// before the refusal nor a write statement's own targets. Every caller of
// rewriteSelectCore calls it after settling its final code and message.
func clearOnUnresolved(resp *pb.RewriteSQLResponse) {
	if resp.GetCode() != pb.RewriteCode_Success &&
		nameresolve.IsUnresolvedUnqualifiedTableMessage(resp.GetMessage()) {
		resp.TableRewrites = map[string]string{}
	}
}

// qualify mirrors nameresolve.qualify (kept local to avoid exporting it).
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}

// touchesStorageIntegrity reports whether any accessed table is SI-flagged.
func touchesStorageIntegrity(accessed []*pb.AccessedTable) bool {
	for _, a := range accessed {
		if a.GetIsStorageIntegrity() {
			return true
		}
	}
	return false
}
