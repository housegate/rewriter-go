package rewriter

import (
	"context"
	"strings"
	"sync"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/handlers"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-go/internal/reverse"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// NativeRewriter is the in-process Rewriter. Phase 0 = pass-through.
type NativeRewriter struct {
	measuredSnapshotProfiles map[string]string
	engine                   engine.Engine
	options                  func(account string) []*pb.RewriteOption // injected account-derived policy
	mu                       sync.Mutex
	last                     *callContext
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
//
// When storage integrity is active, this is also the shared final rejection
// annotation point: every non-Success response is checked for an SI object so
// opaque and otherwise unmodelled statement classes still name what they
// addressed (Spec I D2).
func finalize(resp *pb.RewriteSQLResponse, ast engine.AST, sql string, ec pb.ExistenceClause, siVersion pb.StorageIntegrityContractVersion, e engine.Engine, sel nameresolve.Selection) {
	resp.ExistenceClause = ec
	resp.StorageIntegrityContractVersion = siVersion
	if resp.GetCode() == pb.RewriteCode_Success {
		return
	}
	resp.StatementType = pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	if resp.GetSqlAfterRewrite() == "" {
		resp.SqlAfterRewrite = sql
	}
	if siVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		handlers.AnnotateStorageIntegrityRejectAST(e, resp, ast, sql, sel)
	}
}

// sealStorageIntegrityHandlerError closes the last fail-open escape hatch in
// the active SI pipeline. A handler/collector error means the engine could not
// prove the complete statement surface; exposing that as a Go error would make
// legacy callers forward the original SQL. Empty-SI requests retain that legacy
// error channel, while active SI converts it to an ordinary, acknowledged
// UnsupportedStatement response that HouseGate must reject (Spec I D1/D2).
func sealStorageIntegrityHandlerError(
	resp *pb.RewriteSQLResponse,
	ast engine.AST,
	sql string,
	ec pb.ExistenceClause,
	siVersion pb.StorageIntegrityContractVersion,
	e engine.Engine,
	sel nameresolve.Selection,
	handlerErr error,
) (*pb.RewriteSQLResponse, error) {
	if siVersion == pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		return nil, handlerErr
	}
	resp.Code = pb.RewriteCode_UnsupportedStatement
	resp.Message = StorageIntegrityUnmodelledMessage
	finalize(resp, ast, sql, ec, siVersion, e, sel)
	return resp, nil
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

// StorageIntegrityUnmodelledMessage is returned when a request activates the
// storage-integrity surface (a V1 request with a non-empty tables map, or any
// V2 request) and execution reaches the unmodelled-statement pass-through.
// The rewriter cannot prove such a statement is harmless to the
// protocol-owned namespaces, so it refuses to forward it (Spec I D1).
// Enumerated classes replace this text with a more specific one; see
// handlers.AnnotateStorageIntegrityReject.
const StorageIntegrityUnmodelledMessage = "storage-integrity is configured; statement class is not modelled by the rewriter and cannot be forwarded"

// StorageIntegrityContractMessage rejects an active storage-integrity request
// whose contract_version this engine does not implement.
const StorageIntegrityContractMessage = "storage-integrity contract version V1 or V2 is required"

// doRewrite is the engine-level rewrite pipeline shared by NativeRewriter
// (per-connection, options via callback) and Service (stateless, options
// from the request). A non-nil error means an unexpected/internal failure
// the caller should treat as fail-open; rewrite rejections travel inside
// the response Code instead.
func doRewrite(e engine.Engine, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	resp := &pb.RewriteSQLResponse{SqlAfterRewrite: sql} // SQL always set; echoes input
	siVersion := pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED
	selection := nameresolve.FindActive(opts)
	if selection.Mode == nameresolve.ModeDynamic && nameresolve.StorageIntegritySurfaceActive(selection.Dynamic) {
		version := selection.Dynamic.GetStorageIntegrity().GetContractVersion()
		if version != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1 &&
			version != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2 {
			resp.Code = pb.RewriteCode_InvalidRewriteRequest
			resp.Message = StorageIntegrityContractMessage
			return resp, nil
		}
		if err := nameresolve.ValidateStorageIntegrity(selection.Dynamic); err != nil {
			resp.Code = pb.RewriteCode_InvalidRewriteRequest
			resp.Message = err.Error()
			return resp, nil
		}
		siVersion = version
		resp.StorageIntegrityContractVersion = siVersion
	}
	ast, err := e.ParseOne(sql)
	if err != nil {
		resp.Code = pb.RewriteCode_SyntaxError
		resp.Message = err.Error()
		return resp, nil // SyntaxError is a code, not a Go error
	}
	resp.StatementType = classify(ast)

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

	// Polyglot exposes LIVE VIEW spellings (notably a DEFINER prefix) as an
	// ordinary create_view node and sometimes as an opaque raw/command node.
	// The total classifier cheaply excludes unrelated node kinds and opaque
	// literal/comment decoys before it invokes the engine tokenizer. Exact
	// grammar is eligible for D2 object attribution; malformed prefixes and
	// classifier failures stay generic.
	if siVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		liveViewClass, classifyErr := engine.ClassifyLiveView(e, ast, sql)
		if classifyErr != nil || liveViewClass != engine.NotLiveView {
			resp.Code = pb.RewriteCode_UnsupportedStatement
			resp.Message = StorageIntegrityUnmodelledMessage
			finalize(resp, ast, sql, ec, siVersion, e, selection)
			return resp, nil
		}
	}

	// Phase 2: route writes (CREATE/DROP/ALTER/INSERT/UPDATE/DELETE/RENAME/EXCHANGE/
	// views, + bare-rejects, + out-of-phase CREATE/DROP DATABASE) before SELECT.
	if wresp, handled, werr := handlers.RewriteWrite(e, ast, sql, opts); werr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, werr)
	} else if handled {
		// Design §8 + oracle parity: stamp existence_clause; echo input + clear
		// statement_type on reject.
		finalize(wresp, ast, sql, ec, siVersion, e, selection)
		return wresp, nil
	}

	// Phase 3: route db-level statements (USE / SHOW TABLES / SHOW DATABASES /
	// CREATE DATABASE / DROP DATABASE) after writes, before SELECT.
	if dresp, handled, derr := handlers.RewriteDBLevel(e, ast, sql, opts); derr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, derr)
	} else if handled {
		finalize(dresp, ast, sql, ec, siVersion, e, selection)
		return dresp, nil
	}

	// Phase 4a: DESCRIBE (Spec G §4.3 / Spec E D6). Must run before
	// RewriteExistsShowCreate because both read the same tokenized command
	// node; exists.go now ignores VerbDescribe explicitly.
	if dresp, handled, derr := handlers.RewriteDescribe(e, ast, sql, opts); derr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, derr)
	} else if handled {
		finalize(dresp, ast, sql, ec, siVersion, e, selection)
		return dresp, nil
	}

	// Phase 4b: EXISTS / SHOW CREATE (single-target), then GRANT / REVOKE
	// (privilege deltas) — after db-level, before SELECT. Both match only
	// `command` nodes and recognize disjoint verbs, so their relative order is
	// irrelevant; this mirrors the C++ server order (exists → show_create → grant).
	if xresp, handled, xerr := handlers.RewriteExistsShowCreate(e, ast, sql, opts); xerr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, xerr)
	} else if handled {
		finalize(xresp, ast, sql, ec, siVersion, e, selection)
		return xresp, nil
	}
	if gresp, handled, gerr := handlers.RewriteGrant(e, ast, sql, opts); gerr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, gerr)
	} else if handled {
		finalize(gresp, ast, sql, ec, siVersion, e, selection)
		return gresp, nil
	}

	// Phase 1: route SELECT to the real handler; everything else stays pass-through.
	if kind, _ := engine.NodeKind(ast); kind == engine.NodeSelect || kind == engine.NodeUnion ||
		kind == engine.NodeIntersect || kind == engine.NodeExcept {
		hresp, herr := handlers.RewriteSelect(e, ast, opts, sql)
		if herr != nil {
			return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, herr)
		}
		finalize(hresp, ast, sql, ec, siVersion, e, selection) // SELECT never carries IF [NOT] EXISTS → ec stays UNSPECIFIED
		return hresp, nil
	}

	// Pass-through: regenerate (proves the engine round-trips); fall back to
	// the input on any generate hiccup so SQL is always runnable. With an
	// active storage-integrity contract this branch is a refusal instead:
	// reaching it means no handler modelled the statement, so no handler
	// checked it against the protocol-owned namespaces (Spec I D1).
	if siVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		resp.Code = pb.RewriteCode_UnsupportedStatement
		resp.Message = StorageIntegrityUnmodelledMessage
		finalize(resp, ast, sql, ec, siVersion, e, selection)
		return resp, nil
	}
	if gen, gerr := e.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
	resp.Code = pb.RewriteCode_Success
	finalize(resp, ast, sql, ec, siVersion, e, selection)
	return resp, nil
}

func (r *NativeRewriter) Rewrite(_ context.Context, sql, account string) (RewriteResult, error) {
	var opts []*pb.RewriteOption
	if r.options != nil {
		opts = r.options(account)
	}
	resp, err := doRewrite(r.engine, sql, opts)
	if err != nil {
		return RewriteResult{}, err // unexpected/internal → fail-open Go error
	}
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
func classify(ast engine.AST) pb.StatementType {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
	switch kind {
	case engine.NodeSelect, engine.NodeUnion, engine.NodeIntersect, engine.NodeExcept:
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
	case strings.HasPrefix(u, "DESC"):
		return pb.StatementType_STATEMENT_TYPE_DESCRIBE
	case strings.HasPrefix(u, "SHOW DATABASES"), strings.HasPrefix(u, "SHOW SCHEMAS"):
		return pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES
	case strings.HasPrefix(u, "SHOW TABLES"), strings.HasPrefix(u, "SHOW"):
		return pb.StatementType_STATEMENT_TYPE_SHOW_TABLES
	default:
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
}
