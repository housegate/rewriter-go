package rewriter

import (
	"context"
	"strings"
	"sync"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// NativeRewriter is the in-process Rewriter. Phase 0 = pass-through.
type NativeRewriter struct {
	engine engine.Engine
	mu     sync.Mutex
	last   *callContext
}

type callContext struct {
	sql     string
	account string
}

// New builds a pass-through NativeRewriter over the given engine.
func New(e engine.Engine) *NativeRewriter { return &NativeRewriter{engine: e} }

func (r *NativeRewriter) Rewrite(_ context.Context, sql, account string) (RewriteResult, error) {
	resp := &pb.RewriteSQLResponse{SqlAfterRewrite: sql} // SQL always set; echoes input
	ast, err := r.engine.ParseOne(sql)
	if err != nil {
		resp.Code = pb.RewriteCode_SyntaxError
		resp.Message = err.Error()
		return resultFromPB(resp), nil // SyntaxError is a code, not a Go error
	}
	resp.StatementType = r.classify(ast)
	// Pass-through: regenerate (proves the engine round-trips); fall back to
	// the input on any generate hiccup so SQL is always runnable.
	if gen, gerr := r.engine.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
	resp.Code = pb.RewriteCode_Success
	r.mu.Lock()
	r.last = &callContext{sql: sql, account: account}
	r.mu.Unlock()
	return resultFromPB(resp), nil
}

func (r *NativeRewriter) RewriteErrorMessage(_ context.Context, message string) (string, error) {
	// Phase 0: no reverse-mapping yet (arrives in Phase 5). Echo the message.
	return message, nil
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
