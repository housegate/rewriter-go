// Command fidelity-spike measures polyglot's ClickHouse round-trip fidelity over
// a SQL corpus: per statement it reports the node kind (top-level AST key) and
// whether parse + idempotent round-trip hold. This is the Phase 0 go/no-go signal.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/housegate/rewriter-go/internal/corpus"
	"github.com/housegate/rewriter-go/internal/engine"
)

func main() {
	corpusPath := flag.String("corpus", "internal/corpus/testdata/seed.sql", "path to SQL corpus")
	flag.Parse()

	cases, err := corpus.Load(*corpusPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load corpus:", err)
		os.Exit(1)
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open engine:", err)
		os.Exit(1)
	}
	defer e.Close()

	counts := map[string]int{}
	fmt.Printf("%-4s  %-16s  %-14s  %s\n", "#", "node_kind", "fidelity", "sql")
	fmt.Println(strings.Repeat("-", 90))
	for i, sql := range cases {
		kind := "PARSE_ERR"
		if ast, perr := e.ParseOne(sql); perr == nil {
			if k, kerr := engine.NodeKind(ast); kerr == nil {
				kind = k
			} else {
				kind = "?"
			}
		}
		fr := engine.CheckFidelity(e, sql)
		counts[fr.Status.String()]++
		short := sql
		if len(short) > 50 {
			short = short[:47] + "..."
		}
		fmt.Printf("%-4d  %-16s  %-14s  %s\n", i+1, kind, fr.Status.String(), short)
		if fr.Status == engine.FidelityNonIdempotent {
			fmt.Printf("        gen1: %s\n        gen2: %s\n", fr.Gen1, fr.Gen2)
		}
		if fr.Status == engine.FidelityParseError || fr.Status == engine.FidelityGenerateError {
			fmt.Printf("        err: %s\n", fr.Err)
		}
	}

	fmt.Println("\nsummary:")
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-14s %d\n", k, counts[k])
	}
	fmt.Printf("  %-14s %d\n", "total", len(cases))
}
