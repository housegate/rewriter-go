# Native Go Rewriter — Phase 0 Implementation Plan (Engine + Harness + Fidelity Spike)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the foundation for a polyglot-backed native Go ClickHouse SQL rewriter — the `Engine` adapter over polyglot, generated `pb` contract types, a differential-comparison harness, a pass-through `Rewriter`, and a **fidelity spike** that quantifies how faithfully polyglot round-trips our ClickHouse corpus (the go/no-go for full parity).

**Architecture:** A Go module (`github.com/housegate/rewriter-go`) wraps the [tobilg/polyglot](https://github.com/tobilg/polyglot) Rust engine through its PureGo SDK (`CGO_ENABLED=0`, native lib loaded at runtime). The public `rewriter` package exposes the `Rewriter` interface; `internal/engine` is the only code that touches polyglot (the seam a future WASM backend would replace); `internal/harness` compares native output against the C++ `rewriter-grpc` oracle; `cmd/fidelity-spike` runs the corpus and emits a divergence report.

**Tech Stack:** Go 1.22+, `github.com/tobilg/polyglot/packages/go` (PureGo SDK), `libpolyglot_sql_ffi` (built from polyglot source via `cargo`), `buf` + `protoc-gen-go`/`protoc-gen-go-grpc` for the contract types, `google.golang.org/grpc` for the oracle client, standard `testing`.

> **Assumptions baked into this plan (adjust if wrong):**
> - **Module path** `github.com/housegate/rewriter-go` (derived from remote `housegate/rewriter-go`). If your VCS host differs, change it in `go.mod` and every import.
> - **polyglot pin** `v0.4.3` (matches the Go SDK `sdkVersion` constant). Bump `POLYGLOT_REF` + the `go.mod` require together.
> - **Branch:** do this work on `feat/phase-0-engine-harness` off `main` (the execution skill sets up the worktree/branch).
> - The polyglot **AST JSON shape is undocumented**; Task 3 captures it into `internal/engine/testdata/ast-shapes/` and Task 4 centralizes the one shape-dependent constant (`astClassKey`). Verify it against the captured snapshot before relying on classification.

---

## File Structure

```
rewriter-go/
  go.mod                                  # module + deps
  Makefile                                # ffi build, proto gen, test (exports FFI path)
  .gitignore
  proto/rewriter.proto                    # pinned copy of rewriter-grpc's contract (+ go_package)
  buf.yaml                                # buf module config
  buf.gen.yaml                            # buf codegen config (go + go-grpc)
  gen/pb/                                 # GENERATED: rewriter.pb.go, rewriter_grpc.pb.go
  rewriter.go                             # public: Rewriter interface, RewriteResult, resultFromPB
  native.go                               # public: NativeRewriter (pass-through in Phase 0) + New()
  native_test.go
  internal/
    engine/
      engine.go                           # Engine interface + AST type alias
      polyglot.go                         # polyglot-backed Engine (the only polyglot import)
      polyglot_test.go
      ast.go                              # astClass()/astKind() helpers (discriminator)
      ast_test.go
      testdata/ast-shapes/                # captured AST JSON snapshots (Task 3)
    corpus/
      corpus.go                           # corpus loader (---delimited .sql files)
      corpus_test.go
      testdata/seed.sql                   # seed corpus
    harness/
      compare.go                          # ResponseComparator (exact fields + semantic SQL)
      compare_test.go
      oracle.go                           # gRPC client to rewriter-grpc (env-gated)
  cmd/fidelity-spike/
    main.go                               # run spike over corpus, write report
  docs/superpowers/
    plans/2026-06-03-native-go-rewriter-phase-0.md
    reports/                              # spike output lands here (committed)
```

---

## Task 1: Module scaffold

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd rewriter-go
go mod init github.com/housegate/rewriter-go
go mod edit -go=1.22
```

- [ ] **Step 2: Add `.gitignore`**

Create `.gitignore`:
```gitignore
# built FFI lib + polyglot source checkout
/third_party/
# spike scratch
/tmp/
# go
*.test
*.out
```

- [ ] **Step 3: Add the Makefile skeleton**

Create `Makefile`:
```makefile
SHELL := /bin/bash
POLYGLOT_REF ?= v0.4.3
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  FFI_EXT := dylib
else
  FFI_EXT := so
endif
FFI_LIB := third_party/lib/libpolyglot_sql_ffi.$(FFI_EXT)
export POLYGLOT_SQL_FFI_PATH := $(abspath $(FFI_LIB))

.PHONY: ffi proto test tidy
ffi: $(FFI_LIB)
$(FFI_LIB):
	rm -rf third_party/polyglot-src
	git clone --depth 1 --branch $(POLYGLOT_REF) https://github.com/tobilg/polyglot third_party/polyglot-src
	cd third_party/polyglot-src && cargo build -p polyglot-sql-ffi --profile ffi_release
	mkdir -p third_party/lib
	cp third_party/polyglot-src/target/ffi_release/libpolyglot_sql_ffi.$(FFI_EXT) $(FFI_LIB)

proto:
	buf generate

test: ffi
	go test ./...

tidy:
	go mod tidy
```

- [ ] **Step 4: Verify the module builds**

Run: `go build ./... && echo OK`
Expected: `OK` (no packages yet, exits clean).

- [ ] **Step 5: Commit**

```bash
git add go.mod .gitignore Makefile
git commit -m "chore: module scaffold (go.mod, Makefile, gitignore)"
```

---

## Task 2: Build the polyglot FFI library + smoke test

**Files:**
- Modify: `go.mod` (add the SDK dep)
- Create: `internal/engine/smoke_test.go` (temporary; deleted at end of task)

- [ ] **Step 1: Add the polyglot Go SDK dependency**

Run:
```bash
go get github.com/tobilg/polyglot/packages/go@v0.4.3
```

- [ ] **Step 2: Build the native FFI library**

Run: `make ffi`
Expected: ends with `third_party/lib/libpolyglot_sql_ffi.<ext>` present. Verify:
```bash
ls -l third_party/lib/
```
(Requires a Rust toolchain — `rustup`/`cargo`. If absent: `curl https://sh.rustup.rs -sSf | sh`.)

- [ ] **Step 3: Write a smoke test that loads the lib**

Create `internal/engine/smoke_test.go`:
```go
package engine

import (
	"os"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

func TestSmokeLoadFFI(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	c, err := polyglot.OpenDefault()
	if err != nil {
		t.Fatalf("OpenDefault: %v", err)
	}
	defer c.Close()
	v, err := c.RuntimeVersion()
	if err != nil {
		t.Fatalf("RuntimeVersion: %v", err)
	}
	if v == "" {
		t.Fatal("empty runtime version")
	}
	t.Logf("polyglot runtime version: %s", v)
}
```

- [ ] **Step 4: Run the smoke test**

Run: `make test` (sets `POLYGLOT_SQL_FFI_PATH`) or `POLYGLOT_SQL_FFI_PATH=$(pwd)/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine/ -run TestSmokeLoadFFI -v`
Expected: PASS, logs a non-empty version.

- [ ] **Step 5: Remove the smoke test and commit**

```bash
rm internal/engine/smoke_test.go
go mod tidy
git add go.mod go.sum
git commit -m "build: wire polyglot Go SDK + FFI build (smoke-verified)"
```

---

## Task 3: Capture the polyglot AST JSON shape (characterization)

This produces the reference snapshots every later task and phase relies on. No production code — it records reality.

**Files:**
- Create: `internal/engine/characterize_test.go`
- Create (output): `internal/engine/testdata/ast-shapes/*.json`

- [ ] **Step 1: Write the characterization test**

Create `internal/engine/characterize_test.go`:
```go
package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// Statements chosen to exercise the discriminator across the families the
// rewriter must classify in later phases.
var characterizeCases = map[string]string{
	"select":       "SELECT a FROM db.t WHERE x IN (1,2)",
	"insert":       "INSERT INTO db.t (a) VALUES (1)",
	"create_table": "CREATE TABLE db.t (a Int64) ENGINE = MergeTree ORDER BY a",
	"drop_table":   "DROP TABLE IF EXISTS db.t",
	"alter_table":  "ALTER TABLE db.t ADD COLUMN b Int64",
	"rename_table": "RENAME TABLE db.a TO db.b",
	"use":          "USE db",
	"show_tables":  "SHOW TABLES FROM db",
	"show_create":  "SHOW CREATE TABLE db.t",
	"exists_table": "EXISTS TABLE db.t",
	"grant":        "GRANT SELECT ON db.t TO u",
	"select_join":  "SELECT * FROM a GLOBAL JOIN b ON a.id = b.id",
}

func TestCharacterizeAST(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	c, err := polyglot.OpenDefault()
	if err != nil {
		t.Fatalf("OpenDefault: %v", err)
	}
	defer c.Close()

	dir := filepath.Join("testdata", "ast-shapes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, sql := range characterizeCases {
		ast, err := c.ParseOne(sql, "clickhouse")
		if err != nil {
			t.Errorf("%s: ParseOne(%q): %v", name, sql, err)
			continue
		}
		var pretty json.RawMessage = ast
		buf, _ := json.MarshalIndent(json.RawMessage(pretty), "", "  ")
		out := filepath.Join(dir, name+".json")
		if err := os.WriteFile(out, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", out, len(buf))
	}
}
```

- [ ] **Step 2: Run it to generate the snapshots**

Run: `make test` or `POLYGLOT_SQL_FFI_PATH=... go test ./internal/engine/ -run TestCharacterizeAST -v`
Expected: PASS; `internal/engine/testdata/ast-shapes/` now holds one `.json` per case. Any case that logs a `ParseOne` error is an early fidelity signal — note it (it becomes a divergence-report entry in Task 11).

- [ ] **Step 3: Inspect the discriminator**

Run: `head -5 internal/engine/testdata/ast-shapes/select.json`
Read the top-level keys. **Identify the field that names the node kind** (SQLGlot-style serializers use a key such as `"class"`, `"type"`, or `"key"`). You will hard-code this key name once, in Task 4 (`astClassKey`). Record what you found in the commit message.

- [ ] **Step 4: Commit the snapshots**

```bash
git add internal/engine/characterize_test.go internal/engine/testdata/ast-shapes/
git commit -m "test: capture polyglot ClickHouse AST JSON shapes (discriminator: <KEY>)"
```

---

## Task 4: `Engine` interface + AST classification helper

**Files:**
- Create: `internal/engine/engine.go`
- Create: `internal/engine/ast.go`
- Create: `internal/engine/ast_test.go`

- [ ] **Step 1: Define the `Engine` interface and `AST` type**

Create `internal/engine/engine.go`:
```go
// Package engine is the ONLY code that talks to polyglot. It is the seam a
// future WASM/wazero backend would replace; nothing outside this package may
// import the polyglot SDK.
package engine

import "encoding/json"

// AST is polyglot's JSON AST. We decode only the nodes we mutate.
type AST = json.RawMessage

// Engine wraps the polyglot SQL engine, pinned to the ClickHouse dialect.
type Engine interface {
	ParseOne(sql string) (AST, error)
	Generate(ast AST) (string, error)
	RenameTables(ast AST, mapping map[string]string) (AST, error)
	QualifyTables(ast AST, db string) (AST, error)
	Tokenize(sql string) (AST, error)
	// DiffSQL compares two SQL strings semantically (polyglot parses both and
	// diffs the ASTs). Returns the raw diff JSON; harness code interprets it.
	DiffSQL(sql1, sql2 string) (AST, error)
	Close() error
}
```

- [ ] **Step 2: Write the failing test for `astClass`**

Create `internal/engine/ast_test.go`:
```go
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAstClassFromSnapshots(t *testing.T) {
	want := map[string]string{
		// filename (without .json) -> expected class token as polyglot emits it.
		// FILL these in from the Task 3 snapshots (e.g. "Select", "Insert", ...).
		"select": astClassSelect,
		"insert": astClassInsert,
	}
	for name, expect := range want {
		raw, err := os.ReadFile(filepath.Join("testdata", "ast-shapes", name+".json"))
		if err != nil {
			t.Fatalf("read snapshot %s: %v", name, err)
		}
		got, err := astClass(AST(raw))
		if err != nil {
			t.Fatalf("%s: astClass: %v", name, err)
		}
		if got != expect {
			t.Errorf("%s: astClass = %q, want %q", name, got, expect)
		}
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/engine/ -run TestAstClassFromSnapshots`
Expected: FAIL — `astClass`, `astClassSelect`, `astClassInsert` undefined.

- [ ] **Step 4: Implement `ast.go`**

Create `internal/engine/ast.go`. **Set `astClassKey` and the `astClass*` constants to the exact tokens from your Task 3 snapshots** (the values below are the SQLGlot-conventional shape — verify and correct):
```go
package engine

import (
	"encoding/json"
	"fmt"
)

// astClassKey is the top-level JSON field that names a node's kind.
// VERIFY against internal/engine/testdata/ast-shapes/*.json (Task 3).
const astClassKey = "class"

// Class tokens as polyglot emits them. VERIFY/adjust from the snapshots.
const (
	astClassSelect = "Select"
	astClassInsert = "Insert"
	astClassCreate = "Create"
	astClassDrop   = "Drop"
	astClassAlter  = "Alter"
	astClassRename = "RenameTable"
	astClassUse    = "Use"
	astClassGrant  = "Grant"
	astClassCmd    = "Command" // SHOW / EXISTS often land here in SQLGlot
)

// astClass returns the node-kind token of an AST root.
func astClass(ast AST) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(ast, &head); err != nil {
		return "", fmt.Errorf("engine: decode AST head: %w", err)
	}
	raw, ok := head[astClassKey]
	if !ok {
		return "", fmt.Errorf("engine: AST has no %q field", astClassKey)
	}
	var class string
	if err := json.Unmarshal(raw, &class); err != nil {
		return "", fmt.Errorf("engine: decode %q: %w", astClassKey, err)
	}
	return class, nil
}
```

- [ ] **Step 5: Fill in `want` and run the test to pass**

Edit `ast_test.go`'s `want` map to cover all snapshot files using the constants. Run:
`go test ./internal/engine/ -run TestAstClassFromSnapshots -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/engine.go internal/engine/ast.go internal/engine/ast_test.go
git commit -m "feat(engine): Engine interface + AST class discriminator"
```

---

## Task 5: polyglot-backed `Engine` — ParseOne + Generate (round-trip)

**Files:**
- Create: `internal/engine/polyglot.go`
- Create: `internal/engine/polyglot_test.go`

- [ ] **Step 1: Write the failing round-trip test**

Create `internal/engine/polyglot_test.go`:
```go
package engine

import (
	"os"
	"testing"
)

func newTestEngine(t *testing.T) Engine {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatalf("NewPolyglot: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestPolyglotParseGenerateRoundTrip(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("SELECT a FROM db.t")
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	sql, err := e.Generate(ast)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sql == "" {
		t.Fatal("Generate returned empty SQL")
	}
	t.Logf("round-tripped: %s", sql)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/engine/ -run TestPolyglotParseGenerateRoundTrip`
Expected: FAIL — `NewPolyglot` undefined.

- [ ] **Step 3: Implement `polyglot.go` (ParseOne, Generate, Close)**

Create `internal/engine/polyglot.go`:
```go
package engine

import (
	"fmt"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

const dialect = "clickhouse"

type polyglotEngine struct {
	c *polyglot.Client
}

// NewPolyglot loads the FFI lib. If libPath is "", OpenDefault is used
// (honours POLYGLOT_SQL_FFI_PATH and local build dirs).
func NewPolyglot(libPath string) (Engine, error) {
	var (
		c   *polyglot.Client
		err error
	)
	if libPath == "" {
		c, err = polyglot.OpenDefault()
	} else {
		c, err = polyglot.Open(libPath)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: open polyglot: %w", err)
	}
	return &polyglotEngine{c: c}, nil
}

func (e *polyglotEngine) ParseOne(sql string) (AST, error) {
	ast, err := e.c.ParseOne(sql, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: parse: %w", err)
	}
	return AST(ast), nil
}

func (e *polyglotEngine) Generate(ast AST) (string, error) {
	out, err := e.c.Generate(ast, dialect)
	if err != nil {
		return "", fmt.Errorf("engine: generate: %w", err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("engine: generate returned no statements")
	}
	return out[0], nil
}

func (e *polyglotEngine) Close() error { return e.c.Close() }
```

- [ ] **Step 4: Run the test to pass**

Run: `make test` (or set the FFI path) then `go test ./internal/engine/ -run TestPolyglotParseGenerateRoundTrip -v`
Expected: PASS, logs the regenerated SQL.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/polyglot.go internal/engine/polyglot_test.go
git commit -m "feat(engine): polyglot ParseOne/Generate round-trip"
```

---

## Task 6: polyglot-backed `Engine` — RenameTables, QualifyTables, Tokenize, DiffSQL

**Files:**
- Modify: `internal/engine/polyglot.go`
- Modify: `internal/engine/polyglot_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/polyglot_test.go`:
```go
func TestPolyglotRenameTables(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("SELECT a FROM old_table")
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	renamed, err := e.RenameTables(ast, map[string]string{"old_table": "new_table"})
	if err != nil {
		t.Fatalf("RenameTables: %v", err)
	}
	sql, err := e.Generate(renamed)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !contains(sql, "new_table") {
		t.Fatalf("expected new_table in %q", sql)
	}
}

func TestPolyglotTokenizeAndDiff(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Tokenize("SELECT 1"); err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if _, err := e.DiffSQL("SELECT a FROM t", "SELECT b FROM t"); err != nil {
		t.Fatalf("DiffSQL: %v", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/engine/ -run 'TestPolyglotRenameTables|TestPolyglotTokenizeAndDiff'`
Expected: FAIL — `RenameTables`/`QualifyTables`/`Tokenize`/`DiffSQL` not on the type.

- [ ] **Step 3: Implement the four methods**

Append to `internal/engine/polyglot.go`:
```go
func (e *polyglotEngine) RenameTables(ast AST, mapping map[string]string) (AST, error) {
	out, err := e.c.RenameTables(ast, mapping, polyglot.RenameTablesOptions{})
	if err != nil {
		return nil, fmt.Errorf("engine: rename tables: %w", err)
	}
	return AST(out), nil
}

func (e *polyglotEngine) QualifyTables(ast AST, db string) (AST, error) {
	out, err := e.c.QualifyTables(ast, polyglot.QualifyTablesOptions{Dialect: dialect, DB: db})
	if err != nil {
		return nil, fmt.Errorf("engine: qualify tables: %w", err)
	}
	return AST(out), nil
}

func (e *polyglotEngine) Tokenize(sql string) (AST, error) {
	out, err := e.c.Tokenize(sql, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: tokenize: %w", err)
	}
	return AST(out), nil
}

func (e *polyglotEngine) DiffSQL(sql1, sql2 string) (AST, error) {
	out, err := e.c.Diff(sql1, sql2, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: diff: %w", err)
	}
	return AST(out), nil
}
```

- [ ] **Step 4: Run to pass**

Run: `make test` then `go test ./internal/engine/ -run 'TestPolyglotRenameTables|TestPolyglotTokenizeAndDiff' -v`
Expected: PASS. (If `RenameTables` doesn't surface `new_table`, the mapping key may need qualifying — note it as a Task 11 finding; do not block.)

- [ ] **Step 5: Commit**

```bash
git add internal/engine/polyglot.go internal/engine/polyglot_test.go
git commit -m "feat(engine): RenameTables/QualifyTables/Tokenize/DiffSQL"
```

---

## Task 7: Generate the `pb` contract types

**Files:**
- Create: `proto/rewriter.proto` (pinned copy + `go_package`)
- Create: `buf.yaml`, `buf.gen.yaml`
- Create (generated): `gen/pb/*.go`

- [ ] **Step 1: Vendor the proto with a `go_package` option**

Run:
```bash
mkdir -p proto
cp ../rewriter-grpc/protos/rewriter.proto proto/rewriter.proto
```
Then edit `proto/rewriter.proto`: directly under the `package rewriter;` line, add:
```proto
option go_package = "github.com/housegate/rewriter-go/gen/pb;pb";
```

- [ ] **Step 2: Add buf config**

Create `buf.yaml`:
```yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

Create `buf.gen.yaml`:
```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: gen/pb
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: gen/pb
    opt: paths=source_relative
```

- [ ] **Step 3: Install codegen tools**

Run:
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# buf: brew install bufbuild/buf/buf   (or see https://buf.build/docs/installation)
```

- [ ] **Step 4: Generate and build**

Run:
```bash
make proto
go mod tidy
go build ./gen/...
```
Expected: `gen/pb/rewriter.pb.go` + `gen/pb/rewriter_grpc.pb.go` exist and compile.

- [ ] **Step 5: Commit**

```bash
git add proto/ buf.yaml buf.gen.yaml gen/ go.mod go.sum
git commit -m "build: vendor rewriter.proto + generate pb (messages + grpc client)"
```

---

## Task 8: Corpus loader

**Files:**
- Create: `internal/corpus/corpus.go`
- Create: `internal/corpus/corpus_test.go`
- Create: `internal/corpus/testdata/seed.sql`

- [ ] **Step 1: Write the seed corpus**

Create `internal/corpus/testdata/seed.sql` (statements separated by a line containing only `---`):
```sql
SELECT a FROM db.t WHERE x IN (1, 2, 3)
---
SELECT * FROM a GLOBAL JOIN b ON a.id = b.id
---
INSERT INTO db.t (a) VALUES (1)
---
CREATE TABLE db.t (a Int64) ENGINE = MergeTree ORDER BY a
---
DROP TABLE IF EXISTS db.t
---
ALTER TABLE db.t ADD COLUMN b Int64
---
RENAME TABLE db.a TO db.b
---
USE db
---
SHOW TABLES FROM db
---
SHOW CREATE TABLE db.t
---
EXISTS TABLE db.t
---
GRANT SELECT ON db.t TO u
```

- [ ] **Step 2: Write the failing test**

Create `internal/corpus/corpus_test.go`:
```go
package corpus

import "testing"

func TestLoadSeed(t *testing.T) {
	cases, err := Load("testdata/seed.sql")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 12 {
		t.Fatalf("got %d cases, want 12", len(cases))
	}
	if cases[0] != "SELECT a FROM db.t WHERE x IN (1, 2, 3)" {
		t.Fatalf("first case = %q", cases[0])
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/corpus/`
Expected: FAIL — `Load` undefined.

- [ ] **Step 4: Implement `corpus.go`**

Create `internal/corpus/corpus.go`:
```go
// Package corpus loads SQL test corpora. Files are UTF-8 SQL with statements
// separated by a line containing only "---".
package corpus

import (
	"os"
	"strings"
)

// Load reads one corpus file and returns its statements (trimmed, non-empty).
func Load(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, block := range strings.Split(string(raw), "\n---\n") {
		s := strings.TrimSpace(block)
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Run to pass**

Run: `go test ./internal/corpus/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/corpus/
git commit -m "feat(corpus): ---delimited SQL corpus loader + seed"
```

---

## Task 9: Fidelity metric (parse coverage + round-trip idempotence)

The spike's core measurement: can polyglot faithfully represent our SQL? Metric per statement — (1) does it parse; (2) is generation idempotent (`gen(parse(sql)) == gen(parse(gen(parse(sql))))`). Non-idempotence or parse failure = a fidelity gap.

**Files:**
- Create: `internal/engine/fidelity.go`
- Create: `internal/engine/fidelity_test.go`

- [ ] **Step 1: Write the failing test (with a fake engine — no FFI needed)**

Create `internal/engine/fidelity_test.go`:
```go
package engine

import "testing"

// fakeEngine lets us unit-test the metric logic deterministically.
type fakeEngine struct {
	parseErr map[string]bool
	gen      map[string]string // sql -> canonical generated form
}

func (f *fakeEngine) ParseOne(sql string) (AST, error) {
	if f.parseErr[sql] {
		return nil, errFake
	}
	return AST(`{"sql":"` + sql + `"}`), nil
}
func (f *fakeEngine) Generate(ast AST) (string, error) {
	// AST carries the original sql; map it through gen.
	var head struct {
		SQL string `json:"sql"`
	}
	_ = jsonUnmarshal(ast, &head)
	if g, ok := f.gen[head.SQL]; ok {
		return g, nil
	}
	return head.SQL, nil
}
func (f *fakeEngine) RenameTables(a AST, m map[string]string) (AST, error) { return a, nil }
func (f *fakeEngine) QualifyTables(a AST, db string) (AST, error)          { return a, nil }
func (f *fakeEngine) Tokenize(string) (AST, error)                         { return AST("[]"), nil }
func (f *fakeEngine) DiffSQL(string, string) (AST, error)                  { return AST("{}"), nil }
func (f *fakeEngine) Close() error                                         { return nil }

func TestCheckFidelity(t *testing.T) {
	f := &fakeEngine{
		parseErr: map[string]bool{"BAD SQL": true},
		gen: map[string]string{
			// idempotent: parse->gen yields a stable canonical form
			"SELECT 1": "SELECT 1",
			// non-idempotent: first gen differs, second gen stabilizes
			"SELECT a,b": "SELECT a, b",
		},
	}
	if got := CheckFidelity(f, "SELECT 1"); got.Status != FidelityOK {
		t.Errorf("SELECT 1: %v, want OK", got.Status)
	}
	if got := CheckFidelity(f, "BAD SQL"); got.Status != FidelityParseError {
		t.Errorf("BAD SQL: %v, want ParseError", got.Status)
	}
	if got := CheckFidelity(f, "SELECT a,b"); got.Status != FidelityNonIdempotent {
		t.Errorf("SELECT a,b: %v, want NonIdempotent", got.Status)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/engine/ -run TestCheckFidelity`
Expected: FAIL — `CheckFidelity`, `FidelityOK`, etc. undefined.

- [ ] **Step 3: Implement `fidelity.go`**

Create `internal/engine/fidelity.go`:
```go
package engine

import (
	"encoding/json"
	"errors"
)

var errFake = errors.New("fake parse error")

// jsonUnmarshal is a tiny indirection so tests can share it.
func jsonUnmarshal(b AST, v any) error { return json.Unmarshal(b, v) }

type FidelityStatus int

const (
	FidelityOK            FidelityStatus = iota // parses and round-trip is idempotent
	FidelityParseError                          // ParseOne failed
	FidelityGenerateError                       // Generate failed
	FidelityNonIdempotent                       // gen1 != gen2 (information lost/mangled)
)

func (s FidelityStatus) String() string {
	switch s {
	case FidelityOK:
		return "OK"
	case FidelityParseError:
		return "ParseError"
	case FidelityGenerateError:
		return "GenerateError"
	case FidelityNonIdempotent:
		return "NonIdempotent"
	}
	return "Unknown"
}

type FidelityResult struct {
	SQL     string
	Status  FidelityStatus
	Gen1    string
	Gen2    string
	Err     string
}

// CheckFidelity measures whether the engine faithfully represents one statement.
func CheckFidelity(e Engine, sql string) FidelityResult {
	r := FidelityResult{SQL: sql}
	a1, err := e.ParseOne(sql)
	if err != nil {
		r.Status, r.Err = FidelityParseError, err.Error()
		return r
	}
	g1, err := e.Generate(a1)
	if err != nil {
		r.Status, r.Err = FidelityGenerateError, err.Error()
		return r
	}
	r.Gen1 = g1
	a2, err := e.ParseOne(g1)
	if err != nil {
		r.Status, r.Err = FidelityParseError, err.Error()
		return r
	}
	g2, err := e.Generate(a2)
	if err != nil {
		r.Status, r.Err = FidelityGenerateError, err.Error()
		return r
	}
	r.Gen2 = g2
	if g1 != g2 {
		r.Status = FidelityNonIdempotent
		return r
	}
	r.Status = FidelityOK
	return r
}
```

- [ ] **Step 4: Run to pass**

Run: `go test ./internal/engine/ -run TestCheckFidelity -v`
Expected: PASS (no FFI needed — uses the fake).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/fidelity.go internal/engine/fidelity_test.go
git commit -m "feat(engine): fidelity metric (parse coverage + round-trip idempotence)"
```

---

## Task 10: Public `Rewriter` interface, `RewriteResult`, pass-through `NativeRewriter`

**Files:**
- Create: `rewriter.go`
- Create: `native.go`
- Create: `native_test.go`

- [ ] **Step 1: Define the public interface and result type**

Create `rewriter.go`:
```go
// Package rewriter is a native Go ClickHouse SQL rewriter, an in-process
// alternative to the C++ rewriter-grpc service. Phase 0 ships a PASS-THROUGH
// implementation: it parses + classifies + regenerates, but applies no
// rewrites. Statement-specific rewriting arrives in later phases.
package rewriter

import (
	"context"

	"github.com/housegate/rewriter-go/gen/pb"
)

// RewriteResult mirrors pb.RewriteSQLResponse with interface-friendly names.
type RewriteResult struct {
	SQL                    string
	Code                   pb.RewriteCode
	Message                string
	StatementType          pb.StatementType
	TableRewrites          map[string]string
	DatabaseRewrites       map[string]string
	OriginalAccessedTables []*pb.AccessedTable
	PrivilegesDeltas       []*pb.PrivilegeDelta
	ExistenceClause        pb.ExistenceClause
	FailedCTEAliases       []string
}

// Rewriter rewrites Sentio-Network mode SQL into real SQL, bound to one client
// connection. Fail-open: a non-nil error means forward the original SQL.
type Rewriter interface {
	Rewrite(ctx context.Context, sql, effectiveAccount string) (RewriteResult, error)
	RewriteErrorMessage(ctx context.Context, message string) (string, error)
	Close() error
}

func resultFromPB(r *pb.RewriteSQLResponse) RewriteResult {
	return RewriteResult{
		SQL:                    r.GetSqlAfterRewrite(),
		Code:                   r.GetCode(),
		Message:                r.GetMessage(),
		StatementType:          r.GetStatementType(),
		TableRewrites:          r.GetTableRewrites(),
		DatabaseRewrites:       r.GetDatabaseRewrites(),
		OriginalAccessedTables: r.GetOriginalAccessedTables(),
		PrivilegesDeltas:       r.GetPrivilegesDeltas(),
		ExistenceClause:        r.GetExistenceClause(),
		FailedCTEAliases:       r.GetFailedCteAliases(),
	}
}
```

> If `go build` reports a getter name mismatch (e.g. `GetFailedCteAliases`), use the exact getter from `gen/pb/rewriter.pb.go` — proto field `failed_cte_aliases` → `GetFailedCteAliases`. Adjust to whatever was generated.

- [ ] **Step 2: Write the failing pass-through test**

Create `native_test.go`:
```go
package rewriter

import (
	"context"
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

func newNative(t *testing.T) *NativeRewriter {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return New(e)
}

func TestPassThroughClassifiesAndEchoes(t *testing.T) {
	r := newNative(t)
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT a FROM db.t", "acct")
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v, want Success", res.Code)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("stmt = %v, want SELECT", res.StatementType)
	}
	if res.SQL == "" {
		t.Fatal("SQL must always be set on success")
	}
}

func TestSyntaxErrorIsCodeNotGoError(t *testing.T) {
	r := newNative(t)
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT FROM", "acct")
	if err != nil {
		t.Fatalf("syntax error must be returned in code, not as Go error: %v", err)
	}
	if res.Code != pb.RewriteCode_SyntaxError {
		t.Fatalf("code = %v, want SyntaxError", res.Code)
	}
	if res.SQL != "SELECT FROM" {
		t.Fatalf("on non-Success, SQL must echo input; got %q", res.SQL)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test . -run 'TestPassThrough|TestSyntaxError'`
Expected: FAIL — `New`/`NativeRewriter` undefined.

- [ ] **Step 4: Implement `native.go`**

Create `native.go`. **Map class tokens using the constants verified in Task 4.** `astClass` is unexported in `engine`, so expose a small classifier there first — add to `internal/engine/ast.go`:
```go
// Classify maps an AST root to a pb.StatementType-friendly token string.
// Returns the raw class token; callers map it to their enum.
func Classify(ast AST) (string, error) { return astClass(ast) }
```
Then create `native.go`:
```go
package rewriter

import (
	"context"
	"sync"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// NativeRewriter is the in-process Rewriter. Phase 0 = pass-through.
type NativeRewriter struct {
	engine engine.Engine
	mu     sync.Mutex
	last   *callContext
}

type callContext struct {
	sql     string
	account string
}

// New builds a pass-through NativeRewriter over the given engine.
func New(e engine.Engine) *NativeRewriter { return &NativeRewriter{engine: e} }

func (r *NativeRewriter) Rewrite(_ context.Context, sql, account string) (RewriteResult, error) {
	resp := &pb.RewriteSQLResponse{SqlAfterRewrite: sql} // SQL always set; echoes input
	ast, err := r.engine.ParseOne(sql)
	if err != nil {
		resp.Code = pb.RewriteCode_SyntaxError
		resp.Message = err.Error()
		return resultFromPB(resp), nil // SyntaxError is a code, not a Go error
	}
	if class, cerr := engine.Classify(ast); cerr == nil {
		resp.StatementType = classToStatementType(class)
	}
	// Pass-through: regenerate (proves the engine round-trips); fall back to
	// the input on any generate hiccup so SQL is always runnable.
	if gen, gerr := r.engine.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
	resp.Code = pb.RewriteCode_Success
	r.mu.Lock()
	r.last = &callContext{sql: sql, account: account}
	r.mu.Unlock()
	return resultFromPB(resp), nil
}

func (r *NativeRewriter) RewriteErrorMessage(_ context.Context, message string) (string, error) {
	// Phase 0: no reverse-mapping yet (arrives in Phase 5). Echo the message.
	return message, nil
}

func (r *NativeRewriter) Close() error {
	r.mu.Lock()
	r.last = nil
	r.mu.Unlock()
	return r.engine.Close()
}

// classToStatementType maps polyglot class tokens to pb.StatementType.
// VERIFY the class tokens against Task 3/4 snapshots.
func classToStatementType(class string) pb.StatementType {
	switch class {
	case "Select":
		return pb.StatementType_STATEMENT_TYPE_SELECT
	case "Insert":
		return pb.StatementType_STATEMENT_TYPE_INSERT
	case "Create":
		return pb.StatementType_STATEMENT_TYPE_CREATE_TABLE
	case "Drop":
		return pb.StatementType_STATEMENT_TYPE_DROP_TABLE
	case "Alter":
		return pb.StatementType_STATEMENT_TYPE_ALTER_TABLE
	case "RenameTable":
		return pb.StatementType_STATEMENT_TYPE_RENAME_TABLE
	case "Use":
		return pb.StatementType_STATEMENT_TYPE_USE
	default:
		return pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	}
}
```

- [ ] **Step 5: Run to pass**

Run: `make test` then `go test . -run 'TestPassThrough|TestSyntaxError' -v`
Expected: PASS. (If `STATEMENT_TYPE_SELECT` doesn't match because the class token differs, fix `classToStatementType` from the snapshot — that's exactly the Task 3 payoff.)

- [ ] **Step 6: Commit**

```bash
git add rewriter.go native.go native_test.go internal/engine/ast.go
git commit -m "feat: public Rewriter + pass-through NativeRewriter (code-not-error, fail-open)"
```

---

## Task 11: Differential harness — comparator + env-gated oracle client

**Files:**
- Create: `internal/harness/compare.go`
- Create: `internal/harness/compare_test.go`
- Create: `internal/harness/oracle.go`

- [ ] **Step 1: Write the failing comparator test (no network)**

Create `internal/harness/compare_test.go`:
```go
package harness

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestCompareExactFields(t *testing.T) {
	a := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{"db.t": "p.db_t"},
	}
	b := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{"db.t": "p.db_t"},
	}
	if d := Compare(a, b, nil); !d.Equal() {
		t.Fatalf("identical responses should match, got: %v", d.Mismatches)
	}

	b.TableRewrites["db.t"] = "DIFFERENT"
	if d := Compare(a, b, nil); d.Equal() {
		t.Fatal("differing table_rewrites should mismatch")
	}
}

func TestCompareSQLSemanticSkippedWithoutEngine(t *testing.T) {
	a := &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, SqlAfterRewrite: "SELECT a FROM t"}
	b := &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, SqlAfterRewrite: "SELECT  a  FROM  t"}
	// nil semanticEq -> falls back to exact string compare, so these differ.
	if d := Compare(a, b, nil); d.Equal() {
		t.Fatal("without semantic compare, whitespace differences mismatch")
	}
	// With a semantic equality fn that ignores whitespace, they match.
	eq := func(s1, s2 string) (bool, error) { return normalizeWS(s1) == normalizeWS(s2), nil }
	if d := Compare(a, b, eq); !d.Equal() {
		t.Fatalf("with semantic compare, should match; got %v", d.Mismatches)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/harness/`
Expected: FAIL — `Compare`, `Diff`, `normalizeWS` undefined.

- [ ] **Step 3: Implement `compare.go`**

Create `internal/harness/compare.go`:
```go
// Package harness runs the native rewriter and the C++ oracle over a corpus and
// diffs their responses. Comparison is exact for structured fields and
// semantic (caller-supplied) for sql_after_rewrite.
package harness

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
)

// SemanticEq reports whether two SQL strings are semantically equal.
type SemanticEq func(sql1, sql2 string) (bool, error)

type Diff struct {
	Mismatches []string
}

func (d Diff) Equal() bool { return len(d.Mismatches) == 0 }

// Compare diffs two responses. If semanticEq is nil, sql_after_rewrite is
// compared as an exact string.
func Compare(got, want *pb.RewriteSQLResponse, semanticEq SemanticEq) Diff {
	var d Diff
	add := func(f string, a, b any) { d.Mismatches = append(d.Mismatches, fmt.Sprintf("%s: got %v, want %v", f, a, b)) }

	if got.GetCode() != want.GetCode() {
		add("code", got.GetCode(), want.GetCode())
	}
	if got.GetStatementType() != want.GetStatementType() {
		add("statement_type", got.GetStatementType(), want.GetStatementType())
	}
	if got.GetExistenceClause() != want.GetExistenceClause() {
		add("existence_clause", got.GetExistenceClause(), want.GetExistenceClause())
	}
	if !reflect.DeepEqual(got.GetTableRewrites(), want.GetTableRewrites()) {
		add("table_rewrites", got.GetTableRewrites(), want.GetTableRewrites())
	}
	if !reflect.DeepEqual(got.GetDatabaseRewrites(), want.GetDatabaseRewrites()) {
		add("database_rewrites", got.GetDatabaseRewrites(), want.GetDatabaseRewrites())
	}
	if !reflect.DeepEqual(got.GetFailedCteAliases(), want.GetFailedCteAliases()) {
		add("failed_cte_aliases", got.GetFailedCteAliases(), want.GetFailedCteAliases())
	}
	// sql_after_rewrite
	gs, ws := got.GetSqlAfterRewrite(), want.GetSqlAfterRewrite()
	if semanticEq == nil {
		if gs != ws {
			add("sql_after_rewrite(exact)", gs, ws)
		}
	} else if eq, err := semanticEq(gs, ws); err != nil {
		add("sql_after_rewrite(semantic-error)", err.Error(), "")
	} else if !eq {
		add("sql_after_rewrite(semantic)", gs, ws)
	}
	return d
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }
```

> `original_accessed_tables` and `privileges_deltas` comparison (repeated messages) is added in the phase that first populates them (Phase 1 / Phase 4) — Phase 0's pass-through never sets them, so leaving them out keeps the comparator honest about what it actually checks.

- [ ] **Step 4: Run to pass**

Run: `go test ./internal/harness/ -run TestCompare -v`
Expected: PASS.

- [ ] **Step 5: Add the env-gated oracle client**

Create `internal/harness/oracle.go`:
```go
package harness

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/housegate/rewriter-go/gen/pb"
)

// OracleAddrEnv names the env var pointing at a running rewriter-grpc service.
const OracleAddrEnv = "REWRITER_ORACLE_ADDR"

// Oracle is a thin gRPC client to the C++ rewriter-grpc service.
type Oracle struct {
	conn   *grpc.ClientConn
	client pb.RewriterServiceClient
}

// DialOracle connects to REWRITER_ORACLE_ADDR. Returns (nil, nil) when the env
// var is unset, so callers can skip oracle-backed comparisons gracefully.
func DialOracle() (*Oracle, error) {
	addr := os.Getenv(OracleAddrEnv)
	if addr == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("harness: dial oracle %s: %w", addr, err)
	}
	return &Oracle{conn: conn, client: pb.NewRewriterServiceClient(conn)}, nil
}

func (o *Oracle) Rewrite(sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return o.client.Rewrite(ctx, &pb.RewriteSQLRequest{Sql: sql, Options: opts})
}

func (o *Oracle) Close() error {
	if o == nil || o.conn == nil {
		return nil
	}
	return o.conn.Close()
}
```

- [ ] **Step 6: Build and commit**

Run: `go build ./... && go test ./internal/harness/ -v`
Expected: build clean, comparator tests PASS.
```bash
go mod tidy
git add internal/harness/ go.mod go.sum
git commit -m "feat(harness): response comparator + env-gated rewriter-grpc oracle client"
```

---

## Task 12: Fidelity-spike command + divergence report

**Files:**
- Create: `cmd/fidelity-spike/main.go`
- Create (output, committed): `docs/superpowers/reports/2026-06-03-phase0-fidelity.md`

- [ ] **Step 1: Implement the spike command**

Create `cmd/fidelity-spike/main.go`:
```go
// Command fidelity-spike runs the corpus through the polyglot engine and reports
// parse coverage + round-trip idempotence — the go/no-go signal for full parity.
//
// Usage: POLYGLOT_SQL_FFI_PATH=... go run ./cmd/fidelity-spike <corpus.sql>...
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/housegate/rewriter-go/internal/corpus"
	"github.com/housegate/rewriter-go/internal/engine"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fidelity-spike <corpus.sql>...")
		os.Exit(2)
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		os.Exit(1)
	}
	defer e.Close()

	var all []string
	for _, path := range os.Args[1:] {
		cases, err := corpus.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load %s: %v\n", path, err)
			os.Exit(1)
		}
		all = append(all, cases...)
	}

	counts := map[engine.FidelityStatus]int{}
	var problems []engine.FidelityResult
	for _, sql := range all {
		r := engine.CheckFidelity(e, sql)
		counts[r.Status]++
		if r.Status != engine.FidelityOK {
			problems = append(problems, r)
		}
	}

	total := len(all)
	fmt.Printf("# Phase 0 fidelity spike\n\n")
	fmt.Printf("- corpus statements: %d\n", total)
	for _, s := range []engine.FidelityStatus{engine.FidelityOK, engine.FidelityParseError, engine.FidelityGenerateError, engine.FidelityNonIdempotent} {
		fmt.Printf("- %s: %d (%.1f%%)\n", s, counts[s], pct(counts[s], total))
	}
	if len(problems) > 0 {
		fmt.Printf("\n## Divergences (the parity punch-list)\n\n")
		sort.Slice(problems, func(i, j int) bool { return problems[i].Status < problems[j].Status })
		for _, p := range problems {
			fmt.Printf("### [%s] `%s`\n", p.Status, p.SQL)
			if p.Err != "" {
				fmt.Printf("- error: %s\n", p.Err)
			}
			if p.Status == engine.FidelityNonIdempotent {
				fmt.Printf("- gen1: `%s`\n- gen2: `%s`\n", p.Gen1, p.Gen2)
			}
			fmt.Println()
		}
	}
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}
```

- [ ] **Step 2: Build it**

Run: `go build ./cmd/fidelity-spike`
Expected: builds clean.

- [ ] **Step 3: Run the spike over the seed corpus and capture the report**

Run:
```bash
mkdir -p docs/superpowers/reports
make ffi   # ensure the lib exists
POLYGLOT_SQL_FFI_PATH=$(pwd)/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so) \
  go run ./cmd/fidelity-spike internal/corpus/testdata/seed.sql \
  > docs/superpowers/reports/2026-06-03-phase0-fidelity.md
cat docs/superpowers/reports/2026-06-03-phase0-fidelity.md
```
Expected: a markdown report with per-status counts and a divergence list.

- [ ] **Step 4: Write the go/no-go note**

Append a short verdict section to `docs/superpowers/reports/2026-06-03-phase0-fidelity.md` summarizing: parse-coverage %, idempotence %, the top divergence categories, and a recommendation (proceed to Phase 1 / expand corpus / reconsider substrate). Base it on the actual numbers.

- [ ] **Step 5: Commit**

```bash
git add cmd/fidelity-spike/ docs/superpowers/reports/
git commit -m "feat(spike): fidelity-spike command + Phase 0 divergence report"
```

---

## Task 13: Phase 0 wrap-up — README + CI hook + branch PR

**Files:**
- Modify: `README.md`
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Update the README status + how-to-run**

Append to `README.md` a "Building & testing" section documenting: `make ffi` (build the native lib), `make proto` (regen pb), `make test` (run all Go tests with the FFI path exported), and `go run ./cmd/fidelity-spike <corpus>`.

- [ ] **Step 2: Add a minimal CI workflow**

Create `.github/workflows/ci.yml`:
```yaml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.22' }
      - uses: dtolnay/rust-toolchain@stable
      - name: Build FFI lib
        run: make ffi
      - name: Test
        run: make test
        env:
          CGO_ENABLED: '0'
```

- [ ] **Step 3: Verify the full suite green locally**

Run: `make test`
Expected: all packages PASS (FFI-dependent tests run; none skipped).

- [ ] **Step 4: Commit and open the PR**

```bash
git add README.md .github/
git commit -m "ci: build FFI + run tests; document Phase 0 dev loop"
git push -u origin feat/phase-0-engine-harness
gh pr create --fill --base main --title "Phase 0: engine + harness + fidelity spike"
```

---

## Self-Review

**Spec coverage (Phase 0 scope per spec §9):**
- Engine adapter over polyglot → Tasks 4–6. ✅
- FFI-lib build/vendor for CI → Tasks 2, 13. ✅
- Generated `pb` contract types → Task 7. ✅
- Differential-harness skeleton → Task 11. ✅
- Pass-through native impl exercising the interface → Task 10. ✅
- Fidelity spike → Tasks 9, 12. ✅
- Engine seam isolates polyglot (spec §4) → enforced by the `internal/engine`-only import rule (Task 4 doc comment). ✅
- Error-vs-code contract (spec §8/§11) → Task 10 `TestSyntaxErrorIsCodeNotGoError`. ✅
- Session-held state (spec §4/§11) → **deferred to Phase 1+** (pass-through has no session); noted, not silently dropped. ✅

**Placeholder scan:** No "TBD"/"implement later". The two "VERIFY against snapshot" notes (`astClassKey`, class tokens) are deliberate, with the exact file to check and a one-line fix point — not placeholders. The `want` map in Task 4 Step 5 is explicitly filled from the snapshots during execution.

**Type consistency:** `Engine` methods (`ParseOne`/`Generate`/`RenameTables`/`QualifyTables`/`Tokenize`/`DiffSQL`/`Close`) match across the interface (Task 4), the polyglot impl (Tasks 5–6), and the fake (Task 9). `engine.Classify` (Task 10 Step 4) wraps `astClass` (Task 4). `Compare`/`Diff`/`SemanticEq`/`normalizeWS` consistent within Task 11. `resultFromPB` getters flagged for exact-name verification against generated code (Task 10 Step 1 note).

---

## Subsequent phases (planned after the Phase 0 report)

Phases 1–5 (SELECT; writes; db-level; exists/show-create/grant; error-reverse-mapping + hardening) are **intentionally not broken into tasks yet**: Phase 0's divergence report is what tells us *where* polyglot diverges, which reorders and reshapes those tasks. Each gets its own `docs/superpowers/plans/<date>-...-phase-N.md`, written once the report exists, and each is gated by the differential harness (now built) going green on its corpus slice.
