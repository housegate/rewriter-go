package handlers

import (
	"strings"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// rewriteStringLookups applies spec 2026-09-26 T6 to the hasColumnInTable
// calls of a SELECT body. Called from rewriteSelectCore before
// RewriteSelectTables, so an embedded view/INSERT/CTAS body gets the same
// treatment as a top-level SELECT. A joinGet/joinGetOrNull or dictGet-family
// call never reaches here: PreflightTableReferences refuses every one of them
// statement-wide (embedded bodies included) before any handler runs, because
// ClickHouse cannot resolve a dotted logical name for them (measured on
// ClickHouse 25.8) — there is no rewrite to perform.
//
// hasColumnInTable's database/table pair (its last two arguments before the
// column name — an optional leading host[, user[, pw]] triple shifts every
// index, exactly like stringLookupArgDatabase) is resolved and rewritten
// exactly like an ordinary FROM reference when both are string literals: a
// mapped pair becomes the physical database and the constructed
// "<logical>.<table>" physical table name, recorded in table_rewrites and,
// once per distinct table, in original_accessed_tables. An unmapped, remote,
// or storage-integrity Active pair, or a non-literal database/table, is
// refused with the "does not resolve" message.
//
// Returns (ast, handled, err): handled=true means resp already carries a
// refusal and the caller must stop; the returned ast is otherwise the
// (possibly mutated) input, ready for RewriteSelectTables. Only runs while a
// dynamic rewrite policy is active; static/no-rewrite requests leave every
// call untouched. A protected database named directly in a lookup argument
// never reaches here either: the preflight's T3 check refused it first.
func rewriteStringLookups(ast engine.AST, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (engine.AST, bool, error) {
	if sel.Mode != nameresolve.ModeDynamic {
		return ast, false, nil
	}
	calls, err := engine.StringLookupCalls(ast)
	if err != nil {
		return nil, false, err
	}
	// First pass (read-only): find any call that must refuse the whole
	// statement BEFORE any mutation is attempted. decideStringLookup is a
	// pure function of (call, sel), so the mutation pass below can never
	// disagree with it.
	for _, call := range calls {
		if !isHasColumnInTable(call.Function) {
			continue
		}
		if _, refuse := decideStringLookup(call, sel); refuse {
			resp.Code = pb.RewriteCode_InvalidRewriteRequest
			resp.Message = stringLookupUnresolvedMessage(call)
			return ast, true, nil
		}
	}
	rewritten, err := engine.RewriteStringLookups(ast, func(call engine.StringLookup) (string, bool) {
		if !isHasColumnInTable(call.Function) {
			return "", false
		}
		d, refuse := decideStringLookup(call, sel)
		if refuse || !d.rewrite {
			return "", false
		}
		orig := engine.TableTarget{DB: d.origDB, Table: d.origTable}
		recordRewrite(resp.TableRewrites, orig, d.newDB, d.newTable)
		if !accessedContains(resp, orig) {
			recordAccessedWrite(resp, orig, sel)
		}
		return d.newDB + "." + d.newTable, true
	})
	if err != nil {
		return nil, false, err
	}
	return rewritten, false, nil
}

// accessedContains reports whether resp already records tt (same original
// database and table), so a table named by several lookups, or by a lookup
// and a FROM clause, is reported once.
func accessedContains(resp *pb.RewriteSQLResponse, tt engine.TableTarget) bool {
	for _, a := range resp.GetOriginalAccessedTables() {
		if a.GetOriginalDatabase() == tt.DB && a.GetOriginalTable() == tt.Table {
			return true
		}
	}
	return false
}

// stringLookupDecision is decideStringLookup's pure-function outcome for one
// call: rewrite=true means (origDB, origTable) resolves to (newDB, newTable).
type stringLookupDecision struct {
	rewrite           bool
	origDB, origTable string
	newDB, newTable   string
}

// decideStringLookup returns the resolved rewrite of a hasColumnInTable call,
// or refuse=true when its pair is non-literal, storage-integrity Active, or
// otherwise unresolvable. The database and table literals are resolved as the
// two separate names ClickHouse reads (call.DB, call.Table), never joined and
// re-split: a literal that is empty or contains a '.' is refused, because
// re-splitting 'db1.t', 'x' would resolve db1 / t.x, a table ClickHouse does
// not read.
func decideStringLookup(call engine.StringLookup, sel nameresolve.Selection) (stringLookupDecision, bool) {
	if !isHasColumnInTable(call.Function) || !call.Literal {
		return stringLookupDecision{}, true
	}
	db, table := call.DB, call.Table
	if db == "" || table == "" || strings.Contains(db, ".") || strings.Contains(table, ".") {
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

func stringLookupUnresolvedMessage(call engine.StringLookup) string {
	return call.Function + ` target "` + call.Arg + `" does not resolve through the caller's databases`
}
