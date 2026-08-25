package engine

import (
	"fmt"
	"strings"
	"unicode"
)

const mutationProbeTable = "__hg_si_probe"

// MutationReadKind distinguishes ordinary table reads from indirect namespace
// surfaces without losing their common source order.
type MutationReadKind uint8

const (
	MutationReadTable MutationReadKind = iota + 1
	MutationReadNamespace
)

// MutationRead is one source-ordered read event in a mutation expression.
type MutationRead struct {
	Kind      MutationReadKind
	Table     TableTarget
	Namespace NamespaceRef
}

// MutationReadSet is one grammar-ordered mutation expression subtree. Ordered
// is the policy authority: the first SI object wins even when an indirect
// namespace precedes an ordinary table. Tables/Namespaces remain convenient
// typed projections for collector callers and tests.
type MutationReadSet struct {
	Ordered    []MutationRead
	Tables     []TableTarget
	Namespaces []NamespaceRef
}

// MutationReadSurface preserves the statement grammar boundary that controls
// first-reject precedence: UPDATE assignments are inspected before its
// predicate; DELETE has only a predicate surface.
type MutationReadSurface struct {
	Assignments MutationReadSet
	Predicate   MutationReadSet
}

// CollectMutationReadSurface extracts read sources embedded in mutation
// expressions without treating the mutation target or assignment LHS as a
// read. Structured UPDATE/DELETE nodes are read directly. ClickHouse ALTER
// mutations are opaque in Polyglot, so their action tail is adapted through a
// sentinel-target UPDATE/DELETE probe and accepted only when the probe target,
// assignment/predicate shape, and full token stream are all proven exact.
//
// An error means the mutation surface could not be proven. With active storage
// integrity the handler must turn that into a generic UnsupportedStatement
// response (nil Go error), never fall open.
func CollectMutationReadSurface(e Engine, ast AST, sql string) (MutationReadSurface, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return MutationReadSurface{}, err
	}
	if body == nil {
		return MutationReadSurface{}, nil
	}

	switch kind {
	case NodeUpdate:
		if strings.TrimSpace(sql) == "" {
			return MutationReadSurface{}, fmt.Errorf("engine: UPDATE mutation source SQL is required")
		}
		if err := mutationRoundTripsExactly(e, sql, ast); err != nil {
			return MutationReadSurface{}, err
		}
		return collectStructuredMutationSurface(NodeUpdate, body)
	case NodeDelete:
		if strings.TrimSpace(sql) == "" {
			return MutationReadSurface{}, fmt.Errorf("engine: DELETE mutation source SQL is required")
		}
		if err := mutationRoundTripsExactly(e, sql, ast); err != nil {
			return MutationReadSurface{}, err
		}
		return collectStructuredMutationSurface(NodeDelete, body)
	case NodeCommand:
		raw, _ := body["this"].(string)
		if classifyWriteCommand(raw) != CmdAlterUpdate {
			return MutationReadSurface{}, nil
		}
		if strings.TrimSpace(sql) == "" {
			sql = raw
		}
		surface, applied, err := collectAlterMutationSurface(e, sql)
		if err != nil {
			return MutationReadSurface{}, err
		}
		if !applied {
			return MutationReadSurface{}, fmt.Errorf("engine: ALTER UPDATE mutation probe did not match source grammar")
		}
		return surface, nil
	case NodeAlterTable:
		hinted := alterBodyHasMutation(body)
		if !hinted {
			return MutationReadSurface{}, nil
		}
		if strings.TrimSpace(sql) == "" {
			return MutationReadSurface{}, fmt.Errorf("engine: ALTER mutation source SQL is required")
		}
		surface, applied, err := collectAlterMutationSurface(e, sql)
		if err != nil {
			return MutationReadSurface{}, err
		}
		if !applied {
			return MutationReadSurface{}, fmt.Errorf("engine: ALTER mutation probe did not match source grammar")
		}
		return surface, nil
	default:
		return MutationReadSurface{}, nil
	}
}

func collectStructuredMutationSurface(kind string, body map[string]any) (MutationReadSurface, error) {
	var surface MutationReadSurface
	if kind == NodeUpdate {
		assignments, ok := body["set"].([]any)
		if !ok || len(assignments) == 0 {
			return MutationReadSurface{}, fmt.Errorf("engine: UPDATE mutation has no proven assignments")
		}
		for i, rawAssignment := range assignments {
			assignment, ok := rawAssignment.([]any)
			if !ok || len(assignment) != 2 || concreteIdentifierName(assignment[0]) == "" {
				return MutationReadSurface{}, fmt.Errorf("engine: UPDATE assignment %d has an unproven shape", i)
			}
			reads, err := collectMutationExpression(assignment[1])
			if err != nil {
				return MutationReadSurface{}, fmt.Errorf("engine: inspect UPDATE assignment %d: %w", i, err)
			}
			appendMutationReads(&surface.Assignments, reads)
		}
	}

	if predicate := body["where_clause"]; predicate != nil {
		reads, err := collectMutationExpression(predicate)
		if err != nil {
			return MutationReadSurface{}, fmt.Errorf("engine: inspect mutation predicate: %w", err)
		}
		surface.Predicate = reads
	}
	return surface, nil
}

func collectMutationExpression(node any) (MutationReadSet, error) {
	var reads MutationReadSet
	err := walkExpression(node, readSourceScope{}, readSourceVisitor{
		table: func(_, _ map[string]any, target TableTarget) {
			reads.Ordered = append(reads.Ordered, MutationRead{Kind: MutationReadTable, Table: target})
			reads.Tables = append(reads.Tables, target)
		},
		namespace: func(_ map[string]any, detail namespaceRefDetail) {
			ref := detail.refWithOrigins()
			reads.Ordered = append(reads.Ordered, MutationRead{Kind: MutationReadNamespace, Namespace: ref})
			reads.Namespaces = append(reads.Namespaces, ref)
		},
	})
	return reads, err
}

func appendMutationReads(dst *MutationReadSet, src MutationReadSet) {
	dst.Ordered = append(dst.Ordered, src.Ordered...)
	dst.Tables = append(dst.Tables, src.Tables...)
	dst.Namespaces = append(dst.Namespaces, src.Namespaces...)
}

type alterMutationKind uint8

const (
	alterMutationNone alterMutationKind = iota
	alterMutationUpdate
	alterMutationDelete
)

func collectAlterMutationSurface(e Engine, sql string) (MutationReadSurface, bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return MutationReadSurface{}, false, fmt.Errorf("engine: tokenize ALTER mutation: %w", err)
	}
	kind, tailStart, ok := alterMutationTail(toks)
	if !ok {
		return MutationReadSurface{}, false, nil
	}
	tail := strings.TrimSpace(sql[tailStart:])
	if tail == "" {
		return MutationReadSurface{}, true, fmt.Errorf("engine: ALTER mutation action tail is empty")
	}

	probe := "UPDATE " + mutationProbeTable + " SET " + tail
	wantKind := NodeUpdate
	if kind == alterMutationDelete {
		probe = "DELETE FROM " + mutationProbeTable + " " + tail
		wantKind = NodeDelete
	}
	probeAST, err := e.ParseOne(probe)
	if err != nil {
		return MutationReadSurface{}, true, fmt.Errorf("engine: parse ALTER mutation probe: %w", err)
	}
	probeKind, body, _, err := bodyOf(probeAST)
	if err != nil {
		return MutationReadSurface{}, true, err
	}
	if probeKind != wantKind || body == nil {
		return MutationReadSurface{}, true, fmt.Errorf("engine: ALTER mutation probe kind = %q, want %q", probeKind, wantKind)
	}
	target, _ := body["table"].(map[string]any)
	decoded := decodeTableTarget(target)
	if decoded.DB != "" || decoded.Table != mutationProbeTable || decoded.Alias != "" {
		return MutationReadSurface{}, true, fmt.Errorf("engine: ALTER mutation probe target is not the exact sentinel")
	}
	if kind == alterMutationDelete && body["where_clause"] == nil {
		return MutationReadSurface{}, true, fmt.Errorf("engine: ALTER DELETE probe lost its predicate")
	}
	if err := mutationRoundTripsExactly(e, probe, probeAST); err != nil {
		return MutationReadSurface{}, true, err
	}
	surface, err := collectStructuredMutationSurface(wantKind, body)
	if err != nil {
		return MutationReadSurface{}, true, err
	}
	return surface, true, nil
}

// alterMutationTail recognizes only the exact ALTER TABLE prefix up to the
// action keyword. The action body itself is delegated to the sentinel probe.
// Optional IF EXISTS and ON CLUSTER modifiers are consumed in their grammar
// positions; an identifier-looking token elsewhere never becomes an action.
func alterMutationTail(toks []rawToken) (alterMutationKind, int, bool) {
	if len(toks) < 4 || !tokenTextIs(toks[0], "ALTER") || !tokenTextIs(toks[1], "TABLE") {
		return alterMutationNone, 0, false
	}
	i := 2
	if i+1 < len(toks) && tokenTextIs(toks[i], "IF") && tokenTextIs(toks[i+1], "EXISTS") {
		i += 2
	}
	var ok bool
	i, ok = consumeMutationQualifiedName(toks, i)
	if !ok {
		return alterMutationNone, 0, false
	}
	if i+2 < len(toks) && tokenTextIs(toks[i], "ON") && tokenTextIs(toks[i+1], "CLUSTER") && mutationClusterToken(toks[i+2]) {
		i += 3
	}
	if i >= len(toks) {
		return alterMutationNone, 0, false
	}
	switch {
	case tokenTextIs(toks[i], "UPDATE"):
		return alterMutationUpdate, toks[i].Span.End, true
	case tokenTextIs(toks[i], "DELETE"):
		return alterMutationDelete, toks[i].Span.End, true
	default:
		return alterMutationNone, 0, false
	}
}

func consumeMutationQualifiedName(toks []rawToken, i int) (int, bool) {
	if i >= len(toks) || !isNameTok(toks[i].TokenType) {
		return i, false
	}
	i++
	if i+1 < len(toks) && toks[i].TokenType == "DOT" && isNameTok(toks[i+1].TokenType) {
		i += 2
	}
	// Catalog-qualified and otherwise overlong name runs are not part of the
	// pinned ALTER adapter. Refuse them instead of guessing which suffix is the
	// table target.
	if i < len(toks) && toks[i].TokenType == "DOT" {
		return i, false
	}
	return i, true
}

func mutationClusterToken(tok rawToken) bool {
	return isNameTok(tok.TokenType) || tok.TokenType == "STRING"
}

func tokenTextIs(tok rawToken, want string) bool { return strings.EqualFold(tok.Text, want) }

func mutationRoundTripsExactly(e Engine, probe string, ast AST) error {
	generated, err := e.Generate(ast)
	if err != nil {
		return fmt.Errorf("engine: generate mutation completeness probe: %w", err)
	}
	want, err := tokenizeRaw(e, probe)
	if err != nil {
		return fmt.Errorf("engine: retokenize mutation completeness input: %w", err)
	}
	got, err := tokenizeRaw(e, generated)
	if err != nil {
		return fmt.Errorf("engine: tokenize generated mutation completeness probe: %w", err)
	}
	trimmedEnd := len(strings.TrimRightFunc(probe, unicode.IsSpace))
	if len(want) == 0 || want[len(want)-1].Span.End != trimmedEnd {
		return fmt.Errorf("engine: mutation completeness tokenizer did not consume the complete input")
	}
	if len(want) != len(got) {
		return fmt.Errorf("engine: mutation completeness probe lost tokens: input=%d generated=%d", len(want), len(got))
	}
	for i := range want {
		if !mutationProbeTokensEqual(want[i], got[i]) {
			return fmt.Errorf("engine: mutation completeness token %d changed from %s:%q to %s:%q",
				i, want[i].TokenType, want[i].Text, got[i].TokenType, got[i].Text)
		}
	}
	return nil
}

// mutationProbeTokensEqual permits only keyword case normalization. Identifier,
// literal, number, and punctuation text must remain byte-for-byte equal: a
// generator that silently changes a table/column name or literal has not proven
// that the sentinel adaptation represents the original mutation.
func mutationProbeTokensEqual(want, got rawToken) bool {
	if want.TokenType != got.TokenType {
		return false
	}
	if want.Text == got.Text {
		return true
	}
	return mutationProbeKeywordToken(want) && mutationProbeKeywordToken(got) &&
		strings.EqualFold(want.Text, got.Text)
}

func mutationProbeKeywordToken(tok rawToken) bool {
	if tok.TokenType == "VAR" || tok.TokenType == "QUOTED_IDENTIFIER" {
		return false
	}
	for _, r := range tok.TokenType {
		if r != '_' && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return tok.TokenType != "" && strings.EqualFold(tok.TokenType, tok.Text)
}

func alterBodyHasMutation(body map[string]any) bool {
	actions, _ := body["actions"].([]any)
	for _, rawAction := range actions {
		action, _ := rawAction.(map[string]any)
		for tag, payload := range action {
			if strings.EqualFold(tag, "UPDATE") || strings.EqualFold(tag, "DELETE") {
				return true
			}
			if !strings.EqualFold(tag, "RAW") {
				continue
			}
			raw, _ := payload.(map[string]any)
			sql, _ := raw["sql"].(string)
			first, _, _ := strings.Cut(strings.TrimSpace(sql), " ")
			if strings.EqualFold(first, "UPDATE") || strings.EqualFold(first, "DELETE") {
				return true
			}
		}
	}
	return false
}
