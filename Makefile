SHELL := /bin/bash
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  FFI_EXT := dylib
else
  FFI_EXT := so
endif
FFI_LIB := third_party/lib/libpolyglot_sql_ffi.$(FFI_EXT)
export POLYGLOT_SQL_FFI_PATH := $(abspath $(FFI_LIB))

.PHONY: ffi test test-ordinary tidy snapshot-measured-compile snapshot-measured-generate snapshot-measured-run
ffi: $(FFI_LIB)
$(FFI_LIB):
	git submodule update --init third_party/polyglot-src
	cd third_party/polyglot-src && cargo build -p polyglot-sql-ffi --profile ffi_release
	mkdir -p third_party/lib
	cp third_party/polyglot-src/target/ffi_release/libpolyglot_sql_ffi.$(FFI_EXT) $(FFI_LIB)

test-ordinary: ffi
	@echo "Ordinary unit/FFI regressions only; no snapshot qualification"
	SNAPSHOT_QUERY_ORDINARY=1 go test ./...

SNAPSHOT_OUT ?=
SNAPSHOT_TOOL ?=
SNAPSHOT_RECIPE ?=
SNAPSHOT_CORPUS := $(abspath internal/harness/testdata/snapshot_query_cases.json)
snapshot-measured-compile: ffi
	@test -n "$(SNAPSHOT_OUT)" || (echo "SNAPSHOT_OUT is required" >&2; exit 1)
	bash scripts/test-snapshot-measured.sh compile "$(abspath $(SNAPSHOT_OUT))"
snapshot-measured-generate:
	@test -n "$(SNAPSHOT_OUT)" -a -n "$(SNAPSHOT_TOOL)" -a -n "$(SNAPSHOT_RECIPE)" || (echo "Actual external generator and recipe inputs are required" >&2; exit 1)
	bash scripts/test-snapshot-measured.sh generate "$(abspath $(SNAPSHOT_OUT))" "$(SNAPSHOT_TOOL)" "$(SNAPSHOT_RECIPE)" "$(SNAPSHOT_CORPUS)"
snapshot-measured-run:
	@test -n "$(SNAPSHOT_OUT)" || (echo "SNAPSHOT_OUT is required" >&2; exit 1)
	bash scripts/test-snapshot-measured.sh run "$(abspath $(SNAPSHOT_OUT))" "$(POLYGLOT_SQL_FFI_PATH)" "$(abspath $(SNAPSHOT_OUT))/profiles.json" "$(abspath $(SNAPSHOT_OUT))/resolved-corpus.json"
ifeq ($(UNAME_S),Linux)
test: test-ordinary
	$(MAKE) snapshot-measured-compile
	$(MAKE) snapshot-measured-generate
	$(MAKE) snapshot-measured-run
else
test: test-ordinary
	@echo "Non-Linux ordinary lane; measured snapshot qualification requires Linux"
endif

tidy:
	go mod tidy
