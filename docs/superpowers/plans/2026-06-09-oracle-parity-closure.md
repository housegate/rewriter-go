# Oracle Parity Closure + CI Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. A **live C++ oracle** runs at `localhost:50051` (docker container `rwo-oracle`, image `housegate-rewriter:0.11.0` — the exact source that was ported). Verify each fix by re-running the differential against it.

**Goal:** Drive the differential-oracle harness to fully green against the live C++ `rewriter-grpc` oracle (validating all 5 phases' real parity), then wire the oracle differential into CI.

**Architecture:** Two harness-comparison fixes eliminate ~42 false failures (nil≡empty maps; semantic SQL via AST-diff). Four production/corpus fixes close the real divergences the oracle revealed (statement_type-on-reject; GRANT TO ALL; remote() arg quoting; DETACH allow-list). Then a CI job runs the oracle image as a service and sets `REWRITER_ORACLE_ADDR`.

**Verification environment:**
```
export POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib
export REWRITER_ORACLE_ADDR=localhost:50051   # the running oracle container
go test ./internal/harness -count=1            # the differential
```
The oracle container must stay running. If absent, restart it:
`docker run -d --name rwo-oracle --platform linux/amd64 -p 50051:50051 <private-registry>/housegate-rewriter:0.11.0 /clickhousegate_rewriter 50051`

## Baseline parity report (live oracle, captured 2026-06-09)

44 pass / 63 fail. Categories (with the authoritative oracle behavior, probed directly):
- **A — nil≡empty map** (~30, harness): on Success the C++ returns `nil` table_rewrites/database_rewrites; native inits empty `{}` (newWriteResp/newDBResp). `Compare`'s `reflect.DeepEqual(nil, {})==false` → false divergence.
- **B — statement_type on REJECT** (~20, production): probed — on **Success** C++ sets the type (matches native); on **reject** (Unsupported/Invalid) C++ returns `STATEMENT_TYPE_UNSPECIFIED`; native stamps the type via `classify()`+`newXxxResp`.
- **C — SQL formatting** (~12, harness): backtick-vs-`"`, `AS` vs implicit alias, `ENGINE=Memory` vs `ENGINE = Memory`, column backticks. Proven AST-equal via `engine.DiffSQL`; `semanticSQLEq` does polyglot-normalize-then-**string**-compare (too strict). Design §6e prescribed AST-diff.
- **D — GRANT … TO ALL** (1, production bug): C++ **accepts** it (ClickHouse parses `ALL` as a quoted identifier `` `ALL` ``, emits 1 delta grantee name=`ALL`); native over-rejects in `buildGrantees`.
- **E — remote() arg quoting** (~3, production bug): native renders `remote('addr', db, table, 'u', 'p')` with **bare** db/table; C++ renders `remote('addr', 'db', 'table', 'u', 'p')` (string literals). `engine.nodes.go remoteFunc` uses `colBare` for db/table.
- **F — DETACH code** (1, allow-list): `DETACH TABLE db.t` — polyglot parses it (→ native Unsupported); the C++ ClickHouse parser **rejects it as SyntaxError before any handler** (the writes.cc DETACH-reject is dead code). A genuine substrate-parser divergence → §7 allow-list.

## File Structure

- `internal/harness/compare.go` — nil≡empty map normalization (A); the AST-diff comparator is supplied by the golden tests via `engine.DiffSQL` (C).
- `internal/harness/select_golden_test.go` — `semanticSQLEq` → AST-diff (C).
- `native.go` — clear `statement_type` on non-Success (B).
- `internal/handlers/grant.go` — drop the ALL rejection (D).
- `internal/engine/nodes.go` — `remoteFunc` db/table as string literals (E).
- `internal/harness/testdata/{writes,dblevel,phase4}_cases.json` — re-freeze reject `want_stmt` (B), the GRANT-TO-ALL case (D), the remote case (E), the DETACH allow-list (F).
- `internal/harness/*_golden_test.go` — a per-case `allow_code_divergence` carve-out (F).
- `.github/workflows/ci.yml` — the oracle-differential job (CI).

Each task: implement → re-run the differential → confirm the targeted cases now pass (and no regressions) → gofmt/vet/build → commit.

---

## Task 1: Harness — `Compare` treats nil and empty maps as equal (category A)

**Files:** Modify `internal/harness/compare.go`

- [ ] **Step 1: Add a nil-tolerant map comparison.** Replace the three `reflect.DeepEqual(...)` map checks (`table_rewrites`, `database_rewrites`) — NOT `failed_cte_aliases` (that's a slice) — with `mapEq`:

```go
// mapEq compares two string maps treating nil and empty as equal (proto3 emits
// nil for an empty map on the wire, while the native rewriter often inits an
// empty non-nil map — semantically identical).
func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
```

Change the two map comparisons in `Compare`:

```go
	if !mapEq(got.GetTableRewrites(), want.GetTableRewrites()) {
		add("table_rewrites", got.GetTableRewrites(), want.GetTableRewrites())
	}
	if !mapEq(got.GetDatabaseRewrites(), want.GetDatabaseRewrites()) {
		add("database_rewrites", got.GetDatabaseRewrites(), want.GetDatabaseRewrites())
	}
```

(`reflect` may now be unused in compare.go — if so, remove the import. `privilegeDeltasEqual` already does field-wise comparison, so it doesn't use reflect for the messages; check whether `reflect.DeepEqual` is still used for `GetPrivileges()` — if yes, keep the import.)

- [ ] **Step 2: Add a `Compare` unit test** to `internal/harness/compare_test.go`:

```go
func TestCompare_nilVsEmptyMap(t *testing.T) {
	got := &pb.RewriteSQLResponse{TableRewrites: map[string]string{}}      // native: empty non-nil
	want := &pb.RewriteSQLResponse{}                                        // oracle: nil
	if d := Compare(got, want, nil); !d.Equal() {
		t.Errorf("nil vs empty map should be equal: %v", d.Mismatches)
	}
	got2 := &pb.RewriteSQLResponse{TableRewrites: map[string]string{"a": "b"}}
	if d := Compare(got2, want, nil); d.Equal() {
		t.Error("non-empty vs nil should differ")
	}
}
```

- [ ] **Step 3: Verify** — `go test ./internal/harness -run TestCompare` (no FFI) PASS. Then the differential:
  `POLYGLOT_SQL_FFI_PATH=… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1 2>&1 | grep -c "got map\[\], want map\[\]"` → should be **0** (all `got map[] want map[]` divergences gone). The remaining failures are now only B/C/D/E/F.

- [ ] **Step 4: gofmt + vet + commit**

```bash
gofmt -w internal/harness/compare.go internal/harness/compare_test.go
go vet ./... && go build ./...
git add internal/harness/compare.go internal/harness/compare_test.go
git commit -m "fix(harness): Compare treats nil and empty maps as equal (oracle parity)"
```

---

## Task 2: Harness — semantic SQL comparison via AST-diff (category C)

**Files:** Modify `internal/harness/select_golden_test.go` (the `semanticSQLEq` helper, shared by all golden tests)

- [ ] **Step 1: Replace `semanticSQLEq`'s normalize-then-string-compare with `engine.DiffSQL` (AST-diff).** The current impl parses+generates each side and string-compares. Switch to comparing ASTs, which treats backtick-vs-`"`, `AS` vs implicit, spacing, and column-quoting as equal (verified via probe). Keep the signature `func semanticSQLEq(e engine.Engine) SemanticEq`:

```go
// semanticSQLEq reports SQL equality by AST diff: two strings are equal when
// polyglot parses them to the same AST (an empty diff). This is robust to the
// formatting differences between polyglot's generator and ClickHouse's
// formatAst — backtick vs double-quote identifiers, `AS` vs implicit aliases,
// spacing, WhenNecessary column quoting — which are syntactically different but
// semantically identical (design §6e: compare semantically, not byte-wise).
func semanticSQLEq(e engine.Engine) SemanticEq {
	return func(a, b string) (bool, error) {
		if a == b {
			return true, nil
		}
		raw, err := e.DiffSQL(a, b)
		if err != nil {
			return false, err
		}
		return diffIsEmpty(raw), nil
	}
}

// diffIsEmpty reports whether a polyglot Diff result encodes no changes — an
// empty JSON array (modulo whitespace).
func diffIsEmpty(raw engine.AST) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "[]"
}
```

Ensure `select_golden_test.go` imports `strings` and `internal/engine` (it already uses `engine.Engine`). Remove the now-unused `normalizeSQLGolden` if nothing else references it (grep first: `grep -rn normalizeSQLGolden internal/harness`).

- [ ] **Step 2: Verify** — run the differential:
  `POLYGLOT_SQL_FFI_PATH=… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1 -v 2>&1 | grep "sql_after_rewrite(semantic)"` → should drop to **0** (or only genuine AST-different cases like the remote() one, which Task 5 fixes). Confirm the writes/select/dblevel golden tests (without oracle) still pass:
  `POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness -count=1` — the frozen `want_sql` values must still AST-match native output (they will — native output parses to the same AST as itself).

- [ ] **Step 3: gofmt + vet + commit**

```bash
gofmt -w internal/harness/select_golden_test.go
go vet ./... && go build ./...
git add internal/harness/select_golden_test.go
git commit -m "fix(harness): semantic SQL comparison via AST-diff, not string compare (oracle parity)"
```

---

## Task 3: Production — clear `statement_type` on non-Success (category B)

**Files:** Modify `native.go`; re-freeze reject `want_stmt` in `internal/harness/testdata/{writes,dblevel,phase4}_cases.json`

- [ ] **Step 1: Clear `statement_type` on every non-Success handled response.** The C++ sets `statement_type` only in `setSuccessResponse`; on a reject it stays `UNSPECIFIED`. Native stamps it via `classify()` + `newXxxResp`. Add a single normalization. In `native.go`, the cleanest spot is a tiny helper applied wherever a handled `resp` is finalized — fold it into the existing §8 echo blocks. Replace each of the four handled blocks' §8 echo:

```go
		if wresp.GetCode() != pb.RewriteCode_Success && wresp.GetSqlAfterRewrite() == "" {
			wresp.SqlAfterRewrite = sql
		}
```

with a call to a shared finalizer that ALSO clears the type:

```go
		finalizeNonSuccess(wresp, sql)
```

and add the helper:

```go
// finalizeNonSuccess normalizes a handled non-Success response to match the C++
// oracle: it never sets statement_type on a reject (the C++ only sets it in
// setSuccessResponse), and it echoes the original SQL so RewriteResult.SQL stays
// runnable (design §8). No-op on a Success response.
func finalizeNonSuccess(resp *pb.RewriteSQLResponse, sql string) {
	if resp.GetCode() == pb.RewriteCode_Success {
		return
	}
	resp.StatementType = pb.StatementType_STATEMENT_TYPE_UNSPECIFIED
	if resp.GetSqlAfterRewrite() == "" {
		resp.SqlAfterRewrite = sql
	}
}
```

Apply `finalizeNonSuccess(<resp>, sql)` in all four handled branches (write `wresp`, db-level `dresp`, exists `xresp`, grant `gresp`). The SELECT branch (`hresp`) and pass-through never reject (SELECT is lenient; pass-through is Success), and the SyntaxError path already has `StatementType` unset — but for safety, ALSO call `finalizeNonSuccess(resp, sql)` on the SyntaxError early-return (it's already UNSPECIFIED, so this only future-proofs). The pass-through tail is always Success — leave it.

- [ ] **Step 2: Re-freeze reject `want_stmt`.** Every corpus case with `"reject": true` (or a non-Success `want_code`) must now expect `STATEMENT_TYPE_UNSPECIFIED`. Set `"want_stmt": ""` (the golden driver skips the assertion when empty; the oracle Compare validates UNSPECIFIED==UNSPECIFIED directly) — OR add `"UNSPECIFIED"` to each `*StmtByName` map and set `"want_stmt": "UNSPECIFIED"`. **Use `"want_stmt": ""`** (simplest). Edit:
  - `writes_cases.json`: every `"reject": true` case (create_table_as_table_function_reject, drop_table_multi_reject, alter_cross_table_reject, detach_reject, the bare-rejects, COPY, etc.) → set `"want_stmt": ""`.
  - `dblevel_cases.json`: every reject case (use_unresolvable_invalid, use_remote_mapped_unsupported, show_tables_no_context_invalid, create_database_no_dynamic_unsupported, create_database_already_exists_invalid, drop_database_not_managed_invalid) → `"want_stmt": ""`.
  - `phase4_cases.json`: every reject case (exists_database_rejected, show_create_view_rejected, grant_global_scope_rejected, grant_column_rejected, grant_role_membership_rejected, grant_unresolvable_db_invalid, exists_dictionary_rejected, show_create_dictionary_rejected, exists_remote_rejected, grant_attach_rejected, grant_with_replace_rejected, grant_current_grants_rejected, grant_unqualified_no_upstream_invalid) → `"want_stmt": ""`. (grant_to_all is handled in Task 4.)

  > To find them mechanically: every case object containing `"reject": true`. Leave Success cases' `want_stmt` untouched.

- [ ] **Step 3: Verify** — the non-oracle golden tests still pass (`POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness -count=1`), and the oracle differential's `statement_type:` divergences drop to **0**:
  `… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1 -v 2>&1 | grep -c "statement_type:"` → **0**.
  Also confirm native unit/contract tests still pass: `POLYGLOT_SQL_FFI_PATH=… go test ./... -count=1` (the native package's reject tests may assert statement_type — update any that now expect UNSPECIFIED on a reject; grep `native_test.go` for reject-case statement_type assertions).

- [ ] **Step 4: gofmt + vet + commit**

```bash
gofmt -w native.go
go vet ./... && go build ./...
git add native.go internal/harness/testdata/writes_cases.json internal/harness/testdata/dblevel_cases.json internal/harness/testdata/phase4_cases.json native_test.go
git commit -m "fix(native): statement_type UNSPECIFIED on non-Success, matching C++ oracle"
```

---

## Task 4: Production — accept `GRANT … TO ALL` (category D)

**Files:** Modify `internal/handlers/grant.go`; re-freeze `phase4_cases.json` grant_to_all case

- [ ] **Step 1: Drop the ALL rejection.** In `buildGrantees`, ClickHouse parses `TO ALL` / `FROM ALL` as a quoted identifier named `ALL` (the oracle emits a delta with grantee name=`ALL`), NOT the `set.all` keyword polyglot can't distinguish. Remove the `strings.EqualFold(name, "ALL")` → reject case, leaving the default `Grantee{Name: name}` (so `ALL` becomes a normal grantee name):

```go
func buildGrantees(resp *pb.RewriteSQLResponse, kw string, principals []string) ([]*pb.PrivilegeDelta_Grantee, bool) {
	var out []*pb.PrivilegeDelta_Grantee
	for _, name := range principals {
		if strings.EqualFold(name, "CURRENT_USER") {
			out = append(out, &pb.PrivilegeDelta_Grantee{IsCurrentUser: true})
			continue
		}
		out = append(out, &pb.PrivilegeDelta_Grantee{Name: name})
	}
	if len(out) == 0 {
		rejectInvalid(resp, kw+" has no grantees")
		return nil, false
	}
	return out, true
}
```

(The `dir`/`GRANT TO `/`REVOKE FROM ` strings are now unused — remove them. The empty-grantees Invalid reject stays.)

- [ ] **Step 2: Re-freeze the corpus case.** In `phase4_cases.json`, change `grant_to_all_rejected` from a reject to a success. Probe the exact oracle output first:
  `… REWRITER_ORACLE_ADDR=localhost:50051` then run the differential `-run TestPhase4Golden/grant_to_all` to see native's new output. Native should now emit Success + 1 delta. Update the case to (rename to `grant_to_all`):

```json
  {
    "name": "grant_to_all",
    "sql": "GRANT SELECT ON logical1.t TO ALL",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT ON logical1.t TO ALL' AS gstmt",
    "want_privileges_deltas": [{"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"name": "ALL"}], "grant_option": false}]
  }
```

  **IMPORTANT:** verify `want_sql` and the grantee name against BOTH native output AND the oracle (the oracle's marker was `SELECT 'GRANT SELECT ON logical1.t TO \`ALL\`' AS gstmt` — backtick-quoted ALL — so native's marker may differ from the oracle here; if the oracle backtick-quotes `ALL` in the marker and native doesn't, that `sql_after_rewrite` is a synthetic `SELECT '...'` compared EXACTLY → a real divergence. If so, either (a) accept native's form and rely on the AST-diff for the marker — but synthetic markers are exact-compared — or (b) treat the marker-quoting as the remaining divergence and allow-list it. Resolve by checking what the oracle differential reports for this case after the code fix, and freeze/allow-list accordingly. Document the outcome in the commit message.)

- [ ] **Step 3: Verify** — `… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1 -run TestPhase4Golden/grant_to_all -v` PASS (or, if the marker-quoting diverges, allow-list it per Step 2 and confirm). Existing grant handler unit tests: update `internal/handlers/grant_test.go`'s `to all` reject case → now a success (or remove it). `POLYGLOT_SQL_FFI_PATH=… go test ./internal/handlers -count=1` PASS.

- [ ] **Step 4: gofmt + vet + commit**

```bash
gofmt -w internal/handlers/grant.go internal/handlers/grant_test.go
go vet ./... && go build ./...
git add internal/handlers/grant.go internal/handlers/grant_test.go internal/harness/testdata/phase4_cases.json
git commit -m "fix(handlers): accept GRANT/REVOKE TO ALL as a named grantee, matching C++ oracle"
```

---

## Task 5: Production — `remote()` db/table as string literals (category E)

**Files:** Modify `internal/engine/nodes.go`

- [ ] **Step 1: Render db/table as string literals.** In `remoteFunc`, the C++ emits `remote('addr', 'db', 'table', 'user', 'password')` — all args are string literals. Native uses `colBare(r.DB), colBare(r.Table)` (bare identifiers). Change them to `litStr`:

```go
func remoteFunc(r *RemoteSpec) map[string]any {
	return map[string]any{
		"name": "remote",
		"args": []any{
			litStr(r.Addr), litStr(r.DB), litStr(r.Table), litStr(r.User), litStr(r.Password),
		},
		"distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
	}
}
```

Update the `remoteFunc` doc comment ("db/table are bare column identifiers" → "all five args are string literals, matching ClickHouse's canonical remote() form"). If `colBare` is now unused, remove it (grep: `grep -rn colBare internal/engine`).

- [ ] **Step 2: Verify** — re-run the differential for the remote cases:
  `… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1 -v 2>&1 | grep -iE "remote|static_remote"` → no divergence. The select/writes golden `want_sql` for remote cases (frozen native output) will change — re-run the non-oracle golden (`POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness -count=1`) and update any `want_sql`/`sql_contains` that referenced the bare `remote(..., db, table, ...)` form to the quoted form (`remote(..., 'db', 'table', ...)`). Engine round-trip tests in `internal/engine` that assert the remote() rendering also need updating (grep `internal/engine` tests for `remote(`).

- [ ] **Step 3: gofmt + vet + commit**

```bash
gofmt -w internal/engine/nodes.go
go vet ./... && go build ./...
git add internal/engine/nodes.go internal/engine/*_test.go internal/harness/testdata/*.json
git commit -m "fix(engine): remote() db/table args as string literals, matching C++ oracle"
```

---

## Task 6: DETACH allow-list (category F) + final differential sweep

**Files:** Modify the golden-test drivers + `writes_cases.json`

- [ ] **Step 1: Add a per-case allow-list flag.** `DETACH TABLE db.t` is parsed by polyglot (→ native Unsupported) but rejected by ClickHouse's own parser as SyntaxError (the writes.cc DETACH reject is dead code). This is a substrate-parser divergence (design §7 allow-list). Add a `AllowCodeDivergence bool` json field to the writes case struct and, in the oracle-differential block, skip the `code` field when set. In `writes_golden_test.go`:

```go
	// add to writeCase:
	AllowCodeDivergence bool `json:"allow_code_divergence"`
```

In the oracle block, when `c.AllowCodeDivergence`, normalize the oracle `code` onto `got` before Compare (so only `code` is exempted, everything else still gated):

```go
		if c.AllowCodeDivergence {
			got.Code = want.GetCode()
		}
```

(Place it next to the existing `c.Reject` carve-out. Document with a comment: polyglot parses some admin statements ClickHouse's own parser rejects — DETACH being one — so native returns Unsupported where C++ returns SyntaxError; both reject, the code differs by substrate.)

- [ ] **Step 2: Mark the case.** In `writes_cases.json`, `detach_reject`: add `"allow_code_divergence": true`. (Its `want_code` stays `UnsupportedStatement` for the native golden assertion; the oracle differential exempts the code.)

- [ ] **Step 3: FULL differential sweep — the green gate.** With the oracle running:
  `POLYGLOT_SQL_FFI_PATH=… REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1` → **PASS (0 failures)**. If ANY case still diverges, triage it: real bug → fix; intentional substrate divergence → allow-list with a documented reason (and report it in the commit). Capture the final pass count.
  Also run the WHOLE suite both ways: `POLYGLOT_SQL_FFI_PATH=… go test ./... -count=1` (with + without `REWRITER_ORACLE_ADDR`) → all green.

- [ ] **Step 4: gofmt + vet + commit**

```bash
gofmt -w internal/harness/writes_golden_test.go
go vet ./... && go build ./...
git add internal/harness/writes_golden_test.go internal/harness/testdata/writes_cases.json
git commit -m "test(harness): allow-list DETACH code divergence (polyglot parses, CH SyntaxErrors); differential fully green"
```

---

## Task 7: Wire the CI oracle-differential job

**Files:** Modify `.github/workflows/ci.yml`; document the GCP auth requirement

- [ ] **Step 1: Add the `oracle` job** to `.github/workflows/ci.yml`. It needs (a) the FFI lib (build via `make ffi`, reuse the cache from the `ffi` job's key) and (b) the C++ oracle running as a service. The image lives in a **private GCP Artifact Registry** (`<private-registry>/housegate-rewriter`), so the job must authenticate to GCP. Use `google-github-actions/auth` with a repo secret. Append:

```yaml
  # Oracle-differential lane: build the FFI lib + run the C++ rewriter-grpc oracle
  # as a service, then run the harness with REWRITER_ORACLE_ADDR so every golden
  # corpus is diffed field-by-field against the real C++ behavior (the TRUE parity
  # gate). Requires the GCP_ORACLE_KEY secret (a service-account key with read
  # access to the housegate-rewriter Artifact Registry repo) — when absent (e.g.
  # on forks), this job is skipped via the `if` guard so the rest of CI still runs.
  oracle:
    runs-on: ubuntu-latest
    if: ${{ github.repository == 'housegate/rewriter-go' }}
    env:
      ORACLE_IMAGE: <private-registry>/housegate-rewriter
      ORACLE_TAG: "0.11.0"
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - uses: dtolnay/rust-toolchain@stable
      - uses: actions/cache@v4
        with:
          path: |
            ~/.cargo/registry
            ~/.cargo/git
            third_party/polyglot-src/target
          key: polyglot-ffi-${{ runner.os }}-${{ hashFiles('Makefile') }}
      - run: make ffi
      - id: auth
        uses: google-github-actions/auth@v2
        with:
          credentials_json: ${{ secrets.GCP_ORACLE_KEY }}
      - uses: docker/login-action@v3
        with:
          registry: us-west1-docker.pkg.dev
          username: _json_key
          password: ${{ secrets.GCP_ORACLE_KEY }}
      - name: Start C++ oracle
        run: |
          docker run -d --name oracle -p 50051:50051 \
            "$ORACLE_IMAGE:$ORACLE_TAG" /clickhousegate_rewriter 50051
          # wait for the gRPC port to accept connections
          for i in $(seq 1 30); do
            if bash -c "</dev/tcp/127.0.0.1/50051" 2>/dev/null; then echo "oracle up"; break; fi
            sleep 1
          done
      - name: Differential harness
        env:
          POLYGLOT_SQL_FFI_PATH: ${{ github.workspace }}/third_party/lib/libpolyglot_sql_ffi.so
          REWRITER_ORACLE_ADDR: localhost:50051
        run: go test ./internal/harness -count=1 -v
      - name: Oracle logs on failure
        if: failure()
        run: docker logs oracle || true
```

  Notes for the implementer:
  - The CI runner is `linux/amd64`, so the image runs natively (no `--platform`).
  - Pin `ORACLE_TAG` to the version whose C++ source matches the port (`0.11.0` today). Add a comment that it should be bumped in lockstep when porting newer C++ behavior.
  - The `if: github.repository == ...` guard keeps the job from failing on forks/PRs without the secret.

- [ ] **Step 2: Document the secret in `README.md`.** Add a short "CI oracle differential" subsection under the existing CI/build notes: the `oracle` job runs the C++ `rewriter-grpc` image as the parity oracle and requires a repo secret `GCP_ORACLE_KEY` (a GCP service-account JSON key with `roles/artifactregistry.reader` on the registry repo); without it the job is skipped. Note that `ORACLE_TAG` must track the ported C++ version.

- [ ] **Step 3: Validate the workflow YAML** locally — `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))"` (or `yamllint` / `actionlint` if available) → no errors. The job itself can't be fully run in-session (no CI runner), but the harness command it runs is exactly what we verified against the live oracle locally.

- [ ] **Step 4: commit**

```bash
git add .github/workflows/ci.yml README.md
git commit -m "ci: add oracle-differential job (runs C++ rewriter-grpc image, gates true parity)"
```

---

## Self-Review

**Coverage of the parity report:** A→T1, C→T2 (the two harness fixes), B→T3, D→T4, E→T5, F→T6 (the four real-divergence closures), CI→T7. Each production fix re-freezes the affected corpus cases and is verified against the live oracle. The final sweep (T6 Step 3) is the green gate.

**Verification is empirical, not frozen:** every task re-runs `go test ./internal/harness` with `REWRITER_ORACLE_ADDR` set, so "green" means the native output matches the *live C++ oracle*, field-by-field — the true §7 gate, finally exercised.

**Risks / watch-items:**
- T4 marker quoting: the oracle backtick-quotes `ALL` in the GRANT marker (`TO \`ALL\``); native may not. Synthetic `SELECT '...'` markers are exact-compared, so if they differ that's a residual divergence to either fix (match the quoting) or allow-list — resolved empirically in T4 Step 2.
- T3 want_stmt re-freeze: must touch ONLY `reject:true` cases; a Success case wrongly cleared would mask a real type bug.
- T5: changing remote() rendering shifts frozen `want_sql` in select/writes corpora and any engine round-trip test — re-freeze all of them.
- CI (T7) can't be executed in-session; the YAML is validated and the harness command is the one proven locally. The job depends on a secret the maintainer must provision.
