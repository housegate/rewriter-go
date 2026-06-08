package handlers

import (
	"strconv"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// applyOptions applies LIMIT/OFFSET/SETTINGS options to the AST's outermost select.
// TableNameRewrite is handled by the Task-7 table walk (ignored here);
// CommonTableExprRewrite is Task 9.
func applyOptions(ast engine.AST, opts []*pb.RewriteOption) (engine.AST, error) {
	var (
		forceLimit   *int64
		replaceLimit *struct{ threshold, to int64 }
		offset       *int64
		settings     []engine.Setting
	)
	for _, o := range opts {
		switch o.GetOp() {
		case pb.RewriteOp_LimitRewrite:
			switch v := o.GetLimitArgs().GetValue().(type) {
			case *pb.RewriteLimitArgs_ForceLimit:
				n := int64(v.ForceLimit)
				forceLimit = &n
			case *pb.RewriteLimitArgs_ReplaceLimit_:
				replaceLimit = &struct{ threshold, to int64 }{int64(v.ReplaceLimit.GetThreshold()), int64(v.ReplaceLimit.GetReplaceTo())}
			}
		case pb.RewriteOp_OffsetRewrite:
			n := int64(o.GetOffsetArgs().GetOffset())
			offset = &n
		case pb.RewriteOp_SettingsRewrite:
			for _, s := range o.GetSettingsArgs().GetSettings() {
				settings = append(settings, settingToEngine(s))
			}
		}
	}

	if forceLimit != nil {
		var err error
		if ast, err = engine.SetLimit(ast, *forceLimit); err != nil {
			return nil, err
		}
	} else if replaceLimit != nil {
		cur, ok, err := engine.GetLimit(ast)
		if err != nil {
			return nil, err
		}
		if !ok || cur == 0 || cur > replaceLimit.threshold { // replace if absent/0/over threshold
			if ast, err = engine.SetLimit(ast, replaceLimit.to); err != nil {
				return nil, err
			}
		}
	}
	if offset != nil && *offset > 0 {
		var err error
		if ast, err = engine.SetOffset(ast, *offset); err != nil {
			return nil, err
		}
	}
	if len(settings) > 0 {
		var err error
		if ast, err = engine.SetSettings(ast, settings); err != nil {
			return nil, err
		}
	}
	return ast, nil
}

// settingToEngine maps a proto Setting to an engine.Setting, choosing the literal
// type from the value oneof.
func settingToEngine(s *pb.RewriteSettingsArgs_Setting) engine.Setting {
	switch s.GetValue().(type) {
	case *pb.RewriteSettingsArgs_Setting_StringValue:
		return engine.Setting{Key: s.GetKey(), LiteralType: "string", Value: s.GetStringValue()}
	case *pb.RewriteSettingsArgs_Setting_BoolValue:
		v := "0"
		if s.GetBoolValue() {
			v = "1"
		}
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: v}
	case *pb.RewriteSettingsArgs_Setting_IntValue:
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: strconv.FormatInt(int64(s.GetIntValue()), 10)}
	case *pb.RewriteSettingsArgs_Setting_Uint64Value:
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: strconv.FormatUint(s.GetUint64Value(), 10)}
	default:
		return engine.Setting{Key: s.GetKey(), LiteralType: "string", Value: ""}
	}
}
