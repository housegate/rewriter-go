package rewriter

import (
	"context"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"google.golang.org/protobuf/proto"
)

func TestServiceMaterializeSQL_rewritesSupportedFunctions(t *testing.T) {
	// Given
	e := newEngine(t)
	svc := &Service{engine: e}
	nowUnixNS := int64(1710000000123456789)

	// When
	resp, err := svc.MaterializeSQL(context.Background(), &pb.MaterializeSQLRequest{
		Sql: "INSERT INTO db.t VALUES (now(), rand(), generateUUIDv4())",
		Inputs: &pb.MaterializationInputs{
			NowUnixNs:          proto.Int64(nowUnixNS),
			RandomUint64Values: []uint64{7},
			UuidValues:         []string{"018f3c8a-3e8a-7d0a-b5b0-6e95e3f0b001"},
		},
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeSQL: %v", err)
	}
	if resp.GetCode() != pb.MaterializeCode_MaterializeSuccess {
		t.Fatalf("code = %v, want MaterializeSuccess: %s", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetSqlAfterMaterialization() == "" || resp.GetSqlAfterMaterialization() == resp.GetSql() {
		t.Fatalf("sql_after_materialization = %q", resp.GetSqlAfterMaterialization())
	}
	for _, fn := range []string{"now()", "rand()", "generateUUIDv4()"} {
		if strings.Contains(resp.GetSqlAfterMaterialization(), fn) {
			t.Fatalf("materialized SQL still contains %s: %s", fn, resp.GetSqlAfterMaterialization())
		}
	}
	if len(resp.GetReplacements()) != 3 {
		t.Fatalf("replacements = %d, want 3: %+v", len(resp.GetReplacements()), resp.GetReplacements())
	}
}

func TestServiceMaterializeSQL_rewritesAdditionalSafeScalarFunctions(t *testing.T) {
	// Given
	e := newEngine(t)
	svc := &Service{engine: e}
	nowUnixNS := int64(1710000000123456789)

	// When
	resp, err := svc.MaterializeSQL(context.Background(), &pb.MaterializeSQLRequest{
		Sql: "INSERT INTO db.t VALUES (now64(), today(), CURRENT_DATE, curdate(), yesterday(), UTCTimestamp(), CURRENT_TIMESTAMP, localtimestamp, localtime, rand32(), randCanonical())",
		Inputs: &pb.MaterializationInputs{
			NowUnixNs:           proto.Int64(nowUnixNS),
			RandomUint64Values:  []uint64{13},
			RandomFloat64Values: []float64{0.125},
		},
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeSQL: %v", err)
	}
	if resp.GetCode() != pb.MaterializeCode_MaterializeSuccess {
		t.Fatalf("code = %v, want MaterializeSuccess: %s", resp.GetCode(), resp.GetMessage())
	}
	materialized := strings.ToLower(resp.GetSqlAfterMaterialization())
	for _, fn := range []string{"now64", "today", "current_date", "curdate", "yesterday", "utctimestamp", "current_timestamp", "localtimestamp", "localtime", "rand32", "randcanonical"} {
		if strings.Contains(materialized, fn) {
			t.Fatalf("materialized SQL still contains %s: %s", fn, resp.GetSqlAfterMaterialization())
		}
	}
	if len(resp.GetReplacements()) != 11 {
		t.Fatalf("replacements = %d, want 11: %+v", len(resp.GetReplacements()), resp.GetReplacements())
	}
}

func TestServiceMaterializeSQL_passesThroughDeterministicSQL(t *testing.T) {
	// Given
	e := newEngine(t)
	svc := &Service{engine: e}
	sql := "INSERT INTO db.t VALUES (1)"

	// When
	resp, err := svc.MaterializeSQL(context.Background(), &pb.MaterializeSQLRequest{Sql: sql})

	// Then
	if err != nil {
		t.Fatalf("MaterializeSQL: %v", err)
	}
	if resp.GetCode() != pb.MaterializeCode_MaterializeSuccess {
		t.Fatalf("code = %v, want MaterializeSuccess: %s", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetSqlAfterMaterialization() != sql {
		t.Fatalf("sql_after_materialization = %q, want %q", resp.GetSqlAfterMaterialization(), sql)
	}
}

func TestServiceMaterializeSQL_failsClosedWhenInputMissing(t *testing.T) {
	// Given
	e := newEngine(t)
	svc := &Service{engine: e}

	// When
	resp, err := svc.MaterializeSQL(context.Background(), &pb.MaterializeSQLRequest{
		Sql: "INSERT INTO db.t VALUES (rand())",
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeSQL: %v", err)
	}
	if resp.GetCode() != pb.MaterializeCode_MaterializeInvalidRequest {
		t.Fatalf("code = %v, want MaterializeInvalidRequest", resp.GetCode())
	}
	if resp.GetSqlAfterMaterialization() != resp.GetSql() {
		t.Fatalf("sql_after_materialization = %q, want original %q", resp.GetSqlAfterMaterialization(), resp.GetSql())
	}
}

func TestServiceMaterializeSQL_failsClosedWhenKnownVolatileUnsupported(t *testing.T) {
	// Given
	e := newEngine(t)
	svc := &Service{engine: e}

	// When
	resp, err := svc.MaterializeSQL(context.Background(), &pb.MaterializeSQLRequest{
		Sql: "INSERT INTO db.t VALUES (generateUUIDv7())",
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeSQL: %v", err)
	}
	if resp.GetCode() != pb.MaterializeCode_MaterializeUnsupportedStatement {
		t.Fatalf("code = %v, want MaterializeUnsupportedStatement: %s", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetSqlAfterMaterialization() != resp.GetSql() {
		t.Fatalf("sql_after_materialization = %q, want original %q", resp.GetSqlAfterMaterialization(), resp.GetSql())
	}
}
