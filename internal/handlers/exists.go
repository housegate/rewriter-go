package handlers

import (
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteExistsShowCreate ports exists.cc + show_create.cc — two near-identical
// single-target handlers (EXISTS TABLE / SHOW CREATE TABLE) sharing a tokenize
// extractor and the write-side name-rewrite machinery. Returns (resp, handled,
// err) with the RewriteWrite contract; native.go calls it after RewriteDBLevel.
//
// Only the TABLE object is accepted; the DATABASE/VIEW/DICTIONARY variants are
// rejected as UnsupportedStatement. The accepted target runs through
// decideWriteTarget (records accessed + table_rewrites; rejects remote/invalid),
// and the output is the canonical `EXISTS TABLE <name>` / `SHOW CREATE TABLE
// <name>` form — always with the TABLE keyword, db-qualified and backtick-quoted
// WhenNecessary, matching C++ formatAst.
func RewriteExistsShowCreate(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	if kind != engine.NodeCommand {
		return nil, false, nil // EXISTS / SHOW CREATE only ever arrive as command nodes
	}
	t, err := engine.ParseObjectTarget(e, sql)
	if err != nil {
		return nil, false, err
	}
	if t.Verb == engine.VerbNone {
		return nil, false, nil // not EXISTS / SHOW CREATE → caller falls through
	}

	stmt := pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE
	keyword := "EXISTS"
	if t.Verb == engine.VerbShowCreate {
		stmt, keyword = pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE, "SHOW CREATE"
	}
	resp := newWriteResp(stmt)

	if t.ObjType != "TABLE" {
		rejectUnsupported(resp, keyword+" "+t.ObjType+" is not supported; only "+keyword+" TABLE is allowed")
		return resp, true, nil
	}

	sel := nameresolve.FindActive(opts)
	tt := engine.TableTarget{DB: t.DB, Table: t.Table}
	d, ok := decideWriteTarget(tt, keyword+" TABLE", sel, resp)
	if !ok {
		return resp, true, nil // reject populated (accessed recorded first, like C++)
	}
	db, table := t.DB, t.Table
	if d.Action == engine.ActionRename {
		db, table = d.NewDB, d.NewTable
	}
	resp.SqlAfterRewrite = buildObjectSQL(keyword, t.Temporary, db, table)
	return resp, true, nil
}

// buildObjectSQL renders the canonical EXISTS / SHOW CREATE output: the verb, an
// always-present TABLE keyword (ClickHouse normalizes the bare and TEMPORARY
// forms to include it), and the db-qualified, WhenNecessary-backtick-quoted name
// (engine.QuoteQualified — the same quoting the RENAME splice uses, so a dotted
// dynamic table name like `tenant.events` is quoted as one identifier).
func buildObjectSQL(keyword string, temporary bool, db, table string) string {
	s := keyword + " "
	if temporary {
		s += "TEMPORARY "
	}
	return s + "TABLE " + engine.QuoteQualified(db, table)
}
