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

// sealStorageIntegrityHandlerError closes the fail-open escape hatch a Go
// error would otherwise be. A handler/collector/generator error (including a
// polyglot recursion-limit error) means the engine could not prove the
// complete statement surface; exposing that as a Go error would make legacy
// callers forward the original SQL. Every dynamic-mode request therefore
// converts it to an ordinary UnsupportedStatement response HouseGate must
// reject: with the SI surface active the message is the SI catch-all (Spec I
// D1/D2), otherwise the table-reference policy's "statement is not
// supported" (spec 2026-09-26 §5). Static and no-rewrite requests keep the
// legacy error channel.
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
		if sel.Mode != nameresolve.ModeDynamic {
			return nil, handlerErr
		}
		resp.Code = pb.RewriteCode_UnsupportedStatement
		resp.Message = engine.UnsupportedStatementMessage
		finalize(resp, ast, sql, ec, siVersion, e, sel)
		return resp, nil
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

// StorageIntegrityUnmodelledMessage is the SI-active refusal of an
// unmodelled statement class (nameresolve.StorageIntegrityUnmodelledMessage).
// Enumerated classes replace this text with a more specific one; see
// handlers.AnnotateStorageIntegrityReject.
const StorageIntegrityUnmodelledMessage = nameresolve.StorageIntegrityUnmodelledMessage

// StorageIntegrityContractMessage rejects an active storage-integrity request
// whose contract_version this engine does not implement.
const StorageIntegrityContractMessage = "storage-integrity contract version V1 or V2 is required"

// doRewrite is the engine-level rewrite pipeline shared by NativeRewriter
// (per-connection, options via callback) and Service (stateless, options
// from the request). A non-nil error means an unexpected/internal failure on
// a static or no-rewrite request; a dynamic-mode request never returns one
// (sealStorageIntegrityHandlerError turns it into an UnsupportedStatement
// response). Rewrite rejections travel inside the response Code.
//
// CREATE TABLE … EMPTY AS SELECT is rewritten without its EMPTY keyword,
// which polyglot cannot parse, and the keyword is put back into the result;
// a rejection echoes the caller's SQL (spec 2026-09-26 §5).
func doRewrite(e engine.Engine, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	stripped, empty, stripErr := engine.StripCreateTableEmpty(e, sql)
	if stripErr == nil && !empty {
		return rewriteStatement(e, sql, opts)
	}
	text := sql
	if empty {
		text = stripped
	}
	resp, err := rewriteStatement(e, text, opts)
	if err != nil {
		return nil, err
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		resp.SqlAfterRewrite = sql // a rejection echoes the caller's SQL
		return resp, nil
	}
	if stripErr == nil {
		out, insertErr := engine.InsertCreateTableEmpty(e, resp.GetSqlAfterRewrite())
		if insertErr == nil {
			resp.SqlAfterRewrite = out
			return resp, nil
		}
		stripErr = insertErr
	}
	return sealCreateTableEmpty(resp, sql, opts, stripErr)
}

// sealCreateTableEmpty refuses a CREATE TABLE … EMPTY AS SELECT whose EMPTY
// keyword could not be located or put back, instead of forwarding a statement
// without its body (spec 2026-09-26 §5: an engine-internal limit is a coded
// UnsupportedStatement in dynamic mode). Static and no-rewrite requests keep
// the legacy Go-error channel.
func sealCreateTableEmpty(resp *pb.RewriteSQLResponse, sql string, opts []*pb.RewriteOption, cause error) (*pb.RewriteSQLResponse, error) {
	if nameresolve.FindActive(opts).Mode != nameresolve.ModeDynamic {
		return nil, cause
	}
	msg := engine.UnsupportedStatementMessage
	if resp.GetStorageIntegrityContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		msg = StorageIntegrityUnmodelledMessage
	}
	return &pb.RewriteSQLResponse{
		SqlAfterRewrite:                 sql,
		Code:                            pb.RewriteCode_UnsupportedStatement,
		Message:                         msg,
		ExistenceClause:                 resp.GetExistenceClause(),
		StorageIntegrityContractVersion: resp.GetStorageIntegrityContractVersion(),
	}, nil
}

// rewriteStatement is the single-statement pipeline doRewrite runs once the
// CREATE TABLE … EMPTY form has been normalised.
func rewriteStatement(e engine.Engine, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
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
	if selection.Mode == nameresolve.ModeDynamic {
		if err := nameresolve.ValidateProtectedDatabases(selection.Dynamic); err != nil {
			resp.Code = pb.RewriteCode_InvalidRewriteRequest
			resp.Message = err.Error()
			return resp, nil
		}
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

	// Table-reference policy (spec 2026-09-26 §5): identifier parameters and
	// protected databases are refused before any handler can rewrite them.
	if presp, handled, perr := handlers.PreflightTableReferences(e, ast, sql, opts); perr != nil {
		return sealStorageIntegrityHandlerError(resp, ast, sql, ec, siVersion, e, selection, perr)
	} else if handled {
		finalize(presp, ast, sql, ec, siVersion, e, selection)
		return presp, nil
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

	// Spec 2026-09-26 T7: no handler modelled the statement. Without the SI
	// surface this used to pass through as Success; every unmodelled class is
	// now refused, except a session SET, which names no table and which
	// clients send routinely. Under an active SI surface SET stays refused
	// (H6), so the carve-out is inside the inactive branch only.
	if siVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		resp.Code = pb.RewriteCode_UnsupportedStatement
		resp.Message = StorageIntegrityUnmodelledMessage
		finalize(resp, ast, sql, ec, siVersion, e, selection)
		return resp, nil
	}
	if selection.Mode == nameresolve.ModeDynamic {
		isSet, refused, code, msg := sessionSet(e, ast)
		if !isSet || refused {
			resp.Code = pb.RewriteCode_UnsupportedStatement
			resp.Message = engine.UnsupportedStatementMessage
			if refused {
				resp.Code, resp.Message = code, msg
			}
			finalize(resp, ast, sql, ec, siVersion, e, selection)
			return resp, nil
		}
	}
	if gen, gerr := e.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
	resp.Code = pb.RewriteCode_Success
	finalize(resp, ast, sql, ec, siVersion, e, selection)
	return resp, nil
}

// sessionSet classifies a top-level session SET statement for the T7
// carve-out (spec 2026-09-26 T7, R5). Measured 2026-09-26: the pinned
// polyglot renders `SET max_threads = 1` as {"command": {"this": "SET
// max_threads = 1"}}, so the check reads the command text. Only a settings
// assignment qualifies — SET ROLE r1 and SET DEFAULT ROLE r1 TO u1 are
// access-management statements this repo does not model (isSet=false) — and
// the carve-out admits it only when every assignment is `<name> = <value>`
// with a numeric literal, string literal or bare identifier / keyword value
// and no SQL-bearing setting name (refused=true otherwise, with the code and
// message to return).
func sessionSet(e engine.Engine, ast engine.AST) (isSet, refused bool, code pb.RewriteCode, msg string) {
	kind, _ := engine.NodeKind(ast)
	if kind != engine.NodeCommand {
		return false, false, pb.RewriteCode_Success, ""
	}
	text, _ := engine.CommandSQL(ast)
	return handlers.CheckSessionSet(e, text)
}

func (r *NativeRewriter) Rewrite(_ context.Context, sql, account string) (RewriteResult, error) {
	var opts []*pb.RewriteOption
	if r.options != nil {
		opts = r.options(account)
	}
	resp, err := doRewrite(r.engine, sql, opts)
	if err != nil {
		return RewriteResult{}, err // static/no-rewrite internal failure → legacy Go error
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
