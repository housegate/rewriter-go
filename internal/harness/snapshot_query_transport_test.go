package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Timer cases establish transport interruption and no executable client success,
// not the server AST phase or a promise of one-millisecond preemption. Separate
// in-process tests exercise cancellation after actual Prepare generation.
func TestSnapshotQueryTransport(t *testing.T) {
	if os.Getenv("SNAPSHOT_QUERY_ORDINARY") == "1" {
		t.Skip("explicit ordinary lane; no transport qualification")
	}
	if os.Getenv("SNAPSHOT_RUN_LOG_DIR") == "" || os.Getenv("SNAPSHOT_DEADLINE_PROFILE") == "" {
		t.Fatal("private gate requires transport evidence and diagnostic profile")
	}
	oracle, e := DialOracle()
	if e != nil || oracle == nil {
		t.Fatalf("live oracle required: %v", e)
	}
	defer oracle.Close()
	var diagnostic executionProfile
	raw, e := os.ReadFile(os.Getenv("SNAPSHOT_DEADLINE_PROFILE"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &diagnostic); e != nil || len(diagnostic.Profiles) != 1 {
		t.Fatal("one diagnostic profile required")
	}
	var request *pb.AnalyzeSnapshotQueryRequest
	for _, c := range loadSnapshotCorpus(t).Cases {
		if c.Name == "literal_int64_seven" {
			request = new(pb.AnalyzeSnapshotQueryRequest)
			snapshotDecode(t, c.Request, request)
		}
	}
	if request == nil {
		t.Fatal("missing control corpus request")
	}
	defs := make([]string, 500)
	for i := range defs {
		defs[i] = fmt.Sprintf("c%d AS (SELECT 7 AS value)", i)
	}
	request.Sql = "WITH " + strings.Join(defs, ", ") + " INSERT INTO tenant.copy SELECT 7"
	out, e := os.OpenFile(filepath.Join(os.Getenv("SNAPSHOT_RUN_LOG_DIR"), "transport.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	count := 0
	for _, operation := range []string{"analyze", "prepare"} {
		for _, mode := range []string{"control", "pre_cancel", "expired", "timer_cancel", "timer_deadline", "profile_deadline", "recovery"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				r := proto.Clone(request).(*pb.AnalyzeSnapshotQueryRequest)
				if mode == "profile_deadline" {
					r.QueryProfileId = diagnostic.Profiles[0].Q
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				switch mode {
				case "pre_cancel":
					cancel()
				case "expired":
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					defer cancel()
				case "timer_cancel":
					timer := time.AfterFunc(time.Millisecond, cancel)
					defer timer.Stop()
				case "timer_deadline":
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
					defer cancel()
				}
				start := time.Now()
				var response proto.Message
				var err error
				var code pb.SnapshotQueryCode
				var version uint32
				var q, message, sql, target string
				var columns, reads []string
				var input proto.Message = r
				if operation == "analyze" {
					a, e := oracle.client.AnalyzeSnapshotQuery(ctx, r)
					err = e
					if a != nil {
						response = a
						code = a.Code
						version = a.ContractVersion
						q = a.QueryProfileId
						message = a.Message
						sql = a.SqlAfterMaterialization
						target = a.TargetTableId
						columns = a.TargetColumns
						reads = a.ReadTableIds
					}
				} else {
					p := &pb.PrepareSnapshotQueryRequest{Analysis: r}
					input = p
					a, e := oracle.client.PrepareSnapshotQuery(ctx, p)
					err = e
					if a != nil {
						response = a
						code = a.Code
						version = a.ContractVersion
						q = a.QueryProfileId
						message = a.Message
						sql = a.SelectSql
						target = a.TargetTableId
						columns = a.TargetColumns
						reads = a.ReadTableIds
					}
				}
				record := map[string]any{"name": operation + "/" + mode, "request": json.RawMessage(protojson.Format(input)), "grpc_code": status.Code(err).String(), "elapsed_ns": time.Since(start).Nanoseconds()}
				if response != nil {
					record["response"] = json.RawMessage(protojson.Format(response))
				}
				if err != nil {
					record["error"] = err.Error()
				}
				if e = enc.Encode(record); e != nil {
					t.Fatal(e)
				}
				count++
				empty := sql == "" && target == "" && len(columns) == 0 && len(reads) == 0
				switch mode {
				case "control", "recovery":
					if err != nil || code != pb.SnapshotQueryCode_SUCCESS || version != 1 || q != r.QueryProfileId || sql == "" || target == "" {
						t.Fatalf("control/recovery failed: response=%v error=%v", response, err)
					}
				case "pre_cancel", "timer_cancel":
					if status.Code(err) != codes.Canceled || !empty {
						t.Fatalf("expected Canceled without executable output: %v %v", response, err)
					}
				case "expired", "timer_deadline":
					if status.Code(err) != codes.DeadlineExceeded || !empty {
						t.Fatalf("expected DeadlineExceeded without executable output: %v %v", response, err)
					}
				case "profile_deadline":
					if err != nil || code != pb.SnapshotQueryCode_INVALID_INPUT || version != 1 || q != r.QueryProfileId || message != "snapshot query execution deadline exceeded" || !empty {
						t.Fatalf("expected acknowledged profile deadline refusal: %v %v", response, err)
					}
				}
			})
		}
	}
	if count != 14 {
		t.Fatalf("incomplete transport matrix: %d", count)
	}
	if e = out.Sync(); e != nil {
		t.Fatal(e)
	}
	t.Log("TRANSPORT_QUALIFICATION calls=14")
}
