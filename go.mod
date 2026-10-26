module github.com/housegate/rewriter-go

go 1.26.10

require (
	github.com/ebitengine/purego v0.10.0
	github.com/housegate/rewriter-proto v0.4.0
	github.com/tobilg/polyglot/packages/go v0.13.3
	golang.org/x/sys v0.48.0
	google.golang.org/grpc v1.83.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)

// polyglot is vendored as a git submodule so the Go bindings and the Rust FFI
// lib are built from the same pinned commit (see .gitmodules).
replace github.com/tobilg/polyglot/packages/go => ./third_party/polyglot-src/packages/go
