package rewriter

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

type snapshotDeadlineEngine struct {
	engine.Engine
	generated     int
	afterGenerate func(int)
}

func (e *snapshotDeadlineEngine) Generate(ast engine.AST) (string, error) {
	sql, err := e.Engine.Generate(ast)
	if err == nil {
		e.generated++
		e.afterGenerate(e.generated)
	}
	return sql, err
}

// This is a private dispatch regression over the actual FFI, not evidence of
// measured membership. Public measured constructors remain covered separately.
func TestSnapshotPrepareWholeDeadline(t *testing.T) {
	lib := os.Getenv("POLYGLOT_SQL_FFI_PATH")
	if lib == "" {
		t.Skip("requires actual FFI")
	}
	e, err := engine.NewPolyglot(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	// Warm the actual parser/generator outside the timed dispatch. A premature
	// refusal is a test failure; both successful Generate calls must be reached.
	ast, err := e.ParseOne("SELECT 7")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Generate(ast); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success_control", "caller_cancel", "profile_deadline"} {
		t.Run(mode, func(t *testing.T) {
			rec := snapshotRecord{Version: 1, ColumnProfileID: "column-profile-v1", OutputOrderID: "output-order-v1",
				Settings:        []snapshotSetting{{"cast_keep_nullable", "0"}, {"max_threads", "1"}, {"read_overflow_mode", "throw"}, {"result_overflow_mode", "throw"}, {"sort_overflow_mode", "throw"}, {"timeout_overflow_mode", "throw"}},
				ScalarOperators: []string{"and", "column", "equals", "greater", "greaterOrEquals", "in", "less", "lessOrEquals", "literal", "not", "notEquals", "or", "prepare-integer-literal-exact-v1", "prepare-integer-scalar-null-throw-v1", "rand", "rand32", "rand64"},
			}
			rec.Limits.MaxSQLBytes, rec.Limits.MaxDescriptorBytes, rec.Limits.MaxExecutionMS = 65536, 65536, 30000
			if mode == "profile_deadline" {
				rec.Limits.MaxExecutionMS = 1000
			}
			raw, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapped := &snapshotDeadlineEngine{Engine: e, afterGenerate: func(n int) {
				if mode == "caller_cancel" && n == 2 {
					cancel()
				}
				// Each stage fits the profile individually; their sum does not.
				// Resetting the budget between analysis and Prepare must also fail.
				if mode == "profile_deadline" {
					time.Sleep(600 * time.Millisecond)
				}
			}}
			r := &pb.PrepareSnapshotQueryRequest{Analysis: &pb.AnalyzeSnapshotQueryRequest{
				ContractVersion: 1, QueryProfileId: "dispatch-fixture", Sql: "INSERT INTO tenant.copy SELECT 7", LogicalDatabase: "tenant",
				Catalog: []*pb.SnapshotQueryCatalogTable{{Database: "tenant", Table: "copy", TableId: "0x" + strings.Repeat("1", 64), SchemaHash: "0x" + strings.Repeat("2", 64), Columns: []*pb.SnapshotQueryColumn{{Name: "value", Type: "Int64", Generation: pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ORDINARY}}}},
			}}
			got := prepareSnapshot(ctx, wrapped, map[string]string{"dispatch-fixture": string(raw)}, r)
			if wrapped.generated != 2 {
				t.Fatalf("Prepare generation not reached: calls=%d response=%v", wrapped.generated, got)
			}
			if mode == "success_control" {
				if got.Code != pb.SnapshotQueryCode_SUCCESS || got.SelectSql != "SELECT accurateCast('7', 'Int64')" {
					t.Fatalf("control: %v", got)
				}
				return
			}
			if got.Code != pb.SnapshotQueryCode_INVALID_INPUT || got.Message != "snapshot query execution deadline exceeded" || got.ContractVersion != 1 || got.QueryProfileId != "dispatch-fixture" || got.SelectSql != "" || got.TargetTableId != "" || len(got.TargetColumns) != 0 || len(got.ReadTableIds) != 0 {
				t.Fatalf("expired Prepare returned executable fields or wrong refusal: %v", got)
			}
			t.Logf("%s reached both actual Generate calls and refused with empty executable fields", mode)
		})
	}
}
