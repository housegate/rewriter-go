package reverse

import "strings"

// WithheldMessage replaces an error text RewriteErrorMessage cannot return:
// the rewrite it would invert did not succeed, or the inverted text still
// names the request's physical routing. Byte-identical to rewriter-grpc's
// kWithheldErrorMessage.
const WithheldMessage = "Query failed. The error details were withheld: the rewriter could not map them " +
	"back to the original query within its limits."

// WithheldReasonMessage is the response message that says why the text was
// withheld, as rewriter-grpc reports it.
func WithheldReasonMessage(reason string) string { return "error message withheld: " + reason }

// ContainsIdentifier reports whether s mentions ident byte for byte with no
// [A-Za-z0-9_] on either side. Case-sensitive: ClickHouse prints a stored
// name as stored, and a folded match would read a keyword (DEFAULT) as a
// database called default. An empty ident is never mentioned. Mirrors
// rewriter-grpc rewriter_error::containsIdentifier.
func ContainsIdentifier(s, ident string) bool {
	if ident == "" {
		return false
	}
	for from := 0; from+len(ident) <= len(s); {
		i := strings.Index(s[from:], ident)
		if i < 0 {
			return false
		}
		st := from + i
		en := st + len(ident)
		if (st == 0 || !isWordByte(s[st-1])) && (en == len(s) || !isWordByte(s[en])) {
			return true
		}
		from = st + 1
	}
	return false
}

// MentionsHidden reports whether s still names any of the hidden identifiers
// (ContainsIdentifier) or literals (substring).
func MentionsHidden(s string, identifiers, literals []string) bool {
	for _, ident := range identifiers {
		if ContainsIdentifier(s, ident) {
			return true
		}
	}
	for _, lit := range literals {
		if lit != "" && strings.Contains(s, lit) {
			return true
		}
	}
	return false
}
