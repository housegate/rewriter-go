package engine

import "strconv"

func randNodeHasArguments(node any) bool {
	if node == nil {
		return false
	}
	body, ok := node.(map[string]any)
	if !ok {
		return true
	}
	for _, key := range []string{"seed", "lower", "upper"} {
		if body[key] != nil {
			return true
		}
	}
	return false
}

func specialNodeHasArguments(node any) bool {
	if node == nil {
		return false
	}
	body, ok := node.(map[string]any)
	if !ok {
		return true
	}
	for key, value := range body {
		if key == "sysdate" {
			sysdate, _ := value.(bool)
			if sysdate {
				return true
			}
			continue
		}
		if value != nil {
			return true
		}
	}
	return false
}

func functionArgs(fn map[string]any) []any {
	args, _ := fn["args"].([]any)
	return args
}

func bareColumnName(column map[string]any) (string, bool) {
	if column["table"] != nil {
		return "", false
	}
	nameNode, ok := column["name"].(map[string]any)
	if !ok {
		return "", false
	}
	quoted, _ := nameNode["quoted"].(bool)
	if quoted {
		return "", false
	}
	name, ok := nameNode["name"].(string)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

func isKnownNondeterministic(name string) bool {
	switch name {
	case "any",
		"anylast",
		"blocknumber",
		"blocksize",
		"curdate",
		"current_date",
		"current_timestamp",
		"datetimetouuidv7",
		"fuzzbits",
		"fuzzquery",
		"generaterandomstructure",
		"generateserialid",
		"generatesnowflakeid",
		"generateuuidv4",
		"generateuuidv7",
		"localtime",
		"localtimestamp",
		"now",
		"now64",
		"nowinblock",
		"nowinblock64",
		"obfuscatequery",
		"quantile",
		"quantiles",
		"rand",
		"rand32",
		"rand64",
		"randbernoulli",
		"randbinomial",
		"randcanonical",
		"randchisquared",
		"randconstant",
		"randexponential",
		"randfisherf",
		"randlognormal",
		"randnegativebinomial",
		"randnormal",
		"randpoisson",
		"randstudentt",
		"randuniform",
		"random",
		"randomfixedstring",
		"randomprintableascii",
		"randomstring",
		"randomstringutf8",
		"rownumberinallblocks",
		"rownumberinblock",
		"runningaccumulate",
		"runningconcurrency",
		"runningdifference",
		"runningdifferencestartingwithfirstvalue",
		"today",
		"utc_timestamp",
		"utctimestamp",
		"yesterday":
		return true
	default:
		return false
	}
}

func isSupportedNondeterministic(name string) bool {
	switch name {
	case "curdate",
		"current_date",
		"current_timestamp",
		"generateuuidv4",
		"localtime",
		"localtimestamp",
		"now",
		"now64",
		"rand",
		"rand32",
		"rand64",
		"randcanonical",
		"random",
		"today",
		"utc_timestamp",
		"utctimestamp",
		"yesterday":
		return true
	default:
		return false
	}
}

func functionCall(name string, args ...map[string]any) map[string]any {
	a := make([]any, 0, len(args))
	for _, arg := range args {
		a = append(a, arg)
	}
	return map[string]any{"function": map[string]any{
		"name": name, "args": a, "distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
	}}
}

func numLiteralUint(n uint64) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "number", "value": strconv.FormatUint(n, 10)}}
}

func numLiteralFloat(value string) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "number", "value": value}}
}

func replaceMap(dst map[string]any, src map[string]any) {
	clear(dst)
	for k, v := range src {
		dst[k] = v
	}
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f'
}
