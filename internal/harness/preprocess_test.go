package harness

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
)

// TestPreprocessINSplitSkipped records the Phase-5 preprocess DECISION (design
// §6f/§10): the native rewriter intentionally does NOT split large IN clauses
// into OR-batches the way the C++ QueryPreprocessor does (threshold 50). Polyglot
// (Rust) parses arbitrarily large IN lists without ClickHouse's recursive-descent
// stack limit, so this guard is unnecessary — BUT polyglot has its own
// recursive-descent limit on bracket-nesting DEPTH (not IN-list length), guarded
// separately by engine.exceedsNestingDepth (see internal/engine/guard.go). Against
// the C++ oracle a 51+-element
// IN is an ALLOW-LISTED divergence (C++ OR-batches; native keeps the flat IN —
// the two are semantically equal). This test pins native's flat-IN behavior; it
// deliberately performs NO oracle comparison.
func TestPreprocessINSplitSkipped(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// 60 elements — above the C++ IN_CLAUSE_SPLIT_THRESHOLD of 50.
	nums := make([]string, 60)
	for i := range nums {
		nums[i] = strconv.Itoa(i)
	}
	sql := "SELECT x FROM t WHERE x IN (" + strings.Join(nums, ", ") + ")"

	r := newWriteRewriter(e, nil) // no rewrite policy → pure round-trip
	res, err := r.Rewrite(context.Background(), sql, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code.String() != "Success" {
		t.Fatalf("code=%v", res.Code)
	}
	// The IN list survived as a single clause: no OR-batching was introduced.
	if strings.Contains(strings.ToUpper(res.SQL), " OR ") {
		t.Errorf("native unexpectedly split the IN clause into OR-batches: %q", res.SQL)
	}
	if !strings.Contains(res.SQL, "59") {
		t.Errorf("IN list lost elements: %q", res.SQL)
	}
}
