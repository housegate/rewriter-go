// Package handlers ports the C++ rewriter-grpc statement handlers. Phase 1: SELECT.
package handlers

import (
	"sort"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteSelect ports handleSelectQuery: resolve every table, rewrite the AST,
// populate table_rewrites + original_accessed_tables, regenerate. (Options/CTE/
// GLOBAL are layered in Tasks 8-10.)
func RewriteSelect(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	resp := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		Message:       "success",
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{},
	}
	sel := nameresolve.FindActive(opts)

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
			return nil, berr
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
				return nil, ierr
			}
		}
	}
	sort.Strings(resp.FailedCteAliases) // deterministic order

	originals, err := engine.CollectSelectTables(ast)
	if err != nil {
		return nil, err
	}
	resp.OriginalAccessedTables = buildAccessed(originals, sel)

	rewritten, err := engine.RewriteSelectTables(ast, func(tt engine.TableTarget) engine.TableDecision {
		return decideTable(tt, sel, resp.TableRewrites)
	})
	if err != nil {
		return nil, err
	}

	rewritten, err = applyOptions(rewritten, opts)
	if err != nil {
		return nil, err
	}

	rewritten, err = engine.ForceGlobalForRemoteAsymmetry(rewritten)
	if err != nil {
		return nil, err
	}

	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, err
	}
	resp.SqlAfterRewrite = sql
	return resp, nil
}

// decideTable maps a nameresolve.Outcome to an engine.TableDecision and records the
// table_rewrites entry. SELECT is lenient: StatusInvalid → skip (no error).
func decideTable(tt engine.TableTarget, sel nameresolve.Selection, rewrites map[string]string) engine.TableDecision {
	o := nameresolve.Resolve(tt.DB, tt.Table, sel)
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

// buildAccessed produces deduped, key-sorted AccessedTable entries (matches the
// C++ std::map iteration order).
func buildAccessed(targets []engine.TableTarget, sel nameresolve.Selection) []*pb.AccessedTable {
	seen := map[string]bool{}
	keys := make([]string, 0, len(targets))
	byKey := map[string]engine.TableTarget{}
	for _, tt := range targets {
		k := qualify(tt.DB, tt.Table)
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
		byKey[k] = tt
	}
	sort.Strings(keys)
	out := make([]*pb.AccessedTable, 0, len(keys))
	for _, k := range keys {
		tt := byKey[k]
		a := nameresolve.ResolveAccessed(tt.DB, tt.Table, sel)
		out = append(out, &pb.AccessedTable{
			OriginalDatabase: tt.DB, OriginalTable: tt.Table,
			LogicalDatabase: a.LogicalDB, PhysicalDatabase: a.PhysicalDB, IsRemote: a.IsRemote,
		})
	}
	return out
}

// qualify mirrors nameresolve.qualify (kept local to avoid exporting it).
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}
