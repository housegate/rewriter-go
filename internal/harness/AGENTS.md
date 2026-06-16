# HARNESS PACKAGE KNOWLEDGE

## OVERVIEW

`internal/harness` is the parity gate: it compares native output to frozen JSON corpora and, when configured, to a live `rewriter-grpc` oracle.

## WHERE TO LOOK

| Task | Location | Notes |
| --- | --- | --- |
| Response diffing | `compare.go` | Field-by-field compare, semantic SQL hook, nil/empty map equivalence |
| Oracle client | `oracle.go` | `REWRITER_ORACLE_ADDR` gRPC client |
| SELECT corpus | `select_golden_test.go`, `testdata/select_cases.json` | Table rewrite, CTE, limit/offset/settings |
| Write corpus | `writes_golden_test.go`, `testdata/writes_cases.json` | DDL/write parity and allowed divergences |
| DB-level corpus | `dblevel_golden_test.go`, `testdata/dblevel_cases.json` | USE/SHOW/CREATE/DROP database |
| Phase 4 corpus | `phase4_golden_test.go`, `testdata/phase4_cases.json` | EXISTS/SHOW CREATE/GRANT behavior |
| Error-message corpus | `errmsg_golden_test.go`, `testdata/errmsg_cases.json` | `RewriteErrorMessage` inversion |
| Fuzzing | `fuzz_test.go`, `testdata/fuzz` | Fail-open and no-panic contract |

## CONVENTIONS

- Golden JSON is native frozen output plus explicit divergence flags; live oracle comparison is the stronger parity gate when `REWRITER_ORACLE_ADDR` is set.
- Tests that require the engine skip when `POLYGLOT_SQL_FFI_PATH` is unset.
- `Compare` treats nil and empty maps as equal because proto3 wire output from the C++ oracle may differ from native initialized maps.
- SQL comparison should use semantic AST diff when formatting differences are expected.
- Allow-list fields narrowly. Flags such as `allow_*_divergence` should exempt only the field documented by the test.
- Keep JSON fixture structs close to the test that consumes them; this package intentionally does not centralize all fixture decoding.
- Per-case JSON schemas use `want_*` fields plus narrow `allow_*_divergence` flags; keep each schema beside its consuming golden test.
- `dblevel_golden_test.go` has a temporary reject `sql_after_rewrite` echo carve-out pending oracle verification; do not generalize it to other fields or corpora.

## ANTI-PATTERNS

- Do not broaden a divergence allow-list to make a corpus pass.
- Do not make oracle failures mandatory for normal local runs; `REWRITER_ORACLE_ADDR` is optional.
- Do not inspect zero `RewriteResult` values after internal fail-open paths in fuzz tests.
- Do not remove fuzz seeds or golden cases to get a green run.

## LOCAL VERIFICATION

```bash
go test ./internal/harness
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" go test ./internal/harness -count=1
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1
go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 30s
```
