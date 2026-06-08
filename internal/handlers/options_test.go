package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestApplyOptions_forceLimit(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db.t LIMIT 100")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_LimitRewrite,
		Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{
			Value: &pb.RewriteLimitArgs_ForceLimit{ForceLimit: 10}}}}}
	out, err := applyOptions(ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Generate(out); got != "SELECT a FROM db.t LIMIT 10" {
		t.Fatalf("force limit got %q", got)
	}
}

func TestApplyOptions_replaceLimitThreshold(t *testing.T) {
	e := newEngine(t)
	mk := func() *pb.RewriteOption {
		return &pb.RewriteOption{Op: pb.RewriteOp_LimitRewrite,
			Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{
				Value: &pb.RewriteLimitArgs_ReplaceLimit_{ReplaceLimit: &pb.RewriteLimitArgs_ReplaceLimit{Threshold: 50, ReplaceTo: 10}}}}}
	}
	ast1, _ := e.ParseOne("SELECT a FROM db.t LIMIT 100")
	out1, _ := applyOptions(ast1, []*pb.RewriteOption{mk()})
	if g, _ := e.Generate(out1); g != "SELECT a FROM db.t LIMIT 10" {
		t.Fatalf("over threshold got %q", g)
	}
	ast2, _ := e.ParseOne("SELECT a FROM db.t LIMIT 5")
	out2, _ := applyOptions(ast2, []*pb.RewriteOption{mk()})
	if g, _ := e.Generate(out2); g != "SELECT a FROM db.t LIMIT 5" {
		t.Fatalf("under threshold got %q", g)
	}
}

func TestApplyOptions_settings(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db.t")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_SettingsRewrite,
		Value: &pb.RewriteOption_SettingsArgs{SettingsArgs: &pb.RewriteSettingsArgs{
			Settings: []*pb.RewriteSettingsArgs_Setting{
				{Key: "max_threads", Value: &pb.RewriteSettingsArgs_Setting_IntValue{IntValue: 4}},
				{Key: "log_comment", Value: &pb.RewriteSettingsArgs_Setting_StringValue{StringValue: "hi"}},
			}}}}}
	out, err := applyOptions(ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	// number setting renders without quotes; string setting renders with single quotes.
	// Multiple settings are separated by ", ".
	want := "SELECT a FROM db.t SETTINGS max_threads = 4, log_comment = 'hi'"
	if got != want {
		t.Fatalf("settings got %q want %q", got, want)
	}
}

func TestApplyOptions_noOp(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db.t WHERE x IN (1, 2)")
	out, err := applyOptions(ast, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Generate(out); got != "SELECT a FROM db.t WHERE x IN (1, 2)" {
		t.Fatalf("no-op changed sql: %q", got)
	}
}
