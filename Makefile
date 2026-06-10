SHELL := /bin/bash
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
	git submodule update --init third_party/polyglot-src
	cd third_party/polyglot-src && cargo build -p polyglot-sql-ffi --profile ffi_release
	mkdir -p third_party/lib
	cp third_party/polyglot-src/target/ffi_release/libpolyglot_sql_ffi.$(FFI_EXT) $(FFI_LIB)

proto:
	buf generate

test: ffi
	go test ./...

tidy:
	go mod tidy
