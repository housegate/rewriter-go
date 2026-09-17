//go:build linux

package rewriter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func measuredProfileInputs(t *testing.T) (string, string, snapshotProfileFile) {
	t.Helper()
	lib, path := os.Getenv("SNAPSHOT_MEASURED_FFI"), os.Getenv("SNAPSHOT_MEASURED_PROFILE")
	if lib == "" || path == "" {
		t.Skip("compile first, externally generate profile, run unchanged binary with SNAPSHOT_MEASURED_FFI/PROFILE")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeSnapshotProfiles(raw)
	if err != nil {
		t.Fatal(err)
	}
	return lib, path, f
}
func TestSnapshotProfileMeasuredService(t *testing.T) {
	lib, path, f := measuredProfileInputs(t)
	s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.measuredSnapshotProfiles) != 1 {
		t.Fatalf("expected actual measured membership, got %d", len(s.measuredSnapshotProfiles))
	}
	q := f.Profiles[0].QueryProfileID
	if s.measuredSnapshotProfiles[q] == "" {
		t.Fatal("current Q not retained")
	}
	resp, err := s.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: "SELECT 7"})
	if err != nil || resp.Code != pb.RewriteCode_Success {
		t.Fatalf("ordinary real FFI: %v %v", resp, err)
	}
	// Invalid versions and unavailable identities never claim capability.
	for _, requested := range []string{q, "0x" + strings.Repeat("0", 64), ""} {
		a, err := s.AnalyzeSnapshotQuery(context.Background(), &pb.AnalyzeSnapshotQueryRequest{QueryProfileId: requested, ContractVersion: 1, Sql: "SELECT 7"})
		if requested == q {
			if err != nil || a.Code != pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY || a.QueryProfileId != q || a.ContractVersion != 1 {
				t.Fatalf("measured known Q classification: %v %v", a, err)
			}
		} else if err != nil || a.Code != pb.SnapshotQueryCode_PROFILE_UNAVAILABLE || a.GetQueryProfileId() != "" || a.GetContractVersion() != 0 {
			t.Fatalf("stub acknowledged semantics: %v %v", a, err)
		}
		p, err := s.PrepareSnapshotQuery(context.Background(), &pb.PrepareSnapshotQueryRequest{Analysis: &pb.AnalyzeSnapshotQueryRequest{QueryProfileId: requested, ContractVersion: 1, Sql: "SELECT 7"}})
		if requested == q {
			if err != nil || p.Code != pb.SnapshotQueryCode_INVALID_INPUT || p.QueryProfileId != q || p.ContractVersion != 1 {
				t.Fatalf("known ordinary preparation: %v %v", p, err)
			}
		} else if err != nil || p.Code != pb.SnapshotQueryCode_PROFILE_UNAVAILABLE || p.GetQueryProfileId() != "" || p.GetContractVersion() != 0 {
			t.Fatalf("stub acknowledged preparation: %v %v", p, err)
		}
	}
	// File lifetime cannot mutate the immutable local map.
	copyPath := filepath.Join(t.TempDir(), "profile.json")
	raw, _ := os.ReadFile(path)
	os.WriteFile(copyPath, raw, 0600)
	copyService, err := NewServiceWithSnapshotQueryProfiles(lib, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copyService.Close()
	if err = os.WriteFile(copyPath, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if copyService.measuredSnapshotProfiles[q] != s.measuredSnapshotProfiles[q] {
		t.Fatal("installed entry changed with file")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: "SELECT 1"})
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	resp, err = s.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: "SELECT 1"})
	if err != nil || resp.Code == pb.RewriteCode_Success {
		t.Fatal("ordinary operation after Close succeeded")
	}
	t.Logf("actual local N=%s Q=%s SQLplatform=%s; known Q has typed classification; unknown Q stays unavailable; immutable file and concurrent Close PASS", f.Profiles[0].Record.NativeAnalyzerBuildDigest, q, f.Profiles[0].Record.Platform)
}
func TestSnapshotProfileMeasuredMutations(t *testing.T) {
	lib, path, _ := measuredProfileInputs(t)
	for _, kind := range []string{"ffi", "platform", "executable_member", "ffi_member", "sql_platform", "nonlocal_fixture", "malformed_nonlocal"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := os.ReadFile(path)
			f, err := decodeSnapshotProfiles(raw)
			if err != nil {
				t.Fatal(err)
			}
			ffi := lib
			wantLocal := false
			wantErr := false
			switch kind {
			case "ffi":
				b, err := os.ReadFile(lib)
				if err != nil {
					t.Fatal(err)
				}
				ffi = filepath.Join(t.TempDir(), "changed.so")
				b = append(b, 0)
				if err = os.WriteFile(ffi, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "platform":
				for i := range f.Profiles[0].NativeArtifactSet.Artifacts {
					f.Profiles[0].NativeArtifactSet.Artifacts[i].Platform = "linux/other"
				}
			case "executable_member":
				f.Profiles[0].NativeArtifactSet.Artifacts = []snapshotArtifact{{Platform: runtime.GOOS + "/" + runtime.GOARCH, ExecutableDigest: "0x" + strings.Repeat("0", 64), FFIDigest: f.Profiles[0].NativeArtifactSet.Artifacts[0].FFIDigest}}
			case "ffi_member":
				for i := range f.Profiles[0].NativeArtifactSet.Artifacts {
					f.Profiles[0].NativeArtifactSet.Artifacts[i].FFIDigest = "0x" + strings.Repeat("0", 64)
				}
			case "sql_platform":
				f.Profiles[0].Record.Platform = "different/sql-platform"
				wantLocal = true
			case "nonlocal_fixture", "malformed_nonlocal":
				v := loadProfileVector(t)
				f, err = decodeSnapshotProfiles([]byte(v.File))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "malformed_nonlocal" {
					f.Profiles[1].Record.Limits.MaxSQLBytes = 0
					wantErr = true
				}
			}
			if kind != "nonlocal_fixture" && kind != "malformed_nonlocal" {
				f.Profiles[0].Record.NativeAnalyzerBuildDigest = snapshotCanonicalDigest("snapshot-native-analyzer-artifact-set-v1", f.Profiles[0].NativeArtifactSet)
				f.Profiles[0].QueryProfileID = snapshotCanonicalDigest("snapshot-query-profile-v1", f.Profiles[0].Record)
			}
			raw, _ = json.Marshal(f)
			p := filepath.Join(t.TempDir(), "profile.json")
			if err = os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewServiceWithSnapshotQueryProfiles(ffi, p)
			if wantErr {
				if err == nil {
					s.Close()
					t.Fatal("malformed nonlocal entry disappeared")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if (len(s.measuredSnapshotProfiles) > 0) != wantLocal {
				t.Fatalf("wrong local membership: %v", s.measuredSnapshotProfiles)
			}
			t.Logf("mutation=%s local_count=%d", kind, len(s.measuredSnapshotProfiles))
		})
	}
}
func TestSnapshotProfileMeasuredChangedExecutable(t *testing.T) {
	lib, path, _ := measuredProfileInputs(t)
	b, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "changed.test")
	b = append(b, 0)
	if err = os.WriteFile(exe, b, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSnapshotProfileMeasuredChangedExecutableChild$", "-test.v")
	cmd.Env = append(os.Environ(), "SNAPSHOT_MUTATED_CHILD=1", "SNAPSHOT_MEASURED_FFI="+lib, "SNAPSHOT_MEASURED_PROFILE="+path)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatal(err)
	}
}
func TestSnapshotProfileMeasuredChangedExecutableChild(t *testing.T) {
	if os.Getenv("SNAPSHOT_MUTATED_CHILD") == "" {
		return
	}
	lib, path, _ := measuredProfileInputs(t)
	s, err := NewServiceWithSnapshotQueryProfiles(lib, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.measuredSnapshotProfiles) != 0 {
		t.Fatal("changed executable accepted old measured Q")
	}
	t.Log("actual modified executable refuses old Q")
}
