//go:build !linux

package rewriter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotProfileNonLinuxRefusesMeasured(t *testing.T) {
	v := loadProfileVector(t)
	p := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(p, []byte(v.File), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServiceWithSnapshotQueryProfiles("explicit-ffi", p)
	if err == nil {
		s.Close()
		t.Fatal("unsupported OS accepted measured loading")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatal(err)
	}
}
