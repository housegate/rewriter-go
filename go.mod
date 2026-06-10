module github.com/housegate/rewriter-go

go 1.25.0

require (
	github.com/tobilg/polyglot/packages/go v0.5.1
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
)

// polyglot is vendored as a git submodule so the Go bindings and the Rust FFI
// lib are built from the same pinned commit (see .gitmodules).
replace github.com/tobilg/polyglot/packages/go => ./third_party/polyglot-src/packages/go
