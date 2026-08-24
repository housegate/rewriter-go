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
| Storage-integrity corpus | `storage_integrity_golden_test.go`, `testdata/storage_integrity_cases.json` | Shared Go/C++ Spec G contract and response goldens |
| Error-message corpus | `errmsg_golden_test.go`, `testdata/errmsg_cases.json` | `RewriteErrorMessage` inversion |
| Fuzzing | `fuzz_test.go`, `testdata/fuzz` | Fail-open and no-panic contract |
| Root entrypoint parity | `../../native_test.go`, `../../service_test.go` | Public API and stateless service behavior; engine-gated when FFI is required |

## CONVENTIONS

- Golden JSON is native frozen output plus explicit divergence flags; live oracle comparison is the stronger parity gate when `REWRITER_ORACLE_ADDR` is set.
- Tests that require the engine skip when `POLYGLOT_SQL_FFI_PATH` is unset.
- Plain `go test ./...` therefore proves only pure-Go and skipped-engine behavior. Use `make test` or set `POLYGLOT_SQL_FFI_PATH` before claiming FFI-backed parity.
- `Compare` treats nil and empty maps as equal because proto3 wire output from the C++ oracle may differ from native initialized maps.
- SQL comparison should use semantic AST diff when formatting differences are expected, except for the storage-integrity corpus's exact-after-normalization contract below.
- Allow-list fields narrowly. Flags such as `allow_*_divergence` should exempt only the field documented by the test.
- Keep JSON fixture structs close to the test that consumes them; this package intentionally does not centralize all fixture decoding except for the shared storage-integrity schema in `sicorpus_test.go`.
- Per-case JSON schemas use `want_*` fields plus narrow `allow_*_divergence` flags; keep each schema beside its consuming golden test except for the Go/C++ storage-integrity contract below.
- `dblevel_golden_test.go` has a temporary reject `sql_after_rewrite` echo carve-out pending oracle verification; do not generalize it to other fields or corpora.

## STORAGE-INTEGRITY CORPUS CONTRACT

`testdata/storage_integrity_cases.json` is the frozen Go/C++ behaviour
contract. Its published counterpart is
`rewriter-grpc/tests/testdata/storage_integrity_cases.json`. The two files must
be byte-identical, but each repository can enforce only its own local copy:
`TestSICorpusIsBytePinned` checks the local FNV-1a/64 fingerprint, byte count,
and case count. Cross-repository identity is a paired-PR discipline enforced by
an explicit byte comparison and the same recorded SHA-256 in both PRs.

Schema and semantic rules live in `sicorpus_test.go` (`SICase`, the strict
loader, and `ValidateSICorpus`). `TestSICorpusContract` enforces them over the
published corpus.

- Every case is either a reject (`want_code != "Success"`, `reject: true`, and
  a non-empty `want_message_contains`, with no SQL pin) or a success that pins
  SQL exactly.
- A success pins one `want_sql` when both engines agree, or sets
  `allow_sql_divergence: true` with both `want_sql_go` and `want_sql_cpp`.
  There is no `sql_exact`; every success is compared exactly after
  `NormalizeSIIdentifierQuotes`.
- The normalizer changes only supported identifier quoting. It preserves
  string literals and returns the original SQL when it cannot safely classify
  comments, dollar quotes, `INSERT ... FORMAT` payloads, or malformed quoting.
  Never replace it with a global backtick substitution.
- `want_sql_contains` and `want_sql_not_contains` are extra assertions only.
  A contains entry already present in the input, including after identifier
  normalization, is a hard vacuity violation.
- Unknown JSON keys and content after the single corpus JSON value fail the
  strict load. Do not silently accept old keys such as `sql_exact`.

Regenerate SQL pins only with both engines available. `UPDATE_GOLDEN` is
enabled only by the exact value `1`, and the scoped command must provide the
FFI path explicitly because a separate `make ffi` process cannot export it to
the following shell command:

```bash
make ffi
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  UPDATE_GOLDEN=1 REWRITER_ORACLE_ADDR=<host:port> \
  go test ./internal/harness -run '^TestStorageIntegrityGolden$' -count=1
```

The writer uses deterministic JSON encoding with HTML escaping disabled. After
reviewing the regenerated pins, copy the file verbatim to rewriter-grpc,
update `SICorpusFingerprint` / `SICorpusBytes` / `SICorpusCases` here and
`kCorpusFingerprint` / `kCorpusBytes` / `kCorpusCases` there, then prove and
record the paired identity:

```bash
cmp -s internal/harness/testdata/storage_integrity_cases.json \
  <rewriter-grpc-checkout>/tests/testdata/storage_integrity_cases.json
shasum -a 256 internal/harness/testdata/storage_integrity_cases.json \
  <rewriter-grpc-checkout>/tests/testdata/storage_integrity_cases.json
go test ./internal/harness -run 'TestSICorpus(Contract|IsBytePinned)$' -count=1
```

## GOLDEN FIXTURES

Default test runs do not write tracked fixtures. `TestCharacterizeAST`
compares the current polyglot AST to `internal/engine/testdata/ast-shapes/` and
regenerates only under `UPDATE_GOLDEN=1`. Its package-local `updateGoldenEnv`
duplicates `harness.UpdateGoldenEnv` deliberately because `internal/harness`
imports `internal/engine`.

Run AST regeneration as a package-scoped command so it does not also invoke
the corpus regenerator, which requires the C++ oracle:

```bash
make ffi
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  UPDATE_GOLDEN=1 go test ./internal/engine -run '^TestCharacterizeAST$' -count=1
```

CI runs `git diff --exit-code --ignore-submodules=all` after `make test`; a
default test run that mutates any tracked fixture fails the FFI job.

## ANTI-PATTERNS

- Do not broaden a divergence allow-list to make a corpus pass.
- Do not make oracle failures mandatory for normal local runs; `REWRITER_ORACLE_ADDR` is optional.
- Do not inspect zero `RewriteResult` values after internal fail-open paths in fuzz tests.
- Do not call a corpus update "oracle parity" unless the same cases were driven with `REWRITER_ORACLE_ADDR` against a live C++ service or the lack of oracle was explicitly recorded.
- Do not remove fuzz seeds or golden cases to get a green run.

## LOCAL VERIFICATION

```bash
go test ./internal/harness
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" go test ./internal/harness -count=1
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1
go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 30s
```
