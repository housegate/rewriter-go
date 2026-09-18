//go:build linux

package engine

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

func snapshotTestFFI(t *testing.T) string {
	t.Helper()
	p := os.Getenv("SNAPSHOT_MEASURED_FFI")
	if p == "" {
		t.Skip("set SNAPSHOT_MEASURED_FFI for the explicit Linux measured lane")
	}
	return p
}
func snapshotCopyFFI(t *testing.T) (string, []byte) {
	t.Helper()
	b, err := os.ReadFile(snapshotTestFFI(t))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ffi.so")
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p, b
}
func TestSnapshotMeasuredSealedSourceAndLifetime(t *testing.T) {
	path, b := snapshotCopyFFI(t)
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// After the ONE source open, remove the name and install unrelated bytes.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replacement must never be reopened"), 0600); err != nil {
		t.Fatal(err)
	}
	sealed, err := sealSnapshotSource(source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer sealed.Close()
	got, err := hashSnapshotFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("0x%x", sha256.Sum256(b)); got != want {
		t.Fatalf("source reopened: %s != %s", got, want)
	}
	st, err := sealed.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, st.Size() + 1} {
		if err = sealed.Truncate(size); !errors.Is(err, unix.EPERM) {
			t.Fatalf("truncate %d: %v", size, err)
		}
	}
	if _, err = sealed.WriteAt([]byte("x"), 0); !os.IsPermission(err) {
		t.Fatalf("sealed write: %v", err)
	}
	if _, err = unix.FcntlInt(sealed.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_FUTURE_WRITE); err != unix.EPERM {
		t.Fatalf("seal modification: %v", err)
	}
	e, err := loadSnapshotSealed(sealed)
	if err != nil {
		t.Fatal(err)
	}
	fd := sealed.Fd()
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		a, err := e.ParseOne("SELECT 7")
		if err != nil || len(a) == 0 {
			t.Fatalf("sealed execution: %s %v", a, err)
		}
	}
	if _, err = os.Stat(fmt.Sprintf("/proc/self/fd/%d", fd)); err != nil {
		t.Fatal("descriptor not retained", err)
	}
	if err = verifySnapshotSymbols(sealed, e.handle); err != nil {
		t.Fatal(err)
	}
	t.Logf("source replacement/deletion ignored; digest=%s fd=%d seals=0x%x write/grow/shrink/seal-change=EPERM; actual FFI calls and symbol mappings PASS", got, fd, snapshotSeals)
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(fmt.Sprintf("/proc/self/fd/%d", fd)); !os.IsNotExist(err) {
		t.Fatal("descriptor survived Close", err)
	}
	if _, err = e.ParseOne("SELECT 8"); err == nil {
		t.Fatal("operation after Close succeeded")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestSnapshotMeasuredRepeatedConcurrentClose(t *testing.T) {
	path := snapshotTestFFI(t)
	for round := 0; round < 12; round++ {
		e, id, err := NewMeasuredPolyglot(path)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if id.Platform != "linux/"+runtime.GOARCH || len(id.ExecutableDigest) != 66 || len(id.FFIDigest) != 66 {
			t.Fatal(id)
		}
		if _, err = e.ParseOne("SELECT 1"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 20; k++ {
					_, _ = e.ParseOne("SELECT (1), (2), (3)")
				}
			}()
		}
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := e.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if _, err = e.ParseOne("SELECT 1"); err == nil {
			t.Fatal("operation after Close")
		}
		t.Logf("round=%d actual concurrent parse/Close id=%+v", round, id)
	}
}
func TestSnapshotMeasuredRefusesDependenciesAndCleansFailures(t *testing.T) {
	_, original := snapshotCopyFFI(t)
	// Warm the loader before descriptor accounting (Go opens epoll lazily).
	e, _, err := NewMeasuredPolyglot(snapshotTestFFI(t))
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	before, _ := os.ReadDir("/proc/self/fd")
	for _, kind := range []string{"dependency", "runpath", "nodelete", "soname", "invalid", "wrong_machine", "missing_symbols", "missing_one_symbol", "dynamic_section_mismatch"} {
		t.Run(kind, func(t *testing.T) {
			b := append([]byte(nil), original...)
			switch kind {
			case "dependency":
				b = bytes.Replace(b, []byte("libgcc_s.so.1"), []byte("libbad_s.so.1"), 1)
			case "invalid":
				b = []byte("not ELF")
			case "wrong_machine":
				binary.LittleEndian.PutUint16(b[18:20], uint16(elf.EM_386))
			case "dynamic_section_mismatch":
				shoff := binary.LittleEndian.Uint64(b[40:48])
				entsize := uint64(binary.LittleEndian.Uint16(b[58:60]))
				count := uint64(binary.LittleEndian.Uint16(b[60:62]))
				found := false
				for i := uint64(0); i < count; i++ {
					off := shoff + i*entsize
					if binary.LittleEndian.Uint32(b[off+4:]) == uint32(elf.SHT_DYNAMIC) {
						old := binary.LittleEndian.Uint64(b[off+24:])
						binary.LittleEndian.PutUint64(b[off+24:], old+16)
						found = true
						break
					}
				}
				if !found {
					t.Fatal("dynamic section missing")
				}
			case "missing_one_symbol":
				b = bytes.Replace(b, []byte("polyglot_parse_one\x00"), []byte("polyglox_parse_one\x00"), 1)
			case "missing_symbols":
				b = bytes.ReplaceAll(b, []byte("polyglot_"), []byte("polyglox_"))
			default:
				f, err := elf.NewFile(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				section := f.SectionByType(elf.SHT_DYNAMIC)
				found := false
				for off := section.Offset; off < section.Offset+section.Size; off += 16 {
					if elf.DynTag(binary.LittleEndian.Uint64(b[off:])) == elf.DT_RELACOUNT {
						tag := elf.DT_RUNPATH
						value := uint64(1)
						if kind == "nodelete" {
							tag = elf.DT_FLAGS_1
							value = uint64(elf.DF_1_NODELETE)
						}
						if kind == "soname" {
							tag = elf.DT_SONAME
						}
						binary.LittleEndian.PutUint64(b[off:], uint64(tag))
						binary.LittleEndian.PutUint64(b[off+8:], value)
						found = true
						break
					}
				}
				if !found {
					t.Fatal("mutation tag absent")
				}
			}
			p := filepath.Join(t.TempDir(), kind+".so")
			if err = os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				e, _, err := NewMeasuredPolyglot(p)
				if err == nil {
					e.Close()
					t.Fatal("invalid ELF accepted")
				}
				t.Log(err)
			}
		})
	}
	after, _ := os.ReadDir("/proc/self/fd")
	if len(before) != len(after) {
		t.Fatalf("failure leaked descriptors: %d -> %d", len(before), len(after))
	}
	t.Logf("failure cleanup fd_count=%d -> %d", len(before), len(after))
}
func TestSnapshotMeasuredNoFallback(t *testing.T) {
	ffi := snapshotTestFFI(t)
	t.Setenv("POLYGLOT_SQL_FFI_PATH", ffi)
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing.so")} {
		e, _, err := NewMeasuredPolyglot(p)
		if err == nil {
			e.Close()
			t.Fatal("used environment fallback")
		}
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if e, _, err := NewMeasuredPolyglot(fifo); err == nil {
		e.Close()
		t.Fatal("FIFO accepted")
	}
	t.Setenv("LD_LIBRARY_PATH", "/tmp/untrusted")
	if e, _, err := NewMeasuredPolyglot(ffi); err == nil || !strings.Contains(err.Error(), "LD_LIBRARY_PATH") {
		if e != nil {
			e.Close()
		}
		t.Fatal("loader override accepted", err)
	}
}
func TestSnapshotMeasuredDetectsCachedDescriptorAlias(t *testing.T) {
	// An intentionally retained NODELETE loader reference simulates a stale
	// pathname cache. It is test-owned until process exit; no production flag.
	path, _ := snapshotCopyFFI(t)
	s, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := sealSnapshotSource(s)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	fd := old.Fd()
	h, err := purego.Dlopen(fmt.Sprintf("/proc/self/fd/%d", fd), purego.RTLD_NOW|0x1000)
	if err != nil {
		t.Fatal(err)
	}
	purego.Dlclose(h)
	old.Close()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := sealSnapshotSource(source)
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	// Force the exact stale FD pathname using dup3, retaining the new inode.
	if fresh.Fd() != fd {
		if err = unix.Dup3(int(fresh.Fd()), int(fd), unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		fresh.Close()
	}
	alias := fresh
	if fresh.Fd() != fd {
		alias = os.NewFile(fd, "alias")
	}
	defer alias.Close()
	h, err = purego.Dlopen(fmt.Sprintf("/proc/self/fd/%d", fd), purego.RTLD_NOW|snapshotDeepBind)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(h)
	if err = verifySnapshotSymbols(alias, h); err == nil {
		t.Fatal("stale loader cache accepted a different inode")
	}
	t.Logf("fd=%d stale cached handle rejected: %v", fd, err)
}

func TestSnapshotMeasuredCoexistsWithOrdinaryGlobalImage(t *testing.T) {
	path, _ := snapshotCopyFFI(t)
	ordinary, err := NewPolyglot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	measured, _, err := NewMeasuredPolyglot(snapshotTestFFI(t))
	if err != nil {
		t.Fatal(err)
	}
	defer measured.Close()
	// Both contain the same exported symbol names but different mapped inodes.
	// Every measured entrypoint must remain bound to the sealed object.
	e := measured.(*snapshotMeasuredEngine)
	if err = verifySnapshotSymbols(e.sealed, e.handle); err != nil {
		t.Fatal(err)
	}
	for _, engine := range []Engine{ordinary, measured} {
		ast, err := engine.ParseOne("SELECT 123")
		if err != nil {
			t.Fatal(err)
		}
		sql, err := engine.Generate(ast)
		if err != nil || !strings.Contains(sql, "123") {
			t.Fatalf("real coexisting FFI: %s %v", sql, err)
		}
	}
	t.Log("ordinary RTLD_GLOBAL and measured DEEPBIND images coexist; sealed symbol mappings and both real FFI calls PASS")
}
