package rewriter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

func (s *Service) MaterializeSQL(_ context.Context, req *pb.MaterializeSQLRequest) (*pb.MaterializeSQLResponse, error) {
	return doMaterializeSQL(s.engine, req)
}

func (r *NativeRewriter) MaterializeSQL(_ context.Context, req *pb.MaterializeSQLRequest) (*pb.MaterializeSQLResponse, error) {
	return doMaterializeSQL(r.engine, req)
}

func doMaterializeSQL(e engine.Engine, req *pb.MaterializeSQLRequest) (*pb.MaterializeSQLResponse, error) {
	if req == nil {
		return &pb.MaterializeSQLResponse{
			Code:                  pb.MaterializeCode_MaterializeInvalidRequest,
			MaterializerProfileId: engine.DefaultMaterializationProfileID,
			Message:               "materialize request is nil",
		}, nil
	}

	sql := req.GetSql()
	resp := &pb.MaterializeSQLResponse{
		Code:                    pb.MaterializeCode_MaterializeSuccess,
		Sql:                     sql,
		SqlAfterMaterialization: sql,
		MaterializerProfileId:   engine.DefaultMaterializationProfileID,
	}
	profileID, err := materializationProfileID(req.GetPolicy())
	if err != nil {
		resp.Code = pb.MaterializeCode_MaterializeInvalidRequest
		resp.Message = err.Error()
		return resp, nil
	}
	resp.MaterializerProfileId = profileID

	ast, err := e.ParseOne(sql)
	if err != nil {
		resp.Code = pb.MaterializeCode_MaterializeSyntaxError
		resp.Message = err.Error()
		return resp, nil
	}
	result, err := engine.MaterializeNonDeterminism(ast, materializationInputsFromPB(req.GetInputs()))
	if err != nil {
		resp.Code = materializeCodeForError(err)
		resp.Message = err.Error()
		return resp, nil
	}
	if len(result.Replacements) == 0 {
		return resp, nil
	}
	out, err := e.Generate(result.AST)
	if err != nil {
		resp.Code = pb.MaterializeCode_MaterializeError
		resp.Message = err.Error()
		return resp, nil
	}
	resp.SqlAfterMaterialization = out
	resp.Replacements = materializedReplacementsToPB(result.Replacements)
	return resp, nil
}

func materializationProfileID(policy *pb.MaterializationPolicy) (string, error) {
	profileID := policy.GetProfileId()
	if profileID == "" {
		profileID = engine.DefaultMaterializationProfileID
	}
	if profileID != engine.DefaultMaterializationProfileID {
		return "", fmt.Errorf("unsupported materialization profile %q", profileID)
	}
	timezone := strings.ToUpper(policy.GetTimezone())
	if timezone != "" && timezone != "UTC" {
		return "", fmt.Errorf("unsupported materialization timezone %q", policy.GetTimezone())
	}
	return profileID, nil
}

func materializationInputsFromPB(inputs *pb.MaterializationInputs) engine.MaterializationInputs {
	var nowUnixNS *int64
	if inputs != nil && inputs.NowUnixNs != nil {
		value := inputs.GetNowUnixNs()
		nowUnixNS = &value
	}
	return engine.MaterializationInputs{
		NowUnixNS:           nowUnixNS,
		RandomUint64Values:  inputs.GetRandomUint64Values(),
		RandomFloat64Values: inputs.GetRandomFloat64Values(),
		UUIDValues:          inputs.GetUuidValues(),
	}
}

func materializeCodeForError(err error) pb.MaterializeCode {
	switch {
	case errors.Is(err, engine.ErrMaterializationUnsupported):
		return pb.MaterializeCode_MaterializeUnsupportedStatement
	case errors.Is(err, engine.ErrMaterializationInputMissing),
		errors.Is(err, engine.ErrMaterializationInvalidInput):
		return pb.MaterializeCode_MaterializeInvalidRequest
	default:
		return pb.MaterializeCode_MaterializeError
	}
}

func materializedReplacementsToPB(replacements []engine.MaterializedReplacement) []*pb.MaterializedReplacement {
	out := make([]*pb.MaterializedReplacement, 0, len(replacements))
	for _, replacement := range replacements {
		out = append(out, &pb.MaterializedReplacement{
			FunctionName: replacement.FunctionName,
			Ordinal:      replacement.Ordinal,
			LiteralSql:   replacement.LiteralSQL,
			ValueType:    replacement.ValueType,
		})
	}
	return out
}
