package rewriter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/proto"
)

func (s *Service) AnalyzeSnapshotQuery(ctx context.Context, r *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error) {
	a, _ := analyzeSnapshot(ctx, s.engine, s.measuredSnapshotProfiles, r)
	return a, nil
}
func (r *NativeRewriter) AnalyzeSnapshotQuery(ctx context.Context, q *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error) {
	a, _ := analyzeSnapshot(ctx, r.engine, r.measuredSnapshotProfiles, q)
	return a, nil
}
func (s *Service) PrepareSnapshotQuery(ctx context.Context, r *pb.PrepareSnapshotQueryRequest) (*pb.PrepareSnapshotQueryResponse, error) {
	return prepareSnapshot(ctx, s.engine, s.measuredSnapshotProfiles, r), nil
}
func (r *NativeRewriter) PrepareSnapshotQuery(ctx context.Context, q *pb.PrepareSnapshotQueryRequest) (*pb.PrepareSnapshotQueryResponse, error) {
	return prepareSnapshot(ctx, r.engine, r.measuredSnapshotProfiles, q), nil
}

func supportedSnapshotRecord(raw string) (snapshotRecord, bool) {
	var r snapshotRecord
	if json.Unmarshal([]byte(raw), &r) != nil {
		return r, false
	}
	settings := []snapshotSetting{{"cast_keep_nullable", "0"}, {"max_threads", "1"}, {"read_overflow_mode", "throw"}, {"result_overflow_mode", "throw"}, {"sort_overflow_mode", "throw"}, {"timeout_overflow_mode", "throw"}}
	operators := []string{"and", "column", "equals", "greater", "greaterOrEquals", "in", "less", "lessOrEquals", "literal", "not", "notEquals", "or", "prepare-integer-literal-exact-v1", "prepare-integer-scalar-null-throw-v1", "rand", "rand32", "rand64"}
	return r, r.Version == 1 && r.ColumnProfileID == "column-profile-v1" && r.OutputOrderID == "output-order-v1" && reflect.DeepEqual(r.Settings, settings) && reflect.DeepEqual(r.ScalarOperators, operators) && r.Limits.MaxSQLBytes <= 65536 && r.Limits.MaxDescriptorBytes <= 65536 && r.Limits.MaxExecutionMS <= 30000
}
func analyzeSnapshot(ctx context.Context, e engine.Engine, profiles map[string]string, r *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, *engine.SnapshotPlan) {
	out := &pb.AnalyzeSnapshotQueryResponse{}
	fail := func(code pb.SnapshotQueryCode, message string) (*pb.AnalyzeSnapshotQueryResponse, *engine.SnapshotPlan) {
		out.Code = code
		out.Message = "snapshot query " + message
		return out, nil
	}
	if r == nil || r.ContractVersion != 1 {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "contract version is unsupported")
	}
	rec, ok := supportedSnapshotRecord(profiles[r.QueryProfileId])
	if !ok {
		return fail(pb.SnapshotQueryCode_PROFILE_UNAVAILABLE, "profile is unavailable")
	}
	out.ContractVersion = 1
	out.QueryProfileId = r.QueryProfileId
	ctx, cancel := context.WithTimeout(ctx, time.Duration(rec.Limits.MaxExecutionMS)*time.Millisecond)
	defer cancel()
	if ctx.Err() != nil {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "execution deadline exceeded")
	}
	if uint64(len(r.Sql)) > rec.Limits.MaxSQLBytes {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "SQL exceeds the profile limit")
	}
	if uint64(proto.Size(r)) > rec.Limits.MaxDescriptorBytes+uint64(len(r.Sql)) {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "descriptor exceeds the profile limit")
	}
	if !r.Materialize && r.Inputs != nil {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "inputs require materialization")
	}
	opts := engine.SnapshotOptions{Database: r.LogicalDatabase, Materialize: r.Materialize}
	for _, t := range r.Catalog {
		if t == nil || !snapshotDigest(t.TableId) || !snapshotDigest(t.SchemaHash) {
			return fail(pb.SnapshotQueryCode_INVALID_INPUT, "catalog identity is invalid")
		}
		et := engine.SnapshotTable{Database: t.Database, Name: t.Table, ID: t.TableId}
		for _, c := range t.Columns {
			if c == nil {
				return fail(pb.SnapshotQueryCode_INVALID_INPUT, "catalog column is invalid")
			}
			et.Columns = append(et.Columns, engine.SnapshotColumn{Name: c.Name, Type: c.Type, Ordinary: c.Generation == pb.SnapshotQueryColumnGeneration_SNAPSHOT_QUERY_COLUMN_GENERATION_ORDINARY && c.DefaultExpression == ""})
		}
		opts.Catalog = append(opts.Catalog, et)
	}
	if r.Inputs != nil {
		opts.Random = r.Inputs.RandomUint64Values
	}
	plan, err := engine.AnalyzeSnapshot(e, r.Sql, opts)
	if err != nil {
		var typed *engine.SnapshotError
		if errors.As(err, &typed) {
			out.Code = pb.SnapshotQueryCode(pb.SnapshotQueryCode_value[typed.Kind])
			out.Message = typed.Message
			return out, nil
		}
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "requires one recognized statement")
	}
	if ctx.Err() != nil {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "execution deadline exceeded")
	}
	if r.Inputs != nil && (r.Inputs.NowUnixNs != nil || len(r.Inputs.RandomFloat64Values) > 0 || len(r.Inputs.UuidValues) > 0) {
		return fail(pb.SnapshotQueryCode_MATERIALIZATION_FAILED, "materialization is incomplete or unsupported")
	}
	if plan.Ordinary {
		out.Code = pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY
		return out, plan
	}
	if uint64(len(plan.SQL)) > rec.Limits.MaxSQLBytes {
		return fail(pb.SnapshotQueryCode_INVALID_INPUT, "SQL exceeds the profile limit")
	}
	out.Code = pb.SnapshotQueryCode_SUCCESS
	out.SqlAfterMaterialization = plan.SQL
	out.TargetTableId = plan.TargetID
	out.TargetColumns = plan.TargetColumns
	out.ReadTableIds = plan.ReadIDs
	return out, plan
}
func prepareSnapshot(ctx context.Context, e engine.Engine, profiles map[string]string, r *pb.PrepareSnapshotQueryRequest) *pb.PrepareSnapshotQueryResponse {
	if r == nil || r.Analysis == nil {
		return &pb.PrepareSnapshotQueryResponse{Code: pb.SnapshotQueryCode_INVALID_INPUT, Message: "snapshot query preparation requires analysis"}
	}
	rec, supported := supportedSnapshotRecord(profiles[r.Analysis.QueryProfileId])
	if supported && r.Analysis.ContractVersion == 1 {
		// This parent budget survives Analyze's child context and covers scratch
		// processing and generation. Native FFI cannot be interrupted in flight.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(rec.Limits.MaxExecutionMS)*time.Millisecond)
		defer cancel()
	}
	if r.Analysis.ContractVersion == 1 && r.Analysis.Materialize {
		if supported {
			return &pb.PrepareSnapshotQueryResponse{ContractVersion: 1, QueryProfileId: r.Analysis.QueryProfileId, Code: pb.SnapshotQueryCode_INVALID_INPUT, Message: "snapshot query preparation requires nonmaterializing analysis"}
		}
	}
	if supported && r.Analysis.ContractVersion == 1 && uint64(proto.Size(r)) > rec.Limits.MaxDescriptorBytes+uint64(len(r.Analysis.Sql)) {
		return &pb.PrepareSnapshotQueryResponse{ContractVersion: 1, QueryProfileId: r.Analysis.QueryProfileId, Code: pb.SnapshotQueryCode_INVALID_INPUT, Message: "snapshot query descriptor exceeds the profile limit"}
	}
	a, p := analyzeSnapshot(ctx, e, profiles, r.Analysis)
	out := &pb.PrepareSnapshotQueryResponse{ContractVersion: a.ContractVersion, QueryProfileId: a.QueryProfileId, Code: a.Code, Message: a.Message}
	if a.Code != pb.SnapshotQueryCode_SUCCESS {
		if a.Code == pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY {
			out.Code = pb.SnapshotQueryCode_INVALID_INPUT
			out.Message = "snapshot query preparation requires an insert select"
		}
		return out
	}
	invalid := func() *pb.PrepareSnapshotQueryResponse {
		out.Code = pb.SnapshotQueryCode_INVALID_INPUT
		out.Message = "snapshot query scratch bindings must be an exact ordered bijection"
		return out
	}
	expired := func() *pb.PrepareSnapshotQueryResponse {
		out.Code = pb.SnapshotQueryCode_INVALID_INPUT
		out.Message = "snapshot query execution deadline exceeded"
		return out
	}

	if len(r.Bindings) != len(a.ReadTableIds) {
		return invalid()
	}
	bindings := map[string][2]string{}
	scratch := map[[2]string]bool{}
	for i, b := range r.Bindings {
		if b == nil || b.TableId != a.ReadTableIds[i] || strings.TrimSpace(b.ScratchDatabase) == "" || strings.TrimSpace(b.ScratchTable) == "" || strings.ContainsRune(b.ScratchDatabase, 0) || strings.ContainsRune(b.ScratchTable, 0) {
			return invalid()
		}
		pair := [2]string{b.ScratchDatabase, b.ScratchTable}
		if scratch[pair] {
			return invalid()
		}
		scratch[pair] = true
		bindings[b.TableId] = pair
	}
	if ctx.Err() != nil {
		return expired()
	}
	sql, err := p.Prepare(e, bindings)
	if ctx.Err() != nil {
		return expired()
	}
	if err != nil {
		out.Code = pb.SnapshotQueryCode_UNSUPPORTED
		out.Message = "snapshot query syntax is outside the closed profile"
		return out
	}
	if uint64(len(sql)) > rec.Limits.MaxSQLBytes {
		out.Code = pb.SnapshotQueryCode_INVALID_INPUT
		out.Message = "snapshot query SQL exceeds the profile limit"
		return out
	}
	var targetColumns []string
	for _, t := range r.Analysis.Catalog {
		if t.TableId == a.TargetTableId {
			for _, c := range t.Columns {
				targetColumns = append(targetColumns, c.Name)
			}
		}
	}
	if ctx.Err() != nil {
		return expired()
	}
	out.SelectSql = sql
	out.TargetTableId = a.TargetTableId
	out.TargetColumns = targetColumns
	out.ReadTableIds = a.ReadTableIds
	return out
}
