# rewriter-go

Native **Go** implementation of the ClickHouse SQL `Rewriter` interface — an in-process
(no gRPC hop) alternative to the C++ [`rewriter-grpc`](../rewriter-grpc) service, built on
[tobilg/polyglot](https://github.com/tobilg/polyglot) (Rust SQL transpiler, ClickHouse
dialect) via its PureGo SDK (`CGO_ENABLED=0`).

Target: **full behavioral parity** with `rewriter-grpc` across `Rewrite` +
`RewriteErrorMessage`, validated by a differential oracle harness that runs both engines and
diffs their responses.

## Design

See [`docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md`](docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md)
for the full design: architecture, the polyglot engine seam, the parity risk register, the
differential-harness validation strategy, and the phased build plan.

> Status: design approved; implementation phased (Phase 0 = engine + harness + fidelity
> spike). Implementation work lands on `feat/*` branches via PR.
