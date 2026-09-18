//go:build linux

package engine

import (
	"bufio"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// The initial measured lane trusts Linux/glibc and GCC's unwind runtime as OS
// components. SQL/parser dependencies must be inside the sealed image. Search
// paths, audit/filter objects, preloads, NODELETE and GNU-unique symbols are not
// supported. This is local image identity, not attestation against a hostile OS
// or a process with arbitrary memory/loader mutation privileges.
const snapshotSeals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
const snapshotDeepBind = 0x8 // glibc RTLD_DEEPBIND; unsupported libc is refused below.

// NewMeasuredPolyglot opens the source exactly once and loads only the sealed
// object. Both dlopen references and the FD are retained until Close quiesces
// the polyglot client. No expected digest or measurement hook is accepted.
func NewMeasuredPolyglot(path string) (Engine, MeasuredIdentity, error) {
	var id MeasuredIdentity
	if strings.TrimSpace(path) == "" {
		return nil, id, fmt.Errorf("engine: explicit measured FFI path required")
	}
	if err := snapshotLoaderEnvironment(); err != nil {
		return nil, id, err
	}
	exe, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, id, fmt.Errorf("engine: running executable: %w", err)
	}
	defer exe.Close()
	if err = inspectSnapshotELF(exe, false); err != nil {
		return nil, id, err
	}
	id.ExecutableDigest, err = hashSnapshotFile(exe)
	if err != nil {
		return nil, id, err
	}
	id.Platform = runtime.GOOS + "/" + runtime.GOARCH
	source, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, id, fmt.Errorf("engine: open measured FFI source: %w", err)
	}
	sealed, err := sealSnapshotSource(source)
	closeErr := source.Close()
	if err != nil {
		return nil, id, err
	}
	if closeErr != nil {
		sealed.Close()
		return nil, id, closeErr
	}
	id.FFIDigest, err = hashSnapshotFile(sealed)
	if err != nil {
		sealed.Close()
		return nil, id, err
	}
	e, err := loadSnapshotSealed(sealed)
	if err != nil {
		sealed.Close()
		return nil, id, err
	}
	return e, id, nil
}
func snapshotLoaderEnvironment() error {
	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "LD_PROFILE", "LD_DEBUG", "LD_ORIGIN_PATH", "LD_ASSUME_KERNEL", "LD_DYNAMIC_WEAK"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("engine: measured loader does not support %s", name)
		}
	}
	b, err := os.ReadFile("/etc/ld.so.preload")
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("engine: inspect loader preload: %w", err)
	}
	if strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("engine: measured loader does not support system preloads")
	}
	// DEEPBIND is a glibc property; do not silently interpret it on another libc.
	if _, err = purego.Dlsym(purego.RTLD_DEFAULT, "gnu_get_libc_version"); err != nil {
		return fmt.Errorf("engine: measured loader requires glibc: %w", err)
	}
	return nil
}
func hashSnapshotFile(f *os.File) (string, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("engine: measured input is not a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, st.Size()))
	if err != nil {
		return "", err
	}
	if n != st.Size() {
		return "", io.ErrUnexpectedEOF
	}
	return fmt.Sprintf("0x%x", h.Sum(nil)), nil
}

// Takes an already-open source so deterministic tests can replace/delete the
// path before copying, without a global hook or a production digest override.
func sealSnapshotSource(source *os.File) (*os.File, error) {
	st, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("engine: FFI source must be a regular file")
	}
	fd, err := unix.MemfdCreate("snapshot-query-ffi", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("engine: executable memfd unsupported: %w", err)
	}
	f := os.NewFile(uintptr(fd), "snapshot-query-ffi")
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	n, err := io.Copy(f, io.NewSectionReader(source, 0, st.Size()))
	if err != nil {
		return nil, err
	}
	if n != st.Size() {
		return nil, io.ErrUnexpectedEOF
	}
	if _, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, snapshotSeals); err != nil {
		return nil, fmt.Errorf("engine: seal FFI: %w", err)
	}
	seals, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals != snapshotSeals {
		return nil, fmt.Errorf("engine: incomplete FFI seals: %x: %v", seals, err)
	}
	if err = inspectSnapshotELF(f, true); err != nil {
		return nil, err
	}
	ok = true
	return f, nil
}
func snapshotRuntimeLibrary(name string, machine elf.Machine) bool {
	if name == "ld-linux-x86-64.so.2" {
		return machine == elf.EM_X86_64
	}
	switch name {
	case "libc.so.6", "libgcc_s.so.1", "libm.so.6", "libpthread.so.0", "libdl.so.2", "librt.so.1":
		return true
	}
	return false
}
func inspectSnapshotELF(f *os.File, ffi bool) error {
	ef, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("engine: measured ELF: %w", err)
	}
	// Do not Close ef: NewFile shares its reader; the caller owns f.
	machine := elf.EM_NONE
	switch runtime.GOARCH {
	case "arm64":
		machine = elf.EM_AARCH64
	case "amd64":
		machine = elf.EM_X86_64
	default:
		return fmt.Errorf("engine: unsupported measured architecture %s", runtime.GOARCH)
	}
	if ef.Class != elf.ELFCLASS64 || ef.Data != elf.ELFDATA2LSB || ef.Machine != machine || ffi && ef.Type != elf.ET_DYN || !ffi && ef.Type != elf.ET_EXEC && ef.Type != elf.ET_DYN {
		return fmt.Errorf("engine: unsupported measured ELF platform/type")
	}
	if err := snapshotELFLoaderMetadata(ef, ffi); err != nil {
		return err
	}
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH, elf.DT_FILTER, elf.DT_AUXILIARY, elf.DT_AUDIT, elf.DT_DEPAUDIT} {
		vals, err := ef.DynValue(tag)
		if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
			return fmt.Errorf("engine: ELF dynamic metadata: %w", err)
		}
		if len(vals) != 0 {
			return fmt.Errorf("engine: measured ELF forbids %s", tag)
		}
	}
	if ffi {
		names, err := ef.DynString(elf.DT_SONAME)
		if err != nil {
			return err
		}
		if len(names) != 0 {
			return fmt.Errorf("engine: measured FFI forbids SONAME aliases")
		}
	}
	flags, err := ef.DynValue(elf.DT_FLAGS_1)
	if err != nil {
		return err
	}
	for _, v := range flags {
		if v&uint64(elf.DF_1_NODELETE) != 0 {
			return fmt.Errorf("engine: measured ELF forbids NODELETE")
		}
	}
	libs, err := ef.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, lib := range libs {
		if !snapshotRuntimeLibrary(lib, ef.Machine) {
			return fmt.Errorf("engine: unmeasured semantic dependency %q", lib)
		}
	}
	if ffi {
		symbols, err := ef.DynamicSymbols()
		if err != nil {
			return err
		}
		for _, s := range symbols {
			if elf.ST_BIND(s.Info) == elf.SymBind(10) {
				return fmt.Errorf("engine: measured FFI forbids GNU-unique symbols")
			}
			if elf.ST_TYPE(s.Info) == elf.STT_GNU_IFUNC {
				return fmt.Errorf("engine: measured FFI forbids indirect functions")
			}
		}
		for _, s := range snapshotUndefinedSymbols(symbols) {
			// The three optional compiler instrumentation symbols are allowed only
			// when absent from the process; they cannot bind to an external plugin.
			if snapshotOptionalInstrumentation(s) {
				if _, err := purego.Dlsym(purego.RTLD_DEFAULT, s.Name); err == nil {
					return fmt.Errorf("engine: external instrumentation symbol %q", s.Name)
				}
				continue
			}
			if !snapshotRuntimeImport(s, ef.Machine) {
				return fmt.Errorf("engine: unmeasured imported symbol %q (%s/%s)", s.Name, s.Library, s.Version)
			}
		}
	}
	return nil
}

// debug/elf's helpers read section headers; the dynamic loader reads program
// headers. Refuse discrepant views before inspecting dependencies or dlopen.
func snapshotELFLoaderMetadata(f *elf.File, ffi bool) error {
	dynamic := f.SectionByType(elf.SHT_DYNAMIC)
	var program *elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_DYNAMIC {
			if program != nil {
				return fmt.Errorf("engine: multiple dynamic segments")
			}
			program = p
		}
	}
	if program == nil && dynamic == nil && !ffi {
		return nil
	} // static host executable
	if program == nil || dynamic == nil || dynamic.Offset != program.Off || dynamic.Addr != program.Vaddr || dynamic.Size != program.Filesz || dynamic.Size%16 != 0 {
		return fmt.Errorf("engine: inconsistent ELF dynamic segment")
	}
	mapped := func(s *elf.Section) bool {
		for _, p := range f.Progs {
			if p.Type == elf.PT_LOAD && s.Addr >= p.Vaddr && s.Offset >= p.Off && s.Addr-p.Vaddr == s.Offset-p.Off && s.Offset-p.Off <= p.Filesz && s.Size <= p.Filesz-(s.Offset-p.Off) {
				return true
			}
		}
		return false
	}
	if !mapped(dynamic) {
		return fmt.Errorf("engine: unmapped ELF dynamic metadata")
	}
	for _, pair := range []struct {
		tag  elf.DynTag
		kind elf.SectionType
	}{{elf.DT_SYMTAB, elf.SHT_DYNSYM}, {elf.DT_VERSYM, elf.SHT_GNU_VERSYM}, {elf.DT_VERNEED, elf.SHT_GNU_VERNEED}} {
		vals, err := f.DynValue(pair.tag)
		if err != nil {
			return err
		}
		section := f.SectionByType(pair.kind)
		if len(vals) == 0 && section == nil {
			continue
		}
		if len(vals) != 1 || section == nil || section.Addr != vals[0] || !mapped(section) {
			return fmt.Errorf("engine: inconsistent ELF %s metadata", pair.tag)
		}
	}
	vals, err := f.DynValue(elf.DT_STRTAB)
	if err != nil {
		return err
	}
	if int(dynamic.Link) >= len(f.Sections) || len(vals) != 1 {
		return fmt.Errorf("engine: invalid ELF string metadata")
	}
	stringsSection := f.Sections[dynamic.Link]
	sizes, err := f.DynValue(elf.DT_STRSZ)
	if err != nil {
		return err
	}
	if stringsSection.Type != elf.SHT_STRTAB || stringsSection.Addr != vals[0] || len(sizes) != 1 || stringsSection.Size != sizes[0] || !mapped(stringsSection) {
		return fmt.Errorf("engine: inconsistent ELF string metadata")
	}
	syms := f.SectionByType(elf.SHT_DYNSYM)
	if syms == nil || syms.Link != dynamic.Link {
		return fmt.Errorf("engine: inconsistent ELF symbol string table")
	}
	return snapshotELFSymbolExtent(f, syms)
}

// ImportedSymbols intentionally omits WEAK imports. Both loader-policy passes
// must instead use the complete, extent-validated dynamic symbol table.
func snapshotUndefinedSymbols(symbols []elf.Symbol) []elf.Symbol {
	var imports []elf.Symbol
	for _, s := range symbols {
		binding := elf.ST_BIND(s.Info)
		if s.Section == elf.SHN_UNDEF && (binding == elf.STB_GLOBAL || binding == elf.STB_WEAK) {
			imports = append(imports, s)
		}
	}
	return imports
}

func snapshotOptionalInstrumentation(s elf.Symbol) bool {
	if elf.ST_BIND(s.Info) != elf.STB_WEAK || s.Version != "" || s.Library != "" {
		return false
	}
	return s.Name == "__gmon_start__" || s.Name == "_ITM_registerTMCloneTable" || s.Name == "_ITM_deregisterTMCloneTable"
}
func snapshotRuntimeImport(s elf.Symbol, machine elf.Machine) bool {
	// The evidenced AMD64 loader dependency supplies only this TLS ABI. Keep it
	// separate from the common runtime version prefixes and foreign loaders.
	if s.Library == "ld-linux-x86-64.so.2" {
		return machine == elf.EM_X86_64 && s.Name == "__tls_get_addr" && s.Version == "GLIBC_2.3" && elf.ST_TYPE(s.Info) == elf.STT_FUNC
	}
	return snapshotRuntimeLibrary(s.Library, machine) && (strings.HasPrefix(s.Version, "GLIBC_") || strings.HasPrefix(s.Version, "GCC_"))
}

type snapshotMeasuredEngine struct {
	Engine
	sealed   *os.File
	handle   uintptr // separate DEEPBIND reference, in addition to polyglot's handle
	once     sync.Once
	closeErr error
}

func (e *snapshotMeasuredEngine) Close() error {
	e.once.Do(func() {
		// Client.Close holds its write lock until every active FFI call releases its
		// read lock. Only then may either loader reference or the FD be released.
		e.closeErr = errors.Join(e.Engine.Close(), purego.Dlclose(e.handle), e.sealed.Close())
	})
	return e.closeErr
}
func loadSnapshotSealed(f *os.File) (*snapshotMeasuredEngine, error) {
	seals, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals != snapshotSeals {
		return nil, fmt.Errorf("engine: load requires complete seals")
	}
	path := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL|snapshotDeepBind)
	if err != nil {
		return nil, fmt.Errorf("engine: sealed FFI dlopen: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			purego.Dlclose(h)
		}
	}()
	// Verify actual function mappings, not just a loader path/handle. This also
	// refuses cached old /proc/self/fd/N aliases after descriptor reuse.
	if err = verifySnapshotSymbols(f, h); err != nil {
		return nil, err
	}
	e, err := NewPolyglot(path)
	if err != nil {
		return nil, err
	}
	if err = verifySnapshotSymbols(f, h); err != nil {
		e.Close()
		return nil, err
	}
	ok = true
	return &snapshotMeasuredEngine{Engine: e, sealed: f, handle: h}, nil
}
func verifySnapshotSymbols(f *os.File, h uintptr) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return err
	}
	if err := snapshotELFLoaderMetadata(ef, true); err != nil {
		return err
	}
	syms, err := ef.DynamicSymbols()
	if err != nil {
		return err
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return err
	}
	checked := 0
	for _, s := range syms {
		if !strings.HasPrefix(s.Name, "polyglot_") || s.Section == elf.SHN_UNDEF {
			continue
		}
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC {
			return fmt.Errorf("engine: unsupported FFI symbol %s", s.Name)
		}
		addr, err := purego.Dlsym(h, s.Name)
		if err != nil {
			return err
		}
		if !snapshotAddressInFile(maps, addr, &st) {
			return fmt.Errorf("engine: FFI symbol %s maps outside sealed image (loader cache/interposition)", s.Name)
		}
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("engine: no measured FFI entrypoints")
	}
	for _, s := range snapshotUndefinedSymbols(syms) {
		if snapshotOptionalInstrumentation(s) {
			// Recheck absence against both the global namespace and this handle's
			// dependency scope after loading, including all undefined weak imports.
			if _, err := purego.Dlsym(purego.RTLD_DEFAULT, s.Name); err == nil {
				return fmt.Errorf("engine: external instrumentation symbol %q", s.Name)
			}
			if _, err := purego.Dlsym(h, s.Name); err == nil {
				return fmt.Errorf("engine: bound instrumentation symbol %q", s.Name)
			}
			continue
		}
		if !snapshotRuntimeImport(s, ef.Machine) {
			return fmt.Errorf("engine: unmeasured imported symbol %q (%s/%s)", s.Name, s.Library, s.Version)
		}
		addr, err := purego.Dlsym(h, s.Name)
		if err != nil {
			return fmt.Errorf("engine: runtime symbol %s: %w", s.Name, err)
		}
		if !snapshotAddressInRuntime(maps, addr, s.Library) {
			return fmt.Errorf("engine: runtime symbol %s is not in system %s", s.Name, s.Library)
		}
	}
	return nil
}
func snapshotAddressInRuntime(maps []byte, addr uintptr, library string) bool {
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		bounds := strings.Split(fields[0], "-")
		if len(bounds) != 2 {
			continue
		}
		lo, e1 := strconv.ParseUint(bounds[0], 16, 64)
		hi, e2 := strconv.ParseUint(bounds[1], 16, 64)
		if e1 != nil || e2 != nil || uint64(addr) < lo || uint64(addr) >= hi {
			continue
		}
		path := fields[5]
		// The named, loaded OS runtime is trusted under the host OS boundary, not
		// a same-SONAME replacement from a caller-controlled directory or preload.
		return strings.Contains(fields[1], "x") && filepath.Base(path) == library && (strings.HasPrefix(path, "/usr/lib/") || strings.HasPrefix(path, "/lib/") || strings.HasPrefix(path, "/lib64/"))
	}
	return false
}
func snapshotAddressInFile(maps []byte, addr uintptr, st *unix.Stat_t) bool {
	scan := bufio.NewScanner(strings.NewReader(string(maps)))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		bounds := strings.Split(fields[0], "-")
		dev := strings.Split(fields[3], ":")
		if len(bounds) != 2 || len(dev) != 2 {
			continue
		}
		start, e1 := strconv.ParseUint(bounds[0], 16, 64)
		end, e2 := strconv.ParseUint(bounds[1], 16, 64)
		major, e3 := strconv.ParseUint(dev[0], 16, 32)
		minor, e4 := strconv.ParseUint(dev[1], 16, 32)
		inode, e5 := strconv.ParseUint(fields[4], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			continue
		}
		if uint64(addr) >= start && uint64(addr) < end {
			return strings.Contains(fields[1], "x") && inode == st.Ino && unix.Mkdev(uint32(major), uint32(minor)) == uint64(st.Dev)
		}
	}
	return false
}
