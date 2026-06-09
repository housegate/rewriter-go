package harness

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/housegate/rewriter-go/gen/pb"
)

// OracleAddrEnv names the env var pointing at a running rewriter-grpc service.
const OracleAddrEnv = "REWRITER_ORACLE_ADDR"

// Oracle is a thin gRPC client to the C++ rewriter-grpc service.
type Oracle struct {
	conn   *grpc.ClientConn
	client pb.RewriterServiceClient
}

// DialOracle connects to REWRITER_ORACLE_ADDR. Returns (nil, nil) when the env
// var is unset, so callers can skip oracle-backed comparisons gracefully.
func DialOracle() (*Oracle, error) {
	addr := os.Getenv(OracleAddrEnv)
	if addr == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("harness: dial oracle %s: %w", addr, err)
	}
	return &Oracle{conn: conn, client: pb.NewRewriterServiceClient(conn)}, nil
}

// Rewrite calls the C++ oracle's Rewrite RPC.
func (o *Oracle) Rewrite(sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return o.client.Rewrite(ctx, &pb.RewriteSQLRequest{Sql: sql, Options: opts})
}

// RewriteErrorMessage calls the C++ oracle's RewriteErrorMessage RPC. The C++
// re-runs the rewrite from (sql, opts), so the request carries them directly.
func (o *Oracle) RewriteErrorMessage(sql, errorMessage string, opts []*pb.RewriteOption) (*pb.RewriteErrorMessageResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return o.client.RewriteErrorMessage(ctx, &pb.RewriteErrorMessageRequest{
		Sql: sql, ErrorMessage: errorMessage, Options: opts,
	})
}

// Close releases the gRPC connection.
func (o *Oracle) Close() error {
	if o == nil || o.conn == nil {
		return nil
	}
	return o.conn.Close()
}
