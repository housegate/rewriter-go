package rewriter

import (
	"context"
	"strings"
	"sync"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/handlers"
	"github.com/housegate/rewriter-go/internal/reverse"
)

// NativeRewriter is the in-process Rewriter. Phase 0 = pass-through.
type NativeRewriter struct {
	engine  engine.Engine
	options func(account string) []*pb.RewriteOption // injected account-derived policy
	mu      sync.Mutex
	last    *callContext
}

// callContext is the per-connection record of the most recent Rewrite, used by
// RewriteErrorMessage to invert physical names in error text back to logical ones.
// It stashes the forward rewrite maps + sql_after_rewrite + code so the inversion
// needs no re-parse (the Go interface passes only the error message).
type callContext struct {
	sql              string
	account          string
	code             pb.RewriteCode
	sqlAfterRewrite  string
	tableRewrites    map[string]string
	databaseRewrites map[string]string
}

// stash records the just-finished Rewrite as the per-connection last-call context.
func (r *NativeRewriter) stash(sql, account string, resp *pb.RewriteSQLResponse) {
	r.mu.Lock()
	r.last = &callContext{
		sql: sql, account: account,
		code:             resp.GetCode(),
		sqlAfterRewrite:  resp.GetSqlAfterRewrite(),
		tableRewrites:    resp.GetTableRewrites(),
		databaseRewrites: resp.GetDatabaseRewrites(),
	}
	r.mu.Unlock()
}

// finalize normalizes a handled response to match the C++ oracle. existence_clause
// is stamped on EVERY response — Success AND reject — because the proto contract
// requires it accurate even on a non-Success response (it is derived from the AST,
// which a reject still has; only a SyntaxError, which never parses, leaves it
// UNSPECIFIED). On a non-Success response it ALSO clears statement_type (the C++
// sets that only in setSuccessResponse, so a reject stays UNSPECIFIED — native's
// classify() stamps it, so clear it here) and echoes the original SQL so
// RewriteResult.SQL stays runnable (design §8). NOTE: unlike statement_type,
// existence_clause is NOT cleared on a reject.
func finalize(resp *pb.RewriteSQLResponse, sql string, ec pb.ExistenceClause) {
	resp.ExistenceClause = ec
	if resp.GetCode() == pb.RewriteCode_Success {
		return
	}
	resp.StatementType = pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	if resp.GetSqlAfterRewrite() == "" {
		resp.SqlAfterRewrite = sql
	}
}

// Option configures a NativeRewriter.
type Option func(*NativeRewriter)

// WithOptions injects the account-derived RewriteOption builder (buildDatabaseMap
// in the consumer). When unset, SELECT runs with no rewrite policy (round-trip).
func WithOptions(fn func(account string) []*pb.RewriteOption) Option {
	return func(r *NativeRewriter) { r.options = fn }
}

// New builds a NativeRewriter over the given engine.
func New(e engine.Engine, opts ...Option) *NativeRewriter {
	r := &NativeRewriter{engine: e}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *NativeRewriter) Rewrite(_ context.Context, sql, account string) (RewriteResult, error) {
	resp := &pb.RewriteSQLResponse{SqlAfterRewrite: sql} // SQL always set; echoes input
	ast, err := r.engine.ParseOne(sql)
	if err != nil {
		resp.Code = pb.RewriteCode_SyntaxError
		resp.Message = err.Error()
		r.stash(sql, account, resp)
		return resultFromPB(resp), nil // SyntaxError is a code, not a Go error
	}
	resp.StatementType = r.classify(ast)

	// existence_clause is derived from the AST (IF [NOT] EXISTS) and stamped on
	// EVERY handled response below — it survives rejects (proto contract), unlike
	// statement_type. Computed once here; only a SyntaxError (handled above, no
	// AST) leaves it UNSPECIFIED.
	ec := pb.ExistenceClause_EXISTENCE_CLAUSE_UNSPECIFIED
	if inx, ix, _ := engine.ExistenceClause(ast); inx {
		ec = pb.ExistenceClause_EXISTENCE_CLAUSE_IF_NOT_EXISTS
	} else if ix {
		ec = pb.ExistenceClause_EXISTENCE_CLAUSE_IF_EXISTS
	}

	// Compute the account-derived rewrite policy ONCE; shared by the write and
	// SELECT paths below (nil when no options builder is injected → round-trip).
	var opts []*pb.RewriteOption
	if r.options != nil {
		opts = r.options(account)
	}

	// Phase 2: route writes (CREATE/DROP/ALTER/INSERT/UPDATE/DELETE/RENAME/EXCHANGE/
	// views, + bare-rejects, + out-of-phase CREATE/DROP DATABASE) before SELECT.
	if wresp, handled, werr := handlers.RewriteWrite(r.engine, ast, sql, opts); werr != nil {
		return RewriteResult{}, werr // unexpected/internal → fail-open Go error
	} else if handled {
		// Design §8 + oracle parity: stamp existence_clause; echo input + clear
		// statement_type on reject.
		finalize(wresp, sql, ec)
		r.stash(sql, account, wresp)
		return resultFromPB(wresp), nil
	}

	// Phase 3: route db-level statements (USE / SHOW TABLES / SHOW DATABASES /
	// CREATE DATABASE / DROP DATABASE) after writes, before SELECT.
	if dresp, handled, derr := handlers.RewriteDBLevel(r.engine, ast, sql, opts); derr != nil {
		return RewriteResult{}, derr
	} else if handled {
		finalize(dresp, sql, ec)
		r.stash(sql, account, dresp)
		return resultFromPB(dresp), nil
	}

	// Phase 4: EXISTS / SHOW CREATE (single-target), then GRANT / REVOKE
	// (privilege deltas) — after db-level, before SELECT. Both match only
	// `command` nodes and recognize disjoint verbs, so their relative order is
	// irrelevant; this mirrors the C++ server order (exists → show_create → grant).
	if xresp, handled, xerr := handlers.RewriteExistsShowCreate(r.engine, ast, sql, opts); xerr != nil {
		return RewriteResult{}, xerr
	} else if handled {
		finalize(xresp, sql, ec)
		r.stash(sql, account, xresp)
		return resultFromPB(xresp), nil
	}
	if gresp, handled, gerr := handlers.RewriteGrant(r.engine, ast, sql, opts); gerr != nil {
		return RewriteResult{}, gerr
	} else if handled {
		finalize(gresp, sql, ec)
		r.stash(sql, account, gresp)
		return resultFromPB(gresp), nil
	}

	// Phase 1: route SELECT to the real handler; everything else stays pass-through.
	if kind, _ := engine.NodeKind(ast); kind == engine.NodeSelect {
		hresp, herr := handlers.RewriteSelect(r.engine, ast, opts)
		if herr != nil {
			return RewriteResult{}, herr // unexpected/internal → fail-open Go error
		}
		finalize(hresp, sql, ec) // SELECT never carries IF [NOT] EXISTS → ec stays UNSPECIFIED
		r.stash(sql, account, hresp)
		return resultFromPB(hresp), nil
	}

	// Pass-through: regenerate (proves the engine round-trips); fall back to
	// the input on any generate hiccup so SQL is always runnable.
	if gen, gerr := r.engine.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
	resp.Code = pb.RewriteCode_Success
	finalize(resp, sql, ec)
	r.stash(sql, account, resp)
	return resultFromPB(resp), nil
}

// RewriteErrorMessage inverts physical table/database names in a ClickHouse error
// message back to the logical names the client used, using the maps stashed from
// the most recent successful Rewrite on this connection. Returns the message
// unchanged when there's no prior successful rewrite (nil context or a non-Success
// last call) — mirroring doRewriteErrorMessage's non-Success passthrough.
func (r *NativeRewriter) RewriteErrorMessage(_ context.Context, message string) (string, error) {
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()
	if message == "" || last == nil || last.code != pb.RewriteCode_Success {
		return message, nil
	}
	return reverse.Invert(message, last.sql, last.sqlAfterRewrite, last.tableRewrites, last.databaseRewrites), nil
}

func (r *NativeRewriter) Close() error {
	r.mu.Lock()
	r.last = nil
	r.mu.Unlock()
	return r.engine.Close()
}

// classify maps an AST root to a pb.StatementType via its node kind (top-level
// key). `command` nodes carry only raw SQL, so we sub-classify by leading keyword.
func (r *NativeRewriter) classify(ast engine.AST) pb.StatementType {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
	switch kind {
	case engine.NodeSelect:
		return pb.StatementType_STATEMENT_TYPE_SELECT
	case engine.NodeInsert:
		return pb.StatementType_STATEMENT_TYPE_INSERT
	case engine.NodeCreateTable:
		return pb.StatementType_STATEMENT_TYPE_CREATE_TABLE
	case engine.NodeDropTable:
		return pb.StatementType_STATEMENT_TYPE_DROP_TABLE
	case engine.NodeAlterTable:
		return pb.StatementType_STATEMENT_TYPE_ALTER_TABLE
	case engine.NodeCreateDB:
		return pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE
	case engine.NodeDropDB:
		return pb.StatementType_STATEMENT_TYPE_DROP_DATABASE
	case engine.NodeTruncate:
		return pb.StatementType_STATEMENT_TYPE_TRUNCATE_TABLE
	case engine.NodeDelete:
		return pb.StatementType_STATEMENT_TYPE_DELETE
	case engine.NodeCommand:
		sql, _ := engine.CommandSQL(ast)
		return classifyCommand(sql)
	default:
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
}

// classifyCommand sub-classifies an opaque `command` node by leading keyword(s).
func classifyCommand(sql string) pb.StatementType {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(u, "USE"):
		return pb.StatementType_STATEMENT_TYPE_USE
	case strings.HasPrefix(u, "GRANT"):
		return pb.StatementType_STATEMENT_TYPE_GRANT
	case strings.HasPrefix(u, "REVOKE"):
		return pb.StatementType_STATEMENT_TYPE_REVOKE
	case strings.HasPrefix(u, "RENAME"):
		return pb.StatementType_STATEMENT_TYPE_RENAME_TABLE
	case strings.HasPrefix(u, "EXISTS"):
		return pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE
	case strings.HasPrefix(u, "SHOW CREATE"):
		return pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE
	case strings.HasPrefix(u, "SHOW DATABASES"), strings.HasPrefix(u, "SHOW SCHEMAS"):
		return pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES
	case strings.HasPrefix(u, "SHOW TABLES"), strings.HasPrefix(u, "SHOW"):
		return pb.StatementType_STATEMENT_TYPE_SHOW_TABLES
	default:
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
}
