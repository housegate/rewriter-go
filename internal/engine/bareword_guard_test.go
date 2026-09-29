package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// noParensKeywords are the bare words the pinned Polyglot parses as a
// no-parens, argument-less function call. ClickHouse 26.2 reads every one of
// them as an ordinary identifier (it has no niladic-keyword syntax), so the IN
// binding rule (withValueIsNotTableReference, decodeInOperand) must treat them
// as names. Measured: review round 2, N1 / N2.
var noParensKeywords = map[string]bool{
	"CURDATE": true, "CURRENT_CATALOG": true, "CURRENT_DATE": true, "CURRENT_DATETIME": true,
	"CURRENT_ROLE": true, "CURRENT_SCHEMA": true, "CURRENT_TIME": true, "CURRENT_USER": true,
	"GETDATE": true, "LOCALTIME": true, "LOCALTIMESTAMP": true, "NOW": true, "PI": true,
	"SESSION_USER": true, "SYSDATE": true, "SYSDATETIME": true, "SYSTEM_USER": true,
	"SYSTIMESTAMP": true, "UTC_DATE": true, "UTC_TIME": true, "UTC_TIMESTAMP": true,
}

// bareWordLiterals are the only bare words whose value may count as "not a
// table reference": ClickHouse reads them as literals.
var bareWordLiterals = map[string]bool{"TRUE": true, "FALSE": true, "NULL": true}

// bareWordSweep is the set of bare words swept when the rule was written:
// the keywords above, SQL niladic keywords and functions of other dialects,
// ClickHouse spellings, literals and reserved words.
var bareWordSweep = strings.Fields(`
	CURDATE CURRENT_CATALOG CURRENT_DATE CURRENT_DATETIME CURRENT_ROLE CURRENT_SCHEMA
	CURRENT_TIME CURRENT_USER GETDATE LOCALTIME LOCALTIMESTAMP NOW PI SESSION_USER
	SYSDATE SYSTEM_USER SYSTIMESTAMP UTC_DATE UTC_TIME UTC_TIMESTAMP CURRENT_TIMESTAMP
	USER CURRENT_PATH CURRENT_TRANSFORM_GROUP_FOR_TYPE CURRENT_DEFAULT_TRANSFORM_GROUP
	CURRENT_INSTANCE CURRENT_REGION CURRENT_ACCOUNT CURRENT_WAREHOUSE CURRENT_DATABASE
	CURRENT_CLIENT CURRENT_VERSION CURRENT_SESSION CURRENT_STATEMENT CURRENT_TRANSACTION
	CURRENT_TIMEZONE CURRENT_TIMESTAMP_LTZ CURRENT_IP_ADDRESS CURRENT_AVAILABLE_ROLES
	CURRENT_SECONDARY_ROLES CURRENT_ORGANIZATION_NAME CURRENT_ACCOUNT_NAME LOCALDATE
	SYSDATETIME SYSUTCDATETIME GETUTCDATE NOW64 TODAY YESTERDAY RAND RANDOM UUID NEWID E
	TRUE FALSE NULL UNKNOWN DEFAULT CURRENT MAXVALUE MINVALUE INFINITY NAN INF ROWNUM
	ROWID LEVEL SYSTIMESTAMP_TZ DBTIMEZONE SESSIONTIMEZONE UID CURRENT_SERVER
	CURRENT_SQLID CURRENT_MEMBER CURRENT_OBJECT ANY ALL SOME DISTINCT EXCEPT INTERVAL
	VALUES VERSION DATABASE SCHEMA TIMESTAMP DATE TIME DATETIME ARRAY MAP TUPLE JSON
	OBJECT ROW CAST EXTRACT TRIM POSITION SUBSTRING OVERLAY COALESCE NULLIF GREATEST
	LEAST IF CASE EXISTS NOT CURRENT_ROW
`)

// TestBareWordShapesPinTheInBindingRule guards the Polyglot facts the IN
// binding rule depends on. A Polyglot bump that changes either fails here,
// naming the word:
//
//  1. exactly the noParensKeywords parse as a no-parens, argument-less
//     function node (so noParensKeywordName and the no_parens checks see
//     them), in upper and lower case and inside parentheses;
//  2. no bare word except a literal parses into a node that
//     withValueIsNotTableReference accepts as "not a table" — e.g. a typed
//     node with no name key, the way `PI()` becomes {"pi": {}}. ClickHouse
//     reads a bare word as an identifier, so such a node would re-open the
//     table read through an unrewritten IN operand.
//
// A spelling Polyglot refuses to parse is fine (the statement never reaches
// ClickHouse), except for a known keyword, which must keep parsing.
func TestBareWordShapesPinTheInBindingRule(t *testing.T) {
	e := newTestEngine(t)
	seen := map[string]bool{}
	for _, word := range bareWordSweep {
		if seen[word] {
			continue
		}
		seen[word] = true
		for _, spelling := range []string{word, strings.ToLower(word), "(" + word + ")", "((" + strings.ToLower(word) + "))"} {
			value, ok := parseWithValue(t, e, spelling)
			if !ok {
				if noParensKeywords[word] {
					t.Errorf("%s: known no-parens keyword spelling %q no longer parses as a WITH value", word, spelling)
				}
				continue
			}
			inner := unwrapParens(value)
			_, isKeyword := noParensKeywordName(inner)
			if noParensKeywords[word] && !isKeyword {
				t.Errorf("%s: spelling %q is no longer a no-parens, argument-less function node: %s", word, spelling, compactJSON(inner))
			}
			if !noParensKeywords[word] && isKeyword {
				t.Errorf("%s: spelling %q is now a no-parens function node; add it to noParensKeywords after measuring it on ClickHouse: %s", word, spelling, compactJSON(inner))
			}
			notTable := withValueIsNotTableReference(value)
			if bareWordLiterals[word] {
				if !notTable {
					t.Errorf("%s: literal spelling %q is no longer accepted as a value: %s", word, spelling, compactJSON(inner))
				}
				continue
			}
			if notTable {
				t.Errorf("%s: spelling %q parses into a node withValueIsNotTableReference accepts as not a table, but ClickHouse reads a bare word as an identifier: %s", word, spelling, compactJSON(inner))
			}
		}
	}
}

// parseWithValue parses `WITH <spelling> AS z SELECT 1` and returns the WITH
// element's value; ok is false when Polyglot does not produce exactly one WITH
// element.
func parseWithValue(t *testing.T, e Engine, spelling string) (any, bool) {
	t.Helper()
	ast, err := e.ParseOne("WITH " + spelling + " AS z SELECT 1")
	if err != nil {
		return nil, false
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		t.Fatalf("decode %q: %v", spelling, err)
	}
	sel, _ := root[NodeSelect].(map[string]any)
	with, _ := sel["with"].(map[string]any)
	ctes, _ := with["ctes"].([]any)
	if len(ctes) != 1 {
		return nil, false
	}
	cte, _ := ctes[0].(map[string]any)
	value, ok := cte["this"].(map[string]any)
	return value, ok
}

func unwrapParens(node any) map[string]any {
	m, _ := node.(map[string]any)
	for {
		paren, ok := m["paren"].(map[string]any)
		if !ok || len(m) != 1 {
			return m
		}
		m, _ = paren["this"].(map[string]any)
	}
}

func compactJSON(node any) string {
	b, _ := json.Marshal(node)
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

// identifierOperandWords are the bare words the pinned Polyglot parses into an
// `identifier` node (not a `column`) in IN-operand position. ClickHouse 26.2
// reads `a IN EXISTS` / `a IN interval` as the table of that name under both
// analyzers (review round 3, N4). Measured over 2,405 ClickHouse keywords and
// function names in three case spellings: these are the only ones.
var identifierOperandWords = map[string]bool{"EXISTS": true, "INTERVAL": true}

// TestBareWordInOperandsAreTables guards the direct-operand side of the rule:
// every bare word that parses as a single IN operand (infix, parenthesised,
// callable) decodes as a table operand, except the literals TRUE / FALSE /
// NULL. ClickHouse reads a bare word there as a table name. It also pins which
// words reach decodeInOperand as an `identifier` node or a no-parens function,
// so a Polyglot bump that adds a new shape fails here, naming the word.
func TestBareWordInOperandsAreTables(t *testing.T) {
	e := newTestEngine(t)
	seen := map[string]bool{}
	for _, word := range bareWordSweep {
		if seen[word] {
			continue
		}
		seen[word] = true
		for _, spelling := range []string{word, strings.ToLower(word)} {
			for _, form := range []string{"a IN %s", "a IN (%s)", "in(a, %s)"} {
				sql := "SELECT a FROM t WHERE " + strings.Replace(form, "%s", spelling, 1)
				operand, kind, ok := parseInOperand(t, e, sql)
				if !ok {
					if noParensKeywords[word] || identifierOperandWords[word] {
						t.Errorf("%s: %q no longer parses as a single IN operand", word, sql)
					}
					continue
				}
				inner := unwrapParens(operand)
				_, isIdentifier := inner["identifier"]
				if isIdentifier != identifierOperandWords[word] {
					t.Errorf("%s: %q identifier-node shape changed (identifier=%v); measure it on ClickHouse and update identifierOperandWords: %s",
						word, sql, isIdentifier, compactJSON(inner))
				}
				if _, isKeyword := noParensKeywordName(inner); isKeyword != noParensKeywords[word] {
					t.Errorf("%s: %q no-parens function shape changed (keyword=%v): %s", word, sql, isKeyword, compactJSON(inner))
				}
				wantTable := !bareWordLiterals[word]
				if (kind == inOperandTable) != wantTable {
					t.Errorf("%s: %q decodes as operand kind %d, want table=%v (ClickHouse reads a bare word as a table): %s",
						word, sql, kind, wantTable, compactJSON(inner))
				}
			}
		}
	}
}

// parseInOperand parses sql and returns the single IN operand of its WHERE
// clause (infix or callable) and decodeInOperand's classification of it.
func parseInOperand(t *testing.T, e Engine, sql string) (any, inOperandKind, bool) {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil || CheckParsedInFull(e, sql, ast) != nil {
		return nil, 0, false
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		t.Fatalf("decode %q: %v", sql, err)
	}
	sel, _ := root[NodeSelect].(map[string]any)
	where, _ := sel["where_clause"].(map[string]any)
	pred, _ := where["this"].(map[string]any)
	if in, ok := pred["in"].(map[string]any); ok {
		exprs, _ := in["expressions"].([]any)
		if len(exprs) != 1 {
			return nil, 0, false
		}
		kind, _ := decodeInNodeOperand(in)
		return exprs[0], kind, true
	}
	if fn, ok := pred["function"].(map[string]any); ok {
		args, _ := fn["args"].([]any)
		if len(args) != 2 {
			return nil, 0, false
		}
		kind, _ := decodeInOperand(args[1], false)
		return args[1], kind, true
	}
	return nil, 0, false
}

// TestCompoundOverBareKeywordIsNotProof pins containsNameReference's
// no-parens branch (review round 3, N6): a value built over a bare keyword
// carries a name, so it is never proof that a WITH alias is not a table. The
// same compounds over a parenthesised call stay proof.
func TestCompoundOverBareKeywordIsNotProof(t *testing.T) {
	e := newTestEngine(t)
	for _, c := range []struct {
		value    string
		notTable bool
	}{
		{"-NOW", false},
		{"NOW + 0", false},
		{"(now, 1)", false},
		{"[CURRENT_DATE]", false},
		{"CAST(NOW AS String)", false},
		{"-NOW()", true},
		{"NOW() + 0", true},
		{"(now(), 1)", true},
		{"CAST(NOW() AS String)", true},
	} {
		value, ok := parseWithValue(t, e, c.value)
		if !ok {
			t.Fatalf("%q does not parse as a WITH value", c.value)
		}
		if got := withValueIsNotTableReference(value); got != c.notTable {
			t.Errorf("withValueIsNotTableReference(%q) = %v, want %v: %s", c.value, got, c.notTable, compactJSON(value))
		}
	}
}
