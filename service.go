package rewriter

import (
	"context"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/reverse"
)

// Service is the stateless, process-shared entry point mirroring the
// rewriter-grpc service contract (one call = one request/response, all
// context travels in the request). Safe for concurrent use: the engine
// guards FFI calls with an RWMutex and Service holds no mutable state.
//
// Use Service when embedding the rewriter in a host that already speaks
// the proto contract (e.g. housegate's backend seam); use NativeRewriter
// for the per-connection, callback-driven shape.
//
// ctx is accepted for interface symmetry with the gRPC client but is not
// consulted mid-call: polyglot FFI calls cannot be interrupted. Calls are
// local and fast (no network), so timeouts effectively never fire.
type Service struct {
	engine engine.Engine
}

// NewService loads the polyglot FFI library and returns a ready Service.
// libPath == "" falls back to polyglot's default resolution
// (POLYGLOT_SQL_FFI_PATH, then standard install locations). The Service
// owns the engine; Close releases it.
func NewService(libPath string) (*Service, error) {
	e, err := engine.NewPolyglot(libPath)
	if err != nil {
		return nil, err
	}
	return &Service{engine: e}, nil
}

// Rewrite runs the shared doRewrite pipeline with the options carried in
// the request. Rejections travel in resp.Code; a non-nil error means an
// internal failure the caller should treat as fail-open.
func (s *Service) Rewrite(_ context.Context, req *pb.RewriteSQLRequest) (*pb.RewriteSQLResponse, error) {
	return doRewrite(s.engine, req.GetSql(), req.GetOptions())
}

// RewriteErrorMessage inverts physical names in a ClickHouse error message
// back to the logical names the client used. Stateless: the forward maps
// are re-derived by re-running the rewrite on req.Sql + req.Options (error
// paths are rare; one extra parse per exception is acceptable). When the
// forward rewrite is non-Success — or sql/message is empty — the message
// passes through unchanged, mirroring NativeRewriter's non-Success
// passthrough and the C++ doRewriteErrorMessage semantics.
func (s *Service) RewriteErrorMessage(_ context.Context, req *pb.RewriteErrorMessageRequest) (*pb.RewriteErrorMessageResponse, error) {
	msg := req.GetErrorMessage()
	out := &pb.RewriteErrorMessageResponse{Code: pb.RewriteCode_Success, ErrorAfterRewrite: msg}
	if msg == "" || req.GetSql() == "" {
		return out, nil
	}
	resp, err := doRewrite(s.engine, req.GetSql(), req.GetOptions())
	if err != nil || resp.GetCode() != pb.RewriteCode_Success {
		return out, nil
	}
	out.ErrorAfterRewrite = reverse.Invert(msg, req.GetSql(), resp.GetSqlAfterRewrite(), resp.GetTableRewrites(), resp.GetDatabaseRewrites())
	return out, nil
}

// Close releases the engine. Safe to call once; the engine's own Close is
// idempotent.
func (s *Service) Close() error {
	return s.engine.Close()
}
