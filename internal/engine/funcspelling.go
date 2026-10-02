package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Function spellings. The pinned Polyglot parses some ClickHouse function
// calls into typed nodes and generates each typed node under a name of its
// own, and it prints a few generic calls under another name too. Measured
// over every name in ClickHouse 26.8's system.functions, called with zero to
// three arguments (TestFunctionSpellingSweep), the respellings that change
// what ClickHouse executes are:
//
//	startsWith(a, b)      STARTS_WITH(a, b)      no such function
//	toTypeName(a)         TYPEOF(a)              no such function
//	CHAR_LENGTH(a)        LENGTH(a)              characters become bytes
//	CHARACTER_LENGTH(a)   LENGTH(a)              characters become bytes
//	instr(a, b[, c])      POSITION(a, b[, c])    case-insensitive becomes case-sensitive
//	locate(a, b[, c])     POSITION(b, a[, c])    accepted, but spelled apart
//	trim(a, b)            TRIM(b FROM a)         a syntax error
//	cume_dist(…)          CUME_DIST(…)           no such function (case-sensitive)
//	match(a, b)           MATCH(a, b)            no such function (case-sensitive)
//	toStartOfDay(a)       dateTrunc('DAY', a)    DateTime64 results differ
//
// The mid-statement drop gate refuses every one whose spelling changes
// (CheckRegenerated), but folds case, so MATCH and CUME_DIST used to pass as
// Success with SQL ClickHouse rejects. restoreFunctionSpellings puts the
// client's own spelling back on these nodes right after parsing, so the
// generator prints the call as the client wrote it and the gate compares that
// text: a typed node becomes a plain function node with the same argument
// subtrees in the client's argument order, and a generic node is pinned to its
// spelling through Polyglot's qualified_name, which the generator prints
// verbatim instead of applying its name normalizations.
//
// Polyglot keeps no source position on these nodes, so the spelling is read
// from the statement's tokens: a node is restored only when every call in the
// statement (a bare word directly followed by "(") whose upper-cased name is
// one Polyglot parses into that node's kind has one and the same spelling, and
// that spelling is a restorable one. Otherwise the node is left alone and the
// drop gate decides, exactly as before (startsWith(a, 'x') OR STARTSWITH(b,
// 'y') is still refused). A wrong guess cannot pass the gate either: a node
// that did not come from such a call is printed with a name the input does not
// spell there, and the gate compares the multiset of spellings.
//
// Respellings that stay refused, fail closed: first_value / last_value /
// ntile with more than one argument and JSON_QUERY / JSON_VALUE with one
// (ClickHouse rejects each), and group_concat(a, sep), which Polyglot turns
// into GROUP_CONCAT(CONCAT(a, sep)). max_by / min_by, printed argMax / argMin,
// are ClickHouse aliases folded by spellingClass.

// functionSpellingRule describes one node kind whose spelling is restored.
type functionSpellingRule struct {
	// parsedFrom lists the upper-cased call names Polyglot parses into the
	// node kind; every call with one of these names must share the spelling.
	parsedFrom []string
	// restorable lists the upper-cased spellings that are put back; any
	// other shared spelling leaves the node alone.
	restorable []string
	// args returns the call's arguments in the client's order for a node of
	// this kind spelled spelling, or ok=false when the node has a shape the
	// rule does not model.
	args func(spelling string, body map[string]any) (args []any, ok bool)
}

// typedFunctionSpellings maps a typed node kind to its rule.
var typedFunctionSpellings = map[string]functionSpellingRule{
	"starts_with": {
		parsedFrom: []string{"STARTSWITH", "STARTS_WITH"},
		restorable: []string{"STARTSWITH"},
		args:       typedArgs([]string{"this", "expression"}, nil),
	},
	"typeof": {
		parsedFrom: []string{"TOTYPENAME", "TYPEOF"},
		restorable: []string{"TOTYPENAME"},
		args:       typedArgs([]string{"this"}, nil),
	},
	"length": {
		parsedFrom: []string{"LENGTH", "CHAR_LENGTH", "CHARACTER_LENGTH", "LEN"},
		restorable: []string{"CHAR_LENGTH", "CHARACTER_LENGTH"},
		args:       typedArgs([]string{"this"}, nil),
	},
	"str_position": {
		parsedFrom: []string{"INSTR", "LOCATE", "POSITION", "STRPOS"},
		restorable: []string{"INSTR", "LOCATE"},
		args: func(spelling string, body map[string]any) ([]any, bool) {
			// instr(haystack, needle[, start]) is parsed as this = haystack,
			// substr = needle; locate(needle, haystack[, start]) as the same
			// fields in the other order.
			order := []string{"this", "substr"}
			if strings.EqualFold(spelling, "LOCATE") {
				order = []string{"substr", "this"}
			}
			return typedArgs(order, []string{"position"})(spelling, body)
		},
	},
	"trim": {
		parsedFrom: []string{"TRIM"},
		restorable: []string{"TRIM"},
		args: func(spelling string, body map[string]any) ([]any, bool) {
			// Only the two-argument function form trim(s, chars); TRIM(s) and
			// the SQL-standard TRIM([BOTH|LEADING|TRAILING] c FROM s) forms
			// regenerate as written.
			if body["characters"] == nil || body["position"] != "Both" ||
				body["sql_standard_syntax"] != false || body["position_explicit"] != false {
				return nil, false
			}
			return typedArgs([]string{"this", "characters"}, nil, "position", "sql_standard_syntax", "position_explicit")(spelling, body)
		},
	},
	"cume_dist": {
		parsedFrom: []string{"CUME_DIST"},
		restorable: []string{"CUME_DIST"},
		args: func(_ string, body map[string]any) ([]any, bool) {
			for k, v := range body {
				if k != "args" && !inertField(v) {
					return nil, false
				}
			}
			switch a := body["args"].(type) {
			case nil:
				return []any{}, true
			case []any:
				return a, true
			}
			return nil, false
		},
	},
}

// genericFunctionSpellings maps an upper-cased generic function name to its
// rule; the node keeps its kind and arguments and is pinned to the spelling.
var genericFunctionSpellings = map[string]functionSpellingRule{
	// Polyglot's parser upper-cases match (a keyword) in the node's name.
	"MATCH": {parsedFrom: []string{"MATCH"}, restorable: []string{"MATCH"}},
	// The generator prints a one-argument toStartOfDay as dateTrunc('DAY', …).
	"TOSTARTOFDAY": {parsedFrom: []string{"TOSTARTOFDAY"}, restorable: []string{"TOSTARTOFDAY"}},
}

// typedArgs returns an args func that reads the required fields in order,
// then the optional ones while they are set (an optional field after an unset
// one must be unset too). Every other field must hold its default (inertField),
// except the checked ones, which the caller has already validated.
func typedArgs(required, optional []string, checked ...string) func(string, map[string]any) ([]any, bool) {
	return func(_ string, body map[string]any) ([]any, bool) {
		known := map[string]bool{}
		for _, k := range checked {
			known[k] = true
		}
		var args []any
		for _, k := range required {
			known[k] = true
			v, ok := body[k].(map[string]any)
			if !ok {
				return nil, false
			}
			args = append(args, v)
		}
		unset := false
		for _, k := range optional {
			known[k] = true
			switch v := body[k].(type) {
			case nil:
				unset = true
			case map[string]any:
				if unset {
					return nil, false
				}
				args = append(args, v)
			default:
				return nil, false
			}
		}
		for k, v := range body {
			if !known[k] && !inertField(v) {
				return nil, false
			}
		}
		return args, true
	}
}

// inertField reports a field value that carries nothing: null, false, an
// empty string, list or object.
func inertField(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// functionSpellingHints are the lower-cased fragments of every name a rule
// restores; a statement containing none of them is left untouched without
// tokenizing it.
var functionSpellingHints = []string{"startswith", "totypename", "char_length", "character_length",
	"instr", "locate", "trim", "cume_dist", "match", "tostartofday"}

// restoreFunctionSpellings applies the rules above to a freshly parsed AST of
// sql (see the comment at the top of this file). A statement that names no
// restorable function, and an AST without a node a rule covers, is returned
// unchanged.
func restoreFunctionSpellings(e Engine, sql string, ast AST) (AST, error) {
	lower := strings.ToLower(sql)
	hinted := false
	for _, h := range functionSpellingHints {
		if strings.Contains(lower, h) {
			hinted = true
			break
		}
	}
	if !hinted {
		return ast, nil
	}
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("engine: parse: decode AST for function spellings: %w", err)
	}
	typed := map[string][]map[string]any{}   // kind → wrapper nodes
	generic := map[string][]map[string]any{} // upper-cased name → function bodies
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if len(v) == 1 {
				for k, body := range v {
					if b, ok := body.(map[string]any); ok {
						if _, ok := typedFunctionSpellings[k]; ok {
							typed[k] = append(typed[k], v)
						} else if k == "function" {
							if name, _ := b["name"].(string); name != "" {
								if _, ok := genericFunctionSpellings[strings.ToUpper(name)]; ok {
									generic[strings.ToUpper(name)] = append(generic[strings.ToUpper(name)], b)
								}
							}
						}
					}
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(root)
	if len(typed) == 0 && len(generic) == 0 {
		return ast, nil
	}
	calls, err := callSpellings(e, sql)
	if err != nil {
		return nil, err
	}
	changed := false
	for kind, nodes := range typed {
		rule := typedFunctionSpellings[kind]
		spelling, ok := sharedSpelling(calls, rule)
		if !ok {
			continue
		}
		for _, wrapper := range nodes {
			body, _ := wrapper[kind].(map[string]any)
			args, ok := rule.args(spelling, body)
			if !ok {
				continue
			}
			delete(wrapper, kind)
			wrapper["function"] = pinnedFunction(spelling, args)
			changed = true
		}
	}
	for name, bodies := range generic {
		rule := genericFunctionSpellings[name]
		spelling, ok := sharedSpelling(calls, rule)
		if !ok {
			continue
		}
		for _, body := range bodies {
			if !plainFunctionCall(body) || (name == "TOSTARTOFDAY" && len(asList(body["args"])) != 1) {
				continue
			}
			body["name"] = spelling
			body["qualified_name"] = []any{pinnedName(spelling)}
			changed = true
		}
	}
	if !changed {
		return ast, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("engine: parse: encode AST after function spellings: %w", err)
	}
	return AST(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// callSpellings maps each upper-cased call name in sql (a bare word token
// directly followed by "(") to the set of its source spellings.
func callSpellings(e Engine, sql string) (map[string]map[string]bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil, err
	}
	calls := map[string]map[string]bool{}
	for i := 0; i+1 < len(toks); i++ {
		tk := toks[i]
		if toks[i+1].TokenType != "L_PAREN" || isQuotedLexeme(tk.TokenType) || tk.Source == "" || !isWordStart(tk.Source) {
			continue
		}
		u := strings.ToUpper(tk.Source)
		if calls[u] == nil {
			calls[u] = map[string]bool{}
		}
		calls[u][tk.Source] = true
	}
	return calls, nil
}

// sharedSpelling returns the one spelling every call parsed into the rule's
// kind shares, when there is exactly one and it is restorable.
func sharedSpelling(calls map[string]map[string]bool, rule functionSpellingRule) (string, bool) {
	var spellings []string
	for _, name := range rule.parsedFrom {
		for s := range calls[name] {
			spellings = append(spellings, s)
		}
	}
	if len(spellings) != 1 {
		return "", false
	}
	for _, r := range rule.restorable {
		if strings.ToUpper(spellings[0]) == r {
			return spellings[0], true
		}
	}
	return "", false
}

// plainFunctionCall reports a generic function node with nothing but a name
// and arguments: unquoted, with parentheses, no DISTINCT, no bracket syntax,
// no qualified name and no error behaviour.
func plainFunctionCall(body map[string]any) bool {
	for k, v := range body {
		switch k {
		case "name", "args", "span", "trailing_comments":
		default:
			if !inertField(v) {
				return false
			}
		}
	}
	return true
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// pinnedName is the single unquoted name component Polyglot's generator
// prints verbatim for a function with a qualified_name.
func pinnedName(spelling string) map[string]any {
	return map[string]any{"name": spelling, "quoted": false, "trailing_comments": []any{}}
}

// pinnedFunction is a plain function node printed as spelling(args…).
func pinnedFunction(spelling string, args []any) map[string]any {
	if args == nil {
		args = []any{}
	}
	return map[string]any{
		"name": spelling, "args": args, "distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
		"qualified_name": []any{pinnedName(spelling)},
	}
}
