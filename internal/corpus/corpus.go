// Package corpus loads SQL test corpora. Files are UTF-8 SQL with statements
// separated by a line containing only "---".
package corpus

import (
	"os"
	"strings"
)

// Load reads one corpus file and returns its statements (trimmed, non-empty).
func Load(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, block := range strings.Split(string(raw), "\n---\n") {
		s := strings.TrimSpace(block)
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}
