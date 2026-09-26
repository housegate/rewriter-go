package handlers

import (
	"strings"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// rewriteStringLookups applies spec 2026-09-26 T6 to ast. Called from
// rewriteSelectCore before RewriteSelectTables, so an embedded view/INSERT/
// CTAS body gets the same treatment as a top-level SELECT.
//
//   - joinGet/joinGetOrNull and the dictGet*/dictHas/dictGetHierarchy/
//     dictIsIn/dictGetChildren/dictGetDescendants family are refused whenever
//     they name a first argument at all, in every mode. Controller ruling 2
//     measured ClickHouse 25.8 directly: neither the quoted-qualified-string
//     form ('phys.`db1.j`'), the plain dotted-string form ('phys.db1.j'), nor
//     the unquoted identifier form (phys.`db1.j`) actually resolves a dotted
//     logical table name for these functions — all three fail with a
//     SYNTAX_ERROR "Invalid qualified name" — so there is no working rewrite
//     to perform; only refuse, using the caller's own spelling.
//   - hasColumnInTable's database/table pair (its last two arguments before
//     the column name — an optional leading host[, user[, pw]] triple shifts
//     every index, exactly like stringLookupArgDatabase) is resolved and
//     rewritten exactly like an ordinary FROM reference when both are string
//     literals: a mapped pair becomes the physical database and the
//     constructed "<logical>.<table>" physical table name, recorded in
//     table_rewrites/original_accessed_tables like a FROM reference. An
//     unmapped, remote, or storage-integrity Active pair, or a non-literal
//     database/table, is refused with the same "does not resolve" message.
//
// Returns (ast, handled, err): handled=true means resp already carries a
// refusal and the caller must stop; the returned ast is otherwise the
// (possibly mutated) input, ready for RewriteSelectTables.
//
// Only runs while a dynamic rewrite policy is active (sel.Mode ==
// nameresolve.ModeDynamic); static/no-rewrite requests leave every call
// untouched, matching the rest of the table-reference policy. A protected
// database named directly in a lookup's argument (e.g. joinGet('phys.`x`',
// …)) never reaches here: Task 4's preflight (CollectDatabaseReferences via
// collectStringLookupDatabases) already refused it before any handler ran.
//
// Task 7 fix round 1 finding 2: PreflightTableReferences now ALSO refuses
// every joinGet/dictGet-family call statement-wide (SELECT bodies included,
// via engine.StringLookupCalls), before rewriteSelectCore ever runs — so by
// the time this function's own joinGet/dictGet branch below would fire, the
// preflight has already refused the statement with the identical message.
// That branch is kept as a defensive, harmless second layer rather than
// removed: nothing currently relies on it firing, but nothing is wrong if it
// does.
func rewriteStringLookups(ast engine.AST, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (engine.AST, bool, error) {
	if sel.Mode != nameresolve.ModeDynamic {
		return ast, false, nil
	}
	calls, err := engine.StringLookupCalls(ast)
	if err != nil {
		return nil, false, err
	}
	if len(calls) == 0 {
		return ast, false, nil
	}
	// First pass (read-only): find any call that must refuse the whole
	// statement BEFORE any mutation is attempted — mirrors
	// rejectStorageIntegrityNamespaces running entirely before
	// RewriteSelectTables. decideStringLookup is a pure function of (call,
	// sel), so re-deriving the same decision during the mutation pass below
	// can never disagree with what this pass found, regardless of AST
	// traversal order.
	for _, call := range calls {
		if _, refuse := decideStringLookup(call, sel); refuse {
			resp.Code = pb.RewriteCode_InvalidRewriteRequest
			resp.Message = stringLookupUnresolvedMessage(call)
			return ast, true, nil
		}
	}
	rewritten, err := engine.RewriteStringLookups(ast, func(call engine.StringLookup) (string, bool) {
		d, refuse := decideStringLookup(call, sel)
		if refuse || !d.rewrite {
			return "", false
		}
		orig := engine.TableTarget{DB: d.origDB, Table: d.origTable}
		recordRewrite(resp.TableRewrites, orig, d.newDB, d.newTable)
		recordAccessedWrite(resp, orig, sel)
		return d.newDB + "." + d.newTable, true
	})
	if err != nil {
		return nil, false, err
	}
	return rewritten, false, nil
}

// stringLookupDecision is decideStringLookup's pure-function outcome for one
// call: rewrite=true means (origDB, origTable) resolves to (newDB, newTable).
type stringLookupDecision struct {
	rewrite           bool
	origDB, origTable string
	newDB, newTable   string
}

// decideStringLookup returns the resolved rewrite, or refuse=true when call
// must be refused outright (joinGet/dictGet-family always; hasColumnInTable
// only when its pair is non-literal, storage-integrity Active, or otherwise
// unresolvable).
func decideStringLookup(call engine.StringLookup, sel nameresolve.Selection) (stringLookupDecision, bool) {
	if !isHasColumnInTable(call.Function) {
		// joinGet/dictGet family: ruling 2 — always refused, never rewritten.
		return stringLookupDecision{}, true
	}
	if !call.Literal || call.Arg == "" {
		return stringLookupDecision{}, true
	}
	db, table := splitQualifiedArg(call.Arg)
	if table == "" {
		return stringLookupDecision{}, true
	}
	if _, _, ok := nameresolve.LookupStorageIntegrity(db, table, sel.Dynamic); ok {
		return stringLookupDecision{}, true
	}
	o := nameresolve.Resolve(db, table, sel)
	if o.Status != nameresolve.StatusRewrite {
		return stringLookupDecision{}, true
	}
	return stringLookupDecision{
		rewrite: true, origDB: db, origTable: table, newDB: o.PhysicalDB, newTable: o.NewTable,
	}, false
}

func isHasColumnInTable(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "hascolumnintable")
}

// splitQualifiedArg splits "db.table" on the FIRST '.' — the same convention
// stringLookupDatabase and every other qualified-name split in this codebase
// uses, so a table name that itself contains a dot is never mis-split.
func splitQualifiedArg(arg string) (db, table string) {
	idx := strings.IndexByte(arg, '.')
	if idx < 0 {
		return "", arg
	}
	return arg[:idx], arg[idx+1:]
}

func stringLookupUnresolvedMessage(call engine.StringLookup) string {
	return call.Function + ` target "` + call.Arg + `" does not resolve through the caller's databases`
}
