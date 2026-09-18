//go:build linux

package rewriter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func snapshotMeasuredRequest(q string) *pb.AnalyzeSnapshotQueryRequest {
	return &pb.AnalyzeSnapshotQueryRequest{ContractVersion: 1, QueryProfileId: q, Sql: "INSERT INTO tenant.copy SELECT 7", LogicalDatabase: "tenant", Catalog: []*pb.SnapshotQueryCatalogTable{{Database: "tenant", Table: "copy", TableId: "0x" + strings.Repeat("1", 64), SchemaHash: "0x" + strings.Repeat("2", 64), Columns: []*pb.SnapshotQueryColumn{{Name: "value", Type: "Int64", Generation: pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ORDINARY}}}}}
}
func TestSnapshotIndependentMeasuredOwnership(t *testing.T) {
	lib, path, f := measuredProfileInputs(t)
	q := f.Profiles[0].QueryProfileID
	s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := NewNativeRewriterWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	req := snapshotMeasuredRequest(q)
	ctx := context.Background()
	for _, b := range []interface {
		AnalyzeSnapshotQuery(context.Context, *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error)
	}{s, n} {
		a, err := b.AnalyzeSnapshotQuery(ctx, req)
		if err != nil || a.Code != pb.SnapshotQueryCode_SUCCESS {
			t.Fatalf("measured analysis: %v %v", a, err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := n.AnalyzeSnapshotQuery(ctx, req)
	if err != nil || a.Code != pb.SnapshotQueryCode_SUCCESS {
		t.Fatalf("Service Close invalidated independent native: %v %v", a, err)
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = n.AnalyzeSnapshotQuery(ctx, req)
	if err != nil || a.Code == pb.SnapshotQueryCode_SUCCESS || a.SqlAfterMaterialization != "" {
		t.Fatalf("closed native succeeded: %v %v", a, err)
	}
	t.Log("independent measured Service/Native ownership, close, post-close refusal PASS")
}
func TestSnapshotMeasuredSemanticSupport(t *testing.T) {
	lib, _, original := measuredProfileInputs(t)
	for _, kind := range []string{"missing_setting", "wrong_setting", "unknown_setting", "missing_operator", "unknown_operator", "unsupported_column_profile", "unsupported_limit"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(original)
			var f snapshotProfileFile
			json.Unmarshal(raw, &f)
			r := &f.Profiles[0].Record
			switch kind {
			case "missing_setting":
				r.Settings = r.Settings[1:]
			case "wrong_setting":
				r.Settings[0].Value = "1"
			case "unknown_setting":
				r.Settings = append(r.Settings, snapshotSetting{"unknown_setting", "0"})
			case "missing_operator":
				r.ScalarOperators = r.ScalarOperators[1:]
			case "unknown_operator":
				r.ScalarOperators = append(r.ScalarOperators, "unknown_operator")
			case "unsupported_column_profile":
				r.ColumnProfileID = "unsupported"
			case "unsupported_limit":
				r.Limits.MaxSQLBytes = 65537
			}
			f.Profiles[0].QueryProfileID = snapshotCanonicalDigest("snapshot-query-profile-v1", *r)
			raw, _ = json.Marshal(f)
			path := filepath.Join(t.TempDir(), "profiles.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			n, err := NewNativeRewriterWithSnapshotQueryProfiles(lib, path)
			if err != nil {
				t.Fatal(err)
			}
			defer n.Close()
			for name, api := range map[string]interface {
				AnalyzeSnapshotQuery(context.Context, *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error)
				PrepareSnapshotQuery(context.Context, *pb.PrepareSnapshotQueryRequest) (*pb.PrepareSnapshotQueryResponse, error)
			}{"service": s, "native": n} {
				t.Run(name, func(t *testing.T) {
					req := snapshotMeasuredRequest(f.Profiles[0].QueryProfileID)
					a, err := api.AnalyzeSnapshotQuery(context.Background(), req)
					if err != nil || a.Code != pb.SnapshotQueryCode_PROFILE_UNAVAILABLE || a.ContractVersion != 0 || a.QueryProfileId != "" || a.SqlAfterMaterialization != "" || a.TargetTableId != "" || len(a.TargetColumns) != 0 || len(a.ReadTableIds) != 0 {
						t.Fatalf("unsupported measured record acknowledged: %v %v", a, err)
					}
					p, err := api.PrepareSnapshotQuery(context.Background(), &pb.PrepareSnapshotQueryRequest{Analysis: req})
					if err != nil || p.Code != pb.SnapshotQueryCode_PROFILE_UNAVAILABLE || p.ContractVersion != 0 || p.QueryProfileId != "" || p.SelectSql != "" || p.TargetTableId != "" || len(p.TargetColumns) != 0 || len(p.ReadTableIds) != 0 {
						t.Fatalf("unsupported Prepare record acknowledged: %v %v", p, err)
					}
				})
			}
		})
	}
}
func TestSnapshotMeasuredResourceRefusals(t *testing.T) {
	lib, path, f := measuredProfileInputs(t)
	s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q := f.Profiles[0].QueryProfileID
	for _, kind := range []string{"sql_bytes", "descriptor_bytes", "depth", "cancelled", "nil_request"} {
		t.Run(kind, func(t *testing.T) {
			r := snapshotMeasuredRequest(q)
			ctx := context.Background()
			switch kind {
			case "sql_bytes":
				r.Sql = strings.Repeat(" ", 65537)
			case "descriptor_bytes":
				r.Catalog[0].Columns[0].DefaultExpression = strings.Repeat("x", 65537)
			case "depth":
				r.Sql = "INSERT INTO tenant.copy SELECT " + strings.Repeat("(", 1024) + "7" + strings.Repeat(")", 1024)
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "nil_request":
				r = nil
			}
			a, err := s.AnalyzeSnapshotQuery(ctx, r)
			if err != nil || a.Code == pb.SnapshotQueryCode_SUCCESS || a.Code == pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY || a.SqlAfterMaterialization != "" || a.TargetTableId != "" || len(a.TargetColumns) != 0 || len(a.ReadTableIds) != 0 {
				t.Fatalf("resource refusal leaked success: %v %v", a, err)
			}
		})
	}
}

func TestSnapshotMeasuredPrepareDescriptorBound(t *testing.T) {
	lib, path, f := measuredProfileInputs(t)
	s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := &pb.PrepareSnapshotQueryRequest{Analysis: snapshotMeasuredRequest(f.Profiles[0].QueryProfileID), Bindings: []*pb.SnapshotScratchBinding{{TableId: strings.Repeat("x", 65537)}}}
	a, err := s.PrepareSnapshotQuery(context.Background(), r)
	if err != nil || a.Code != pb.SnapshotQueryCode_INVALID_INPUT || a.Message != "snapshot query descriptor exceeds the profile limit" || a.SelectSql != "" || a.TargetTableId != "" || len(a.ReadTableIds) != 0 || len(a.TargetColumns) != 0 {
		t.Fatalf("Prepare descriptor: %v %v", a, err)
	}
}
