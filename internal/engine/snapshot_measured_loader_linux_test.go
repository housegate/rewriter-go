//go:build linux

package engine

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
)

func TestSnapshotMeasuredLoaderABI(t *testing.T) {
	const loader = "ld-linux-x86-64.so.2"
	valid := elf.Symbol{Name: "__tls_get_addr", Library: loader, Version: "GLIBC_2.3", Info: byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_FUNC), Section: elf.SHN_UNDEF}
	for _, machine := range []elf.Machine{elf.EM_X86_64, elf.EM_AARCH64, elf.EM_386, elf.EM_NONE} {
		t.Run(machine.String(), func(t *testing.T) {
			want := machine == elf.EM_X86_64
			if got := snapshotRuntimeLibrary(loader, machine); got != want {
				t.Errorf("loader dependency=%v, want %v", got, want)
			}
			if got := snapshotRuntimeImport(valid, machine); got != want {
				t.Errorf("loader import=%v, want %v", got, want)
			}
			for _, lib := range []string{"ld-linux-aarch64.so.1", "ld-linux-x86-64.so.3", "xld-linux-x86-64.so.2", "/lib64/" + loader, "libsemantic.so"} {
				s := valid
				s.Library = lib
				if snapshotRuntimeLibrary(lib, machine) || snapshotRuntimeImport(s, machine) {
					t.Errorf("accepted foreign/path/lookalike provider %q", lib)
				}
			}
		})
	}
	for _, version := range []string{"", "GCC_3.0", "GLIBC_PRIVATE", "GLIBC_2.2.5", "GLIBC_2.34", "GLIBC_2.3.extra"} {
		s := valid
		s.Version = version
		if snapshotRuntimeImport(s, elf.EM_X86_64) {
			t.Errorf("accepted loader version %q", version)
		}
	}
	for _, name := range []string{"semantic_hook", "__tls_get_addr_extra", ""} {
		s := valid
		s.Name = name
		if snapshotRuntimeImport(s, elf.EM_X86_64) {
			t.Errorf("accepted loader symbol %q", name)
		}
	}
	for _, kind := range []elf.SymType{elf.STT_NOTYPE, elf.STT_OBJECT, elf.STT_GNU_IFUNC} {
		s := valid
		s.Info = byte(elf.STB_GLOBAL)<<4 | byte(kind)
		if snapshotRuntimeImport(s, elf.EM_X86_64) {
			t.Errorf("accepted loader type %v", kind)
		}
	}
	for _, machine := range []elf.Machine{elf.EM_X86_64, elf.EM_AARCH64} {
		for _, library := range []string{"libc.so.6", "libgcc_s.so.1", "libm.so.6", "libpthread.so.0", "libdl.so.2", "librt.so.1"} {
			s := valid
			s.Library = library
			if !snapshotRuntimeLibrary(library, machine) || !snapshotRuntimeImport(s, machine) {
				t.Errorf("changed existing runtime admission: %v %s", machine, library)
			}
		}
	}
}

func TestSnapshotMeasuredLoaderProviderPaths(t *testing.T) {
	const loader = "ld-linux-x86-64.so.2"
	for _, tc := range []struct {
		path, perms string
		address     uintptr
		want        bool
	}{
		{"/usr/lib/x86_64-linux-gnu/" + loader, "r-xp", 0x1800, true},
		{"/lib/x86_64-linux-gnu/" + loader, "r-xp", 0x1800, true},
		{"/lib64/" + loader, "r-xp", 0x1800, true},
		{"/tmp/" + loader, "r-xp", 0x1800, false},
		{"/lib64/x" + loader, "r-xp", 0x1800, false},
		{"/lib/ld-linux-aarch64.so.1", "r-xp", 0x1800, false},
		{"/lib64/" + loader, "r--p", 0x1800, false},
		{"/lib64/" + loader, "r-xp", 0x2000, false},
		{"/lib64/" + loader + " (deleted)", "r-xp", 0x1800, false},
		{"", "r-xp", 0x1800, false},
	} {
		maps := []byte(fmt.Sprintf("1000-2000 %s 00000000 00:01 42 %s\n", tc.perms, tc.path))
		if got := snapshotAddressInRuntime(maps, tc.address, loader); got != tc.want {
			t.Errorf("mapping %q at %x: got %v, want %v", maps, tc.address, got, tc.want)
		}
	}
	if snapshotAddressInRuntime([]byte("malformed mapping"), 0x1800, loader) {
		t.Fatal("accepted malformed mapping")
	}
	// Exercise an actual same-basename DSO without interposing TLS or changing
	// the process interpreter. It is a local test handle with a distinct export.
	dir := t.TempDir()
	source, dso := filepath.Join(dir, "provider.c"), filepath.Join(dir, loader)
	if err := os.WriteFile(source, []byte("int snapshot_provider_fixture(void) { return 42; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cc", "-shared", "-fPIC", "-o", dso, source).CombinedOutput(); err != nil {
		t.Fatalf("compile provider fixture: %v %s", err, out)
	}
	h, err := purego.Dlopen(dso, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(h)
	address, err := purego.Dlsym(h, "snapshot_provider_fixture")
	if err != nil {
		t.Fatal(err)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(maps, []byte(dso)) || snapshotAddressInRuntime(maps, address, loader) {
		t.Fatalf("actual non-system provider mapping missing or accepted: %s", dso)
	}
	for _, line := range strings.Split(string(maps), "\n") {
		if strings.Contains(line, dso) && strings.Contains(line, "r-x") {
			t.Logf("refused actual same-basename provider address=%x mapping=%s", address, line)
		}
	}
}

func TestSnapshotMeasuredActualLoaderProvider(t *testing.T) {
	e, _, err := NewMeasuredPolyglot(snapshotTestFFI(t))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	measured := e.(*snapshotMeasuredEngine)
	f, err := elf.NewFile(measured.sealed)
	if err != nil {
		t.Fatal(err)
	}
	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range snapshotUndefinedSymbols(syms) {
		if s.Library != "ld-linux-x86-64.so.2" {
			continue
		}
		if f.Machine != elf.EM_X86_64 || s.Name != "__tls_get_addr" || s.Version != "GLIBC_2.3" || elf.ST_TYPE(s.Info) != elf.STT_FUNC || elf.ST_BIND(s.Info) != elf.STB_GLOBAL {
			t.Fatalf("unexpected actual loader ABI: %+v", s)
		}
		address, err := purego.Dlsym(measured.handle, s.Name)
		if err != nil || !snapshotAddressInRuntime(maps, address, s.Library) {
			t.Fatalf("actual loader provider: %x %v", address, err)
		}
		for _, line := range strings.Split(string(maps), "\n") {
			if snapshotAddressInRuntime([]byte(line), address, s.Library) {
				t.Logf("actual %v %s %s %s address=%x mapping=%s", f.Machine, s.Name, s.Version, s.Library, address, line)
			}
		}
		count++
	}
	if f.Machine == elf.EM_X86_64 && count != 1 || f.Machine == elf.EM_AARCH64 && count != 0 {
		t.Fatalf("unexpected loader imports: %v count=%d", f.Machine, count)
	}
	if err := verifySnapshotSymbols(measured.sealed, measured.handle); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual machine=%v loader imports=%d; complete symbol verification PASS", f.Machine, count)
}

func TestSnapshotMeasuredLoaderMetadataRefusals(t *testing.T) {
	_, original := snapshotCopyFFI(t)
	f, err := elf.NewFile(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if f.Machine != elf.EM_X86_64 {
		if bytes.Contains(original, []byte("ld-linux-x86-64.so.2\x00")) {
			t.Fatal("foreign loader unexpectedly present in non-AMD64 FFI")
		}
		t.Log("non-AMD64 FFI has no x86 loader; cross-machine/name policy covered by LoaderABI")
		return
	}
	for _, tc := range []struct{ name, old, replacement, refusal string }{
		{"lookalike", "ld-linux-x86-64.so.2", "xd-linux-x86-64.so.2", "unmeasured semantic dependency"},
		{"foreign", "ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1", "unmeasured semantic dependency"},
		{"wrong_symbol", "__tls_get_addr", "semantic_hook", "unmeasured imported symbol"},
		{"wrong_version", "GLIBC_2.3", "GLIBC_9.9", "unmeasured imported symbol"},
		{"wrong_type", "", "", "unmeasured imported symbol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), original...)
			if tc.name == "wrong_type" {
				syms, err := f.DynamicSymbols()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for i, s := range syms {
					if s.Name == "__tls_get_addr" {
						off := f.SectionByType(elf.SHT_DYNSYM).Offset + uint64(i+1)*24 + 4
						b[off] = byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_OBJECT)
						found = true
					}
				}
				if !found {
					t.Fatal("actual TLS import absent")
				}
			} else {
				// Update only the dynamic string table. A longer foreign name uses
				// an existing longer string slot and redirects matching references.
				dynamic := f.SectionByType(elf.SHT_DYNAMIC)
				str := f.Sections[dynamic.Link]
				data := b[str.Offset : str.Offset+str.Size]
				at := bytes.Index(data, []byte(tc.old+"\x00"))
				if at < 0 {
					t.Fatal("mutation target absent")
				}
				if len(tc.replacement) > len(tc.old) {
					// The foreign loader spelling is longer; store it in a long
					// export-name slot, then redirect DT_NEEDED before symbol checks.
					newAt := bytes.Index(data, []byte("polyglot_transpile_with_options\x00"))
					if newAt < 0 {
						t.Fatal("foreign-name fixture slot absent")
					}
					copy(data[newAt:], tc.replacement+"\x00")
					found := false
					for off := dynamic.Offset; off < dynamic.Offset+dynamic.Size; off += 16 {
						if elf.DynTag(binary.LittleEndian.Uint64(b[off:])) == elf.DT_NEEDED && binary.LittleEndian.Uint64(b[off+8:]) == uint64(at) {
							binary.LittleEndian.PutUint64(b[off+8:], uint64(newAt))
							found = true
						}
					}
					if !found {
						t.Fatal("loader dependency reference absent")
					}
				} else {
					copy(data[at:], tc.replacement+"\x00")
				}
			}
			path := filepath.Join(t.TempDir(), "mutated.so")
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir("/proc/self/fd")
			e, _, err := NewMeasuredPolyglot(path)
			if err == nil {
				e.Close()
				t.Fatal("accepted mutated loader metadata")
			}
			if !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("unexpected refusal: %v", err)
			}
			after, _ := os.ReadDir("/proc/self/fd")
			if len(before) != len(after) {
				t.Fatalf("FD leak: %d -> %d", len(before), len(after))
			}
			t.Logf("actual ELF %s refused before loading: %v; FD=%d -> %d", tc.name, err, len(before), len(after))
		})
	}
}
