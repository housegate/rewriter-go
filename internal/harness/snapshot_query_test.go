package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	rewriter "github.com/housegate/rewriter-go"
	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Q is a runtime input: resolved corpus copies must never rebuild this binary.
// This synthetic identity is ONLY for the pre-loader RED checkpoint.
const snapshotRedQ = "0x1111111111111111111111111111111111111111111111111111111111111111"
const snapshotQSlot = "${QUERY_PROFILE_ID}"

type snapshotCase struct {
	Name                    string          `json:"name"`
	Request                 json.RawMessage `json:"request"`
	ExpectedResponse        json.RawMessage `json:"expected_response"`
	PrepareRequest          json.RawMessage `json:"prepare_request,omitempty"`
	ExpectedPrepareResponse json.RawMessage `json:"expected_prepare_response,omitempty"`
}
type snapshotCorpus struct {
	SchemaVersion int            `json:"schema_version"`
	Purpose       string         `json:"purpose"`
	Cases         []snapshotCase `json:"cases"`
}

func loadSnapshotCorpus(t *testing.T) snapshotCorpus {
	t.Helper()
	path := os.Getenv("SNAPSHOT_QUERY_CORPUS")
	if path == "" {
		path = "testdata/snapshot_query_cases.json"
	}
	source, err := os.ReadFile("testdata/snapshot_query_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(source)) != "3d7c1f1077b1ac6c1bd73d7703b135e95c92bde10d1ce4393c8cb091cca1c933" {
		t.Fatal("frozen snapshot source corpus changed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if path != "testdata/snapshot_query_cases.json" {
		profileRaw, er := os.ReadFile(os.Getenv("SNAPSHOT_MEASURED_PROFILE"))
		if er != nil {
			t.Fatal(er)
		}
		var p struct {
			Profiles []struct {
				Q string `json:"query_profile_id"`
			} `json:"profiles"`
		}
		if json.Unmarshal(profileRaw, &p) != nil || len(p.Profiles) != 1 {
			t.Fatal("require one external measured Q")
		}
		expected := bytes.ReplaceAll(source, []byte(snapshotQSlot), []byte(p.Profiles[0].Q))
		if !bytes.Equal(raw, expected) {
			t.Fatal("resolved corpus differs outside frozen profile identity slots")
		}
	}
	var corpus snapshotCorpus
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		t.Fatalf("trailing corpus content: %v", err)
	}
	if corpus.SchemaVersion != 1 || corpus.Purpose == "" || len(corpus.Cases) != 636 {
		t.Fatal("invalid corpus metadata")
	}
	prepares := 0
	names := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty/duplicate case %q", c.Name)
		}
		names[c.Name] = true
		if len(c.PrepareRequest) > 0 {
			prepares++
		}
		if (len(c.PrepareRequest) == 0) != (len(c.ExpectedPrepareResponse) == 0) {
			t.Fatalf("unpaired preparation %s", c.Name)
		}
	}
	if prepares != 130 {
		t.Fatalf("expected 130 Prepare vectors, got %d", prepares)
	}
	return corpus
}
func snapshotDecode(t *testing.T, raw json.RawMessage, out proto.Message) {
	t.Helper()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	// Resolve only typed identity fields, never strings elsewhere in the message.
	switch m := out.(type) {
	case *pb.AnalyzeSnapshotQueryRequest:
		if m.QueryProfileId == snapshotQSlot {
			m.QueryProfileId = snapshotRedQ
		}
	case *pb.AnalyzeSnapshotQueryResponse:
		if m.QueryProfileId == snapshotQSlot {
			m.QueryProfileId = snapshotRedQ
		}
	case *pb.PrepareSnapshotQueryRequest:
		if m.Analysis != nil && m.Analysis.QueryProfileId == snapshotQSlot {
			m.Analysis.QueryProfileId = snapshotRedQ
		}
	case *pb.PrepareSnapshotQueryResponse:
		if m.QueryProfileId == snapshotQSlot {
			m.QueryProfileId = snapshotRedQ
		}
	}
}

// Exact protobuf equality includes acknowledgement, Q, code/category, executable
// SQL, target order and sorted closure. No SQL or actual-profile normalization.
func snapshotCompare(want, got proto.Message) error {
	if !proto.Equal(want, got) {
		return fmt.Errorf("exact response mismatch\nwant: %s\ngot:  %s", want, got)
	}
	return nil
}
func TestSnapshotQueryCorpusContract(t *testing.T) {
	corpus := loadSnapshotCorpus(t)
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var req pb.AnalyzeSnapshotQueryRequest
			var want pb.AnalyzeSnapshotQueryResponse
			snapshotDecode(t, c.Request, &req)
			snapshotDecode(t, c.ExpectedResponse, &want)
			if want.Code == pb.SnapshotQueryCode_UNSPECIFIED {
				t.Fatal("unspecified expected classification")
			}
			if want.Code != pb.SnapshotQueryCode_SUCCESS && (want.SqlAfterMaterialization != "" || want.TargetTableId != "" || len(want.TargetColumns) > 0 || len(want.ReadTableIds) > 0) {
				t.Fatal("non-success has executable output")
			}
			if len(c.PrepareRequest) > 0 {
				var p pb.PrepareSnapshotQueryRequest
				var w pb.PrepareSnapshotQueryResponse
				snapshotDecode(t, c.PrepareRequest, &p)
				snapshotDecode(t, c.ExpectedPrepareResponse, &w)
			}
		})
	}
}
func TestSnapshotQueryComparisonRejectsInvalidClassification(t *testing.T) {
	want := &pb.AnalyzeSnapshotQueryResponse{ContractVersion: 1, QueryProfileId: snapshotRedQ, Code: pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY}
	mutations := map[string]func(*pb.AnalyzeSnapshotQueryResponse){
		"no_ack":                    func(r *pb.AnalyzeSnapshotQueryResponse) { r.ContractVersion = 0 },
		"wrong_version":             func(r *pb.AnalyzeSnapshotQueryResponse) { r.ContractVersion = 2 },
		"missing_profile":           func(r *pb.AnalyzeSnapshotQueryResponse) { r.QueryProfileId = "" },
		"wrong_profile":             func(r *pb.AnalyzeSnapshotQueryResponse) { r.QueryProfileId = "wrong" },
		"unknown_code":              func(r *pb.AnalyzeSnapshotQueryResponse) { r.Code = 127 },
		"missing_code":              func(r *pb.AnalyzeSnapshotQueryResponse) { r.Code = 0 },
		"executable_classification": func(r *pb.AnalyzeSnapshotQueryResponse) { r.SqlAfterMaterialization = "SELECT 7" },
		"target_classification":     func(r *pb.AnalyzeSnapshotQueryResponse) { r.TargetTableId = "unexpected" },
		"columns_classification":    func(r *pb.AnalyzeSnapshotQueryResponse) { r.TargetColumns = []string{"value"} },
		"closure_classification":    func(r *pb.AnalyzeSnapshotQueryResponse) { r.ReadTableIds = []string{"unexpected"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			got := proto.Clone(want).(*pb.AnalyzeSnapshotQueryResponse)
			mutate(got)
			if snapshotCompare(want, got) == nil {
				t.Fatal("accepted invalid classification")
			}
		})
	}
}

type snapshotAnalyzer interface {
	AnalyzeSnapshotQuery(context.Context, *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error)
	PrepareSnapshotQuery(context.Context, *pb.PrepareSnapshotQueryRequest) (*pb.PrepareSnapshotQueryResponse, error)
}

func TestSnapshotQuery(t *testing.T) {
	corpus := loadSnapshotCorpus(t)
	if os.Getenv("SNAPSHOT_QUERY_ORDINARY") == "1" {
		t.Skip("explicit ordinary lane; snapshot qualification requires measured artifacts")
	}
	lib, profile := os.Getenv("SNAPSHOT_MEASURED_FFI"), os.Getenv("SNAPSHOT_MEASURED_PROFILE")
	if lib == "" || profile == "" || os.Getenv("SNAPSHOT_QUERY_CORPUS") == "" {
		t.Fatal("snapshot qualification requires compile -> external generation -> unchanged-binary measured run")
	}
	raw, err := os.ReadFile(os.Getenv("SNAPSHOT_QUERY_CORPUS"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(snapshotQSlot)) {
		t.Fatal("measured corpus contains unresolved profile identity")
	}
	svc, err := rewriter.NewServiceWithSnapshotQueryProfiles(lib, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	native, err := rewriter.NewNativeRewriterWithSnapshotQueryProfiles(lib, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	backends := map[string]snapshotAnalyzer{"service": svc, "native": native}
	oracle, err := DialOracle()
	if err != nil {
		t.Fatal(err)
	}
	if oracle != nil {
		defer oracle.Close()
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var req pb.AnalyzeSnapshotQueryRequest
			var want pb.AnalyzeSnapshotQueryResponse
			snapshotDecode(t, c.Request, &req)
			snapshotDecode(t, c.ExpectedResponse, &want)
			for name, backend := range backends {
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					got, err := backend.AnalyzeSnapshotQuery(ctx, &req)
					if err != nil {
						t.Errorf("Analyze transport: %v", err)
					} else if err := snapshotCompare(&want, got); err != nil {
						t.Error(err)
					}
					if req.Materialize && got != nil && got.Code == pb.SnapshotQueryCode_SUCCESS {
						replay := proto.Clone(&req).(*pb.AnalyzeSnapshotQueryRequest)
						replay.Materialize = false
						replay.Inputs = nil
						replay.Sql = got.SqlAfterMaterialization
						again, err := backend.AnalyzeSnapshotQuery(ctx, replay)
						if err != nil {
							t.Errorf("replay transport: %v", err)
						} else if err = snapshotCompare(got, again); err != nil {
							t.Errorf("signed logical replay changed analysis: %v", err)
						}
					}
					if len(c.PrepareRequest) > 0 {
						var p pb.PrepareSnapshotQueryRequest
						var w pb.PrepareSnapshotQueryResponse
						snapshotDecode(t, c.PrepareRequest, &p)
						snapshotDecode(t, c.ExpectedPrepareResponse, &w)
						g, err := backend.PrepareSnapshotQuery(ctx, &p)
						if err != nil {
							t.Errorf("Prepare transport: %v", err)
						} else if err := snapshotCompare(&w, g); err != nil {
							t.Error(err)
						}
					}
				})
			}
			if oracle != nil {
				t.Run("oracle", func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					got, err := oracle.client.AnalyzeSnapshotQuery(ctx, &req)
					if err != nil {
						t.Fatal(err)
					}
					if err := snapshotCompare(&want, got); err != nil {
						t.Error(err)
					}
					if len(c.PrepareRequest) > 0 {
						var p pb.PrepareSnapshotQueryRequest
						var w pb.PrepareSnapshotQueryResponse
						snapshotDecode(t, c.PrepareRequest, &p)
						snapshotDecode(t, c.ExpectedPrepareResponse, &w)
						g, err := oracle.client.PrepareSnapshotQuery(ctx, &p)
						if err != nil {
							t.Fatal(err)
						}
						if err := snapshotCompare(&w, g); err != nil {
							t.Error(err)
						}
					}
				})
			}
		})
	}
}
