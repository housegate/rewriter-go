package handlers

// Client-visible storage-integrity refusals for logical table db1.t, pinned
// literally (not through the nameresolve helpers) so a wording change is a
// deliberate test change. rewriter-grpc pins the same bytes.
const (
	siAppendOnlyMsg = "storage-integrity table db1.t is append-only: only INSERT and SELECT are accepted, " +
		"and UPDATE, DELETE, ALTER, TRUNCATE and other statements on it are refused"
	siAppendOnlyV2Msg = siAppendOnlyMsg + "; to change its schema, use a plain DROP TABLE and CREATE it again"
	siReferenceMsg    = "storage-integrity table db1.t can be read only by SELECT, or by INSERT ... SELECT or " +
		"CREATE TABLE ... AS SELECT into an ordinary table; views, table engines, ALTER, UPDATE, DELETE and " +
		"writes into a storage-integrity table cannot reference it"
)
