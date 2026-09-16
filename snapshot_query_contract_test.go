package rewriter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type snapshotQueryClientContract interface {
	AnalyzeSnapshotQuery(context.Context, *pb.AnalyzeSnapshotQueryRequest, ...grpc.CallOption) (*pb.AnalyzeSnapshotQueryResponse, error)
	PrepareSnapshotQuery(context.Context, *pb.PrepareSnapshotQueryRequest, ...grpc.CallOption) (*pb.PrepareSnapshotQueryResponse, error)
}

var _ snapshotQueryClientContract = (pb.RewriterServiceClient)(nil)

type snapshotFieldContract struct {
	name        protoreflect.Name
	number      protoreflect.FieldNumber
	kind        protoreflect.Kind
	cardinality protoreflect.Cardinality
	typeName    protoreflect.FullName
}

func assertSnapshotMessageContract(t *testing.T, message proto.Message, want []snapshotFieldContract) {
	t.Helper()
	fields := message.ProtoReflect().Descriptor().Fields()
	if fields.Len() != len(want) {
		t.Fatalf("%s field count = %d, want %d", message.ProtoReflect().Descriptor().FullName(), fields.Len(), len(want))
	}
	for i, expected := range want {
		field := fields.Get(i)
		if field.Name() != expected.name || field.Number() != expected.number || field.Kind() != expected.kind || field.Cardinality() != expected.cardinality {
			t.Errorf("%s field[%d] = (%s, %d, %s, %s), want (%s, %d, %s, %s)",
				message.ProtoReflect().Descriptor().FullName(), i,
				field.Name(), field.Number(), field.Kind(), field.Cardinality(),
				expected.name, expected.number, expected.kind, expected.cardinality)
		}
		var gotType protoreflect.FullName
		switch field.Kind() {
		case protoreflect.MessageKind:
			gotType = field.Message().FullName()
		case protoreflect.EnumKind:
			gotType = field.Enum().FullName()
		}
		if gotType != expected.typeName {
			t.Errorf("%s.%s type = %q, want %q", message.ProtoReflect().Descriptor().FullName(), field.Name(), gotType, expected.typeName)
		}
	}
}

func TestSnapshotQueryGeneratedMessageContracts(t *testing.T) {
	optional := protoreflect.Optional
	repeated := protoreflect.Repeated
	assertSnapshotMessageContract(t, &pb.SnapshotQueryColumn{}, []snapshotFieldContract{
		{"name", 1, protoreflect.StringKind, optional, ""},
		{"type", 2, protoreflect.StringKind, optional, ""},
		{"generation", 3, protoreflect.EnumKind, optional, "rewriter.SnapshotQueryColumnGeneration"},
		{"default_expression", 4, protoreflect.StringKind, optional, ""},
	})
	assertSnapshotMessageContract(t, &pb.SnapshotQueryCatalogTable{}, []snapshotFieldContract{
		{"database", 1, protoreflect.StringKind, optional, ""},
		{"table", 2, protoreflect.StringKind, optional, ""},
		{"table_id", 3, protoreflect.StringKind, optional, ""},
		{"schema_hash", 4, protoreflect.StringKind, optional, ""},
		{"columns", 5, protoreflect.MessageKind, repeated, "rewriter.SnapshotQueryColumn"},
	})
	assertSnapshotMessageContract(t, &pb.AnalyzeSnapshotQueryRequest{}, []snapshotFieldContract{
		{"contract_version", 1, protoreflect.Uint32Kind, optional, ""},
		{"query_profile_id", 2, protoreflect.StringKind, optional, ""},
		{"sql", 3, protoreflect.StringKind, optional, ""},
		{"logical_database", 4, protoreflect.StringKind, optional, ""},
		{"catalog", 5, protoreflect.MessageKind, repeated, "rewriter.SnapshotQueryCatalogTable"},
		{"materialize", 6, protoreflect.BoolKind, optional, ""},
		{"inputs", 7, protoreflect.MessageKind, optional, "rewriter.MaterializationInputs"},
	})
	assertSnapshotMessageContract(t, &pb.AnalyzeSnapshotQueryResponse{}, []snapshotFieldContract{
		{"contract_version", 1, protoreflect.Uint32Kind, optional, ""},
		{"query_profile_id", 2, protoreflect.StringKind, optional, ""},
		{"code", 3, protoreflect.EnumKind, optional, "rewriter.SnapshotQueryCode"},
		{"message", 4, protoreflect.StringKind, optional, ""},
		{"sql_after_materialization", 5, protoreflect.StringKind, optional, ""},
		{"target_table_id", 6, protoreflect.StringKind, optional, ""},
		{"target_columns", 7, protoreflect.StringKind, repeated, ""},
		{"read_table_ids", 8, protoreflect.StringKind, repeated, ""},
	})
	assertSnapshotMessageContract(t, &pb.SnapshotScratchBinding{}, []snapshotFieldContract{
		{"table_id", 1, protoreflect.StringKind, optional, ""},
		{"scratch_database", 2, protoreflect.StringKind, optional, ""},
		{"scratch_table", 3, protoreflect.StringKind, optional, ""},
	})
	assertSnapshotMessageContract(t, &pb.PrepareSnapshotQueryRequest{}, []snapshotFieldContract{
		{"analysis", 1, protoreflect.MessageKind, optional, "rewriter.AnalyzeSnapshotQueryRequest"},
		{"bindings", 2, protoreflect.MessageKind, repeated, "rewriter.SnapshotScratchBinding"},
	})
	assertSnapshotMessageContract(t, &pb.PrepareSnapshotQueryResponse{}, []snapshotFieldContract{
		{"contract_version", 1, protoreflect.Uint32Kind, optional, ""},
		{"query_profile_id", 2, protoreflect.StringKind, optional, ""},
		{"code", 3, protoreflect.EnumKind, optional, "rewriter.SnapshotQueryCode"},
		{"message", 4, protoreflect.StringKind, optional, ""},
		{"select_sql", 5, protoreflect.StringKind, optional, ""},
		{"target_table_id", 6, protoreflect.StringKind, optional, ""},
		{"target_columns", 7, protoreflect.StringKind, repeated, ""},
		{"read_table_ids", 8, protoreflect.StringKind, repeated, ""},
	})
}

func TestSnapshotQueryGeneratedEnumsAndRPCSignatures(t *testing.T) {
	queryCodes := []pb.SnapshotQueryCode{
		pb.SnapshotQueryCode_UNSPECIFIED,
		pb.SnapshotQueryCode_SUCCESS,
		pb.SnapshotQueryCode_UNSUPPORTED,
		pb.SnapshotQueryCode_INVALID_INPUT,
		pb.SnapshotQueryCode_PROFILE_UNAVAILABLE,
		pb.SnapshotQueryCode_MATERIALIZATION_FAILED,
		pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY,
	}
	for number, code := range queryCodes {
		if int32(code) != int32(number) {
			t.Errorf("SnapshotQueryCode[%d] = %d", number, code)
		}
	}
	generations := []pb.SnapshotQueryColumnGeneration{
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_UNSPECIFIED,
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ORDINARY,
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_DEFAULT,
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_MATERIALIZED,
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ALIAS,
		pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_OTHER,
	}
	for number, generation := range generations {
		if int32(generation) != int32(number) {
			t.Errorf("SnapshotQueryColumnGeneration[%d] = %d", number, generation)
		}
	}

	service := pb.File_rewriter_proto.Services().ByName("RewriterService")
	for _, expected := range []struct {
		name, input, output protoreflect.FullName
	}{
		{"AnalyzeSnapshotQuery", "rewriter.AnalyzeSnapshotQueryRequest", "rewriter.AnalyzeSnapshotQueryResponse"},
		{"PrepareSnapshotQuery", "rewriter.PrepareSnapshotQueryRequest", "rewriter.PrepareSnapshotQueryResponse"},
	} {
		method := service.Methods().ByName(protoreflect.Name(expected.name))
		if method == nil || method.Input().FullName() != expected.input || method.Output().FullName() != expected.output || method.IsStreamingClient() || method.IsStreamingServer() {
			t.Errorf("%s generated signature = %v, want unary %s -> %s", expected.name, method, expected.input, expected.output)
		}
	}
}

type snapshotContractFixture struct {
	SchemaVersion          uint32          `json:"schema_version"`
	FixturePurpose         string          `json:"fixture_purpose"`
	AnalyzeRequest         json.RawMessage `json:"analyze_request"`
	AnalyzeResponse        json.RawMessage `json:"analyze_response"`
	PrepareRequest         json.RawMessage `json:"prepare_request"`
	PrepareResponse        json.RawMessage `json:"prepare_response"`
	MetadataTransportTable json.RawMessage `json:"metadata_transport_table"`
}

func loadSnapshotContractFixture(t *testing.T) snapshotContractFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot_query_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fixture snapshotContractFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&json.RawMessage{}); err != io.EOF {
		t.Fatalf("fixture must contain exactly one JSON value: %v", err)
	}
	if fixture.SchemaVersion != 1 || fixture.FixturePurpose == "" {
		t.Fatalf("fixture metadata = (%d, %q)", fixture.SchemaVersion, fixture.FixturePurpose)
	}
	return fixture
}

func unmarshalSnapshotJSON(t *testing.T, raw json.RawMessage, message proto.Message) {
	t.Helper()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, message); err != nil {
		t.Fatal(err)
	}
}

func assertSnapshotJSONKeys(t *testing.T, raw json.RawMessage, want ...string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want exact protobuf snake_case keys %v", got, want)
	}
	return object
}

func assertSnapshotJSONMessages(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func assertSnapshotBinaryRoundTrip(t *testing.T, original, decoded proto.Message) {
	t.Helper()
	wire, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(wire, decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(original, decoded) {
		t.Fatalf("binary round trip mismatch:\noriginal: %v\ndecoded: %v", original, decoded)
	}
}

func TestSnapshotQuerySharedTransportFixture(t *testing.T) {
	fixture := loadSnapshotContractFixture(t)
	analysisJSON := assertSnapshotJSONKeys(t, fixture.AnalyzeRequest,
		"contract_version", "query_profile_id", "sql", "logical_database", "catalog", "materialize", "inputs")
	assertSnapshotJSONKeys(t, analysisJSON["inputs"],
		"now_unix_ns", "random_uint64_values", "uuid_values", "random_float64_values")
	for _, tableJSON := range assertSnapshotJSONMessages(t, analysisJSON["catalog"]) {
		table := assertSnapshotJSONKeys(t, tableJSON, "database", "table", "table_id", "schema_hash", "columns")
		for _, columnJSON := range assertSnapshotJSONMessages(t, table["columns"]) {
			assertSnapshotJSONKeys(t, columnJSON, "name", "type", "generation", "default_expression")
		}
	}
	assertSnapshotJSONKeys(t, fixture.AnalyzeResponse,
		"contract_version", "query_profile_id", "code", "message", "sql_after_materialization", "target_table_id", "target_columns", "read_table_ids")
	prepareJSON := assertSnapshotJSONKeys(t, fixture.PrepareRequest, "analysis", "bindings")
	prepareAnalysisJSON := assertSnapshotJSONKeys(t, prepareJSON["analysis"],
		"contract_version", "query_profile_id", "sql", "logical_database", "catalog", "materialize", "inputs")
	assertSnapshotJSONKeys(t, prepareAnalysisJSON["inputs"],
		"now_unix_ns", "random_uint64_values", "uuid_values", "random_float64_values")
	for _, tableJSON := range assertSnapshotJSONMessages(t, prepareAnalysisJSON["catalog"]) {
		table := assertSnapshotJSONKeys(t, tableJSON, "database", "table", "table_id", "schema_hash", "columns")
		for _, columnJSON := range assertSnapshotJSONMessages(t, table["columns"]) {
			assertSnapshotJSONKeys(t, columnJSON, "name", "type", "generation", "default_expression")
		}
	}
	for _, bindingJSON := range assertSnapshotJSONMessages(t, prepareJSON["bindings"]) {
		assertSnapshotJSONKeys(t, bindingJSON, "table_id", "scratch_database", "scratch_table")
	}
	assertSnapshotJSONKeys(t, fixture.PrepareResponse,
		"contract_version", "query_profile_id", "code", "message", "select_sql", "target_table_id", "target_columns", "read_table_ids")
	metadataJSON := assertSnapshotJSONKeys(t, fixture.MetadataTransportTable,
		"database", "table", "table_id", "schema_hash", "columns")
	for _, columnJSON := range assertSnapshotJSONMessages(t, metadataJSON["columns"]) {
		assertSnapshotJSONKeys(t, columnJSON, "name", "type", "generation", "default_expression")
	}

	analysis := &pb.AnalyzeSnapshotQueryRequest{}
	unmarshalSnapshotJSON(t, fixture.AnalyzeRequest, analysis)
	assertSnapshotBinaryRoundTrip(t, analysis, &pb.AnalyzeSnapshotQueryRequest{})
	if analysis.ContractVersion != 1 || analysis.QueryProfileId != "synthetic-snapshot-profile-v1" || analysis.Sql != "INSERT INTO tenant.copy SELECT value FROM tenant.events" || analysis.LogicalDatabase != "tenant" || !analysis.Materialize {
		t.Fatalf("analysis scalar fields lost: %v", analysis)
	}
	if len(analysis.Catalog) != 2 || analysis.Catalog[0].Table != "copy" || analysis.Catalog[1].Table != "events" {
		t.Fatalf("catalog order = %v", analysis.Catalog)
	}
	if got := []string{analysis.Catalog[0].Columns[0].Name, analysis.Catalog[0].Columns[1].Name, analysis.Catalog[1].Columns[0].Name, analysis.Catalog[1].Columns[1].Name}; !reflect.DeepEqual(got, []string{"copy_a", "copy_b", "event_value", "event_time"}) {
		t.Fatalf("schema order = %v", got)
	}
	for _, table := range analysis.Catalog {
		for _, column := range table.Columns {
			if column.Generation != pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ORDINARY || column.DefaultExpression != "" {
				t.Fatalf("normal synthetic column %s.%s metadata = (%v, %q)", table.Table, column.Name, column.Generation, column.DefaultExpression)
			}
		}
	}
	if analysis.Inputs.GetNowUnixNs() != 1700000000123456789 || !reflect.DeepEqual(analysis.Inputs.RandomUint64Values, []uint64{11, 22}) || !reflect.DeepEqual(analysis.Inputs.RandomFloat64Values, []float64{0.125, 0.875}) {
		t.Fatalf("materialization input order = %v", analysis.Inputs)
	}

	analysisResponse := &pb.AnalyzeSnapshotQueryResponse{}
	unmarshalSnapshotJSON(t, fixture.AnalyzeResponse, analysisResponse)
	assertSnapshotBinaryRoundTrip(t, analysisResponse, &pb.AnalyzeSnapshotQueryResponse{})
	if analysisResponse.Code != pb.SnapshotQueryCode_SUCCESS || !reflect.DeepEqual(analysisResponse.TargetColumns, []string{"copy_b", "copy_a"}) || !reflect.DeepEqual(analysisResponse.ReadTableIds, []string{"read-table-43"}) {
		t.Fatalf("analysis response/order = %v", analysisResponse)
	}

	prepare := &pb.PrepareSnapshotQueryRequest{}
	unmarshalSnapshotJSON(t, fixture.PrepareRequest, prepare)
	assertSnapshotBinaryRoundTrip(t, prepare, &pb.PrepareSnapshotQueryRequest{})
	if prepare.Analysis == nil || prepare.Analysis.Inputs.GetNowUnixNs() != 1700000000123456789 || len(prepare.Bindings) != 1 {
		t.Fatalf("prepare request = %v", prepare)
	}
	if got := []string{prepare.Bindings[0].TableId, prepare.Bindings[0].ScratchDatabase, prepare.Bindings[0].ScratchTable}; !reflect.DeepEqual(got, []string{"read-table-43", "scratch_db_53", "scratch_table_59"}) {
		t.Fatalf("scratch binding order = %v", got)
	}

	prepareResponse := &pb.PrepareSnapshotQueryResponse{}
	unmarshalSnapshotJSON(t, fixture.PrepareResponse, prepareResponse)
	assertSnapshotBinaryRoundTrip(t, prepareResponse, &pb.PrepareSnapshotQueryResponse{})
	if prepareResponse.Code != pb.SnapshotQueryCode_SUCCESS || !reflect.DeepEqual(prepareResponse.TargetColumns, []string{"copy_a", "copy_b"}) || !reflect.DeepEqual(prepareResponse.ReadTableIds, []string{"read-table-43"}) {
		t.Fatalf("prepare response/order = %v", prepareResponse)
	}

	orderedAnalysisResponse := &pb.AnalyzeSnapshotQueryResponse{
		TargetColumns: []string{"target_b", "target_a"},
		ReadTableIds:  []string{"read-a", "read-b"},
	}
	decodedAnalysisResponse := &pb.AnalyzeSnapshotQueryResponse{}
	assertSnapshotBinaryRoundTrip(t, orderedAnalysisResponse, decodedAnalysisResponse)
	if !reflect.DeepEqual(decodedAnalysisResponse.TargetColumns, []string{"target_b", "target_a"}) || !reflect.DeepEqual(decodedAnalysisResponse.ReadTableIds, []string{"read-a", "read-b"}) {
		t.Fatalf("constructed response order = %v", decodedAnalysisResponse)
	}
	orderedPrepare := &pb.PrepareSnapshotQueryRequest{Bindings: []*pb.SnapshotScratchBinding{
		{TableId: "read-a", ScratchDatabase: "scratch-a-db", ScratchTable: "scratch-a-table"},
		{TableId: "read-b", ScratchDatabase: "scratch-b-db", ScratchTable: "scratch-b-table"},
	}}
	decodedPrepare := &pb.PrepareSnapshotQueryRequest{}
	assertSnapshotBinaryRoundTrip(t, orderedPrepare, decodedPrepare)
	if got := []string{decodedPrepare.Bindings[0].TableId, decodedPrepare.Bindings[0].ScratchDatabase, decodedPrepare.Bindings[0].ScratchTable, decodedPrepare.Bindings[1].TableId, decodedPrepare.Bindings[1].ScratchDatabase, decodedPrepare.Bindings[1].ScratchTable}; !reflect.DeepEqual(got, []string{"read-a", "scratch-a-db", "scratch-a-table", "read-b", "scratch-b-db", "scratch-b-table"}) {
		t.Fatalf("constructed scratch binding order = %v", got)
	}

	transportTable := &pb.SnapshotQueryCatalogTable{}
	unmarshalSnapshotJSON(t, fixture.MetadataTransportTable, transportTable)
	assertSnapshotBinaryRoundTrip(t, transportTable, &pb.SnapshotQueryCatalogTable{})
	wantMetadata := []struct {
		name       string
		generation pb.SnapshotQueryColumnGeneration
		expression string
	}{
		{"default_column", pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_DEFAULT, "41 + 1"},
		{"materialized_column", pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_MATERIALIZED, "default_column * 2"},
		{"alias_column", pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ALIAS, "materialized_column + 3"},
		{"future_column", pb.SnapshotQueryColumnGeneration(127), "future_expression()"},
	}
	if len(transportTable.Columns) != len(wantMetadata) {
		t.Fatalf("transport-only column count = %d", len(transportTable.Columns))
	}
	for i, want := range wantMetadata {
		column := transportTable.Columns[i]
		if column.Name != want.name || column.Generation != want.generation || column.DefaultExpression != want.expression {
			t.Errorf("transport-only column[%d] = %v, want (%q, %d, %q)", i, column, want.name, want.generation, want.expression)
		}
	}
}

func TestSnapshotQueryOldAndUnknownColumnMetadataRemainTransportOnly(t *testing.T) {
	legacyWire := []byte{0x0a, 0x0a, 'o', 'l', 'd', '_', 'c', 'o', 'l', 'u', 'm', 'n', 0x12, 0x06, 'U', 'I', 'n', 't', '6', '4'}
	legacy := &pb.SnapshotQueryColumn{}
	if err := proto.Unmarshal(legacyWire, legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Name != "old_column" || legacy.Type != "UInt64" || legacy.Generation != pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_UNSPECIFIED || legacy.DefaultExpression != "" {
		t.Fatalf("old name/type-only wire decoded as %v; missing metadata must not be inferred as ordinary", legacy)
	}

	const futureGeneration = pb.SnapshotQueryColumnGeneration(127)
	future := &pb.SnapshotQueryColumn{Name: "future_column", Type: "String", Generation: futureGeneration, DefaultExpression: "future_expression()"}
	decoded := &pb.SnapshotQueryColumn{}
	assertSnapshotBinaryRoundTrip(t, future, decoded)
	if decoded.Generation != futureGeneration {
		t.Fatalf("unknown generation = %d, want %d preserved for future handler refusal", decoded.Generation, futureGeneration)
	}
}

func TestSnapshotQueryGeneratedDefaultServiceIsTransportUnimplemented(t *testing.T) {
	server := pb.UnimplementedRewriterServiceServer{}
	analysisResponse, err := server.AnalyzeSnapshotQuery(context.Background(), &pb.AnalyzeSnapshotQueryRequest{ContractVersion: 1})
	if status.Code(err) != codes.Unimplemented || analysisResponse != nil {
		t.Fatalf("AnalyzeSnapshotQuery = (%v, %v), want nil response and transport Unimplemented", analysisResponse, err)
	}
	prepareResponse, err := server.PrepareSnapshotQuery(context.Background(), &pb.PrepareSnapshotQueryRequest{})
	if status.Code(err) != codes.Unimplemented || prepareResponse != nil {
		t.Fatalf("PrepareSnapshotQuery = (%v, %v), want nil response and transport Unimplemented", prepareResponse, err)
	}
}
