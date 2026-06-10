// Command rewrite runs a single SQL statement through the NativeRewriter and
// prints the response as protojson — the same request/response shape as the
// rewriter-grpc HTTP debug endpoint, so its curl bodies work verbatim:
//
//	POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib \
//	  go run ./cmd/rewrite -d '{"sql": "select 1", "options": []}'
//
// The request is read from -d, or from stdin when -d is absent.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/encoding/protojson"

	rewriter "github.com/housegate/rewriter-go"
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

func main() {
	data := flag.String("d", "", "protojson RewriteSQLRequest (default: read stdin)")
	account := flag.String("account", "", "effective account passed to Rewrite")
	flag.Parse()

	body := []byte(*data)
	if *data == "" {
		var err error
		if body, err = io.ReadAll(os.Stdin); err != nil {
			fatal("read stdin:", err)
		}
	}

	var req pb.RewriteSQLRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		fatal("parse request:", err)
	}

	e, err := engine.NewPolyglot("")
	if err != nil {
		fatal("open engine (is POLYGLOT_SQL_FFI_PATH set? run `make ffi`):", err)
	}
	defer e.Close()

	r := rewriter.New(e, rewriter.WithOptions(func(string) []*pb.RewriteOption { return req.GetOptions() }))
	res, err := r.Rewrite(context.Background(), req.GetSql(), *account)
	if err != nil {
		fatal("rewrite:", err)
	}

	out, err := protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(&pb.RewriteSQLResponse{
		Code:                   res.Code,
		Message:                res.Message,
		StatementType:          res.StatementType,
		SqlAfterRewrite:        res.SQL,
		TableRewrites:          res.TableRewrites,
		DatabaseRewrites:       res.DatabaseRewrites,
		OriginalAccessedTables: res.OriginalAccessedTables,
		PrivilegesDeltas:       res.PrivilegesDeltas,
		ExistenceClause:        res.ExistenceClause,
		FailedCteAliases:       res.FailedCTEAliases,
	})
	if err != nil {
		fatal("marshal response:", err)
	}
	fmt.Println(string(out))
}

func fatal(msg string, err error) {
	fmt.Fprintln(os.Stderr, msg, err)
	os.Exit(1)
}
