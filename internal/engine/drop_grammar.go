package engine

// PlainDropTable reports whether sql is exactly
//
//	DROP TABLE [IF EXISTS] name [, name ...] [SYNC | NO DELAY] [;]
//
// where each name is `table` or `database.table` (plain or quoted). It is
// the grammar the V2 storage-integrity contract accepts for DROP TABLE of a
// logical SI table. Polyglot's drop_table node silently discards ON CLUSTER,
// TEMPORARY, IF EMPTY, FORMAT and SETTINGS, so the AST alone cannot prove a
// drop is plain; the token stream can. Comments are not tokens.
func PlainDropTable(e Engine, sql string) (bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false, err
	}
	if n := len(toks); n > 0 && toks[n-1].TokenType == "SEMICOLON" {
		toks = toks[:n-1]
	}
	if len(toks) < 3 || !tokenTextIs(toks[0], "DROP") || !tokenTextIs(toks[1], "TABLE") ||
		toks[1].TokenType != "TABLE" {
		return false, nil
	}
	i := 2
	if i+1 < len(toks) && toks[i].TokenType == "IF" && tokenTextIs(toks[i+1], "EXISTS") {
		i += 2
	}
	for {
		next, ok := consumeMutationQualifiedName(toks, i)
		if !ok {
			return false, nil
		}
		i = next
		if i < len(toks) && toks[i].TokenType == "COMMA" {
			i++
			continue
		}
		break
	}
	switch {
	case i == len(toks):
		return true, nil
	case i+1 == len(toks) && toks[i].TokenType == "VAR" && tokenTextIs(toks[i], "SYNC"):
		return true, nil
	case i+2 == len(toks) && tokenTextIs(toks[i], "NO") && toks[i+1].TokenType == "VAR" && tokenTextIs(toks[i+1], "DELAY"):
		return true, nil
	default:
		return false, nil
	}
}
