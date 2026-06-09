package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// errmsgCase drives a (sql, options) through the native rewriter to populate the
// per-connection last-call maps, then inverts error_message and compares to
// want_inverted (frozen native output). When REWRITER_ORACLE_ADDR is set, the
// inverted string is additionally diffed against the C++ oracle's
// RewriteErrorMessage(sql, error_message, options).error_after_rewrite (exact).
type errmsgCase struct {
	Name         string              `json:"name"`
	SQL          string              `json:"sql"`
	Dynamic      *dblevelDynamicJSON `json:"dynamic"`
	ErrorMessage string              `json:"error_message"`
	WantInverted string              `json:"want_inverted"`
}

func (c errmsgCase) options() []*pb.RewriteOption {
	if c.Dynamic == nil {
		return nil
	}
	da := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                      c.Dynamic.DatabaseMap,
		KnownPhysicalDatabases:           c.Dynamic.KnownPhysicalDatabases,
		UpstreamLogicalDatabaseInContext: c.Dynamic.UpstreamLogical,
		Delim:                            c.Dynamic.Delim,
	}
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func loadErrmsgCases(t *testing.T) []errmsgCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "errmsg_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cs []errmsgCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// TestErrmsgGolden is the Phase-5 parity gate for RewriteErrorMessage. want_inverted
// was frozen from native output; the REWRITER_ORACLE_ADDR differential is the TRUE gate.
func TestErrmsgGolden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oracle, _ := DialOracle()
	defer oracle.Close()
	ctx := context.Background()

	for _, c := range loadErrmsgCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			if _, err := r.Rewrite(ctx, c.SQL, "acct"); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			inv, err := r.RewriteErrorMessage(ctx, c.ErrorMessage)
			if err != nil {
				t.Fatalf("rewriteErrorMessage: %v", err)
			}
			if inv != c.WantInverted {
				t.Errorf("inverted:\n got %q\nwant %q", inv, c.WantInverted)
			}
			if oracle != nil {
				resp, oerr := oracle.RewriteErrorMessage(c.SQL, c.ErrorMessage, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				if inv != resp.GetErrorAfterRewrite() {
					t.Errorf("oracle divergence:\n got %q\nwant %q", inv, resp.GetErrorAfterRewrite())
				}
			}
		})
	}
}
