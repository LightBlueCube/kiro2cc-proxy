package agybin

import (
	"bytes"
	"debug/elf"
	"debug/gosym"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Go 1.20+ pclntab header layout (runtime/symtab.go pcHeader):
// magic (0xfffffff1), two pad bytes, minLC=1, ptrSize=8, followed by
// uint64 fields nfunc, nfiles, textStart, funcnameOffset, ..., pclnOffset.
var pclntabMagic = []byte{0xf1, 0xff, 0xff, 0xff, 0x00, 0x00, 0x01, 0x08}

const (
	pclntabHeaderSize     = 72
	pclntabOffNfunc       = 8
	pclntabOffTextStart   = 24
	pclntabOffFuncname    = 32
	pclntabOffPcln        = 64
	pclntabMinFuncs       = 100
	pclntabMaxFuncs       = 10_000_000
	pclntabNameProbeBytes = 16

	// runtime.moduledata field offsets for text and etext (Go 1.20–1.28, amd64).
	// Used only as a last-resort fallback for stripped static builds (e.g. musl).
	moduledataOffText  = 0xb0
	moduledataOffEtext = 0xb8
	moduledataMinSize  = 0xc0
	relaEntryMinSize   = 24
	relaScanWindow     = 32

	// textAnchor is exported in the dynamic symbol table of cgo builds.
	textAnchor     = "crosscall2"
	symRuntimeText = "runtime.text"
)

// image is a linux/amd64 Go executable held in memory. Function bodies are
// views into data, so rewriting a body rewrites the image.
type image struct {
	data      []byte
	file      *elf.File
	table     *gosym.Table
	textStart uint64
}

// function is one Go function of the image.
type function struct {
	name string
	addr uint64
	body []byte
}

func loadImage(data []byte) (*image, error) {
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not an ELF executable: %w", err)
	}
	if file.Class != elf.ELFCLASS64 || file.Machine != elf.EM_X86_64 {
		return nil, fmt.Errorf("unsupported executable: %v %v, want ELFCLASS64 EM_X86_64", file.Class, file.Machine)
	}

	pclntabOff, err := findPclntab(file, data)
	if err != nil {
		return nil, err
	}
	table, err := gosym.NewTable(nil, gosym.NewLineTable(data[pclntabOff:], 0))
	if err != nil {
		return nil, fmt.Errorf("go function table: %w", err)
	}

	im := &image{data: data, file: file, table: table}
	if im.textStart, err = im.calibrate(pclntabOff); err != nil {
		return nil, err
	}
	return im, nil
}

// findPclntab returns the file offset of the main program's Go function table.
// agy can embed another Go program in data, so only validated tables in
// read-only segments are considered, and there must be exactly one.
func findPclntab(file *elf.File, data []byte) (int, error) {
	var found []int
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_LOAD || prog.Flags&elf.PF_W != 0 {
			continue
		}
		segment := data[prog.Off : prog.Off+prog.Filesz]
		for from := 0; ; {
			at := bytes.Index(segment[from:], pclntabMagic)
			if at < 0 {
				break
			}
			off := int(prog.Off) + from + at
			from += at + 1
			if validPclntab(data, off) {
				found = append(found, off)
			}
		}
	}
	if len(found) != 1 {
		return 0, fmt.Errorf("found %d Go function tables in read-only segments, want 1", len(found))
	}
	return found[0], nil
}

func validPclntab(data []byte, off int) bool {
	if off < 0 || off+pclntabHeaderSize > len(data) {
		return false
	}
	avail := uint64(len(data) - off)
	nfunc := binary.LittleEndian.Uint64(data[off+pclntabOffNfunc:])
	if nfunc < pclntabMinFuncs || nfunc > pclntabMaxFuncs {
		return false
	}
	namesOff := binary.LittleEndian.Uint64(data[off+pclntabOffFuncname:])
	pclnOff := binary.LittleEndian.Uint64(data[off+pclntabOffPcln:])
	if namesOff >= avail || avail-namesOff < pclntabNameProbeBytes || pclnOff >= avail {
		return false
	}
	for _, b := range data[off+int(namesOff) : off+int(namesOff)+pclntabNameProbeBytes] {
		if b != 0 && (b < 0x20 || b >= 0x7f) {
			return false
		}
	}
	return true
}

// calibrate resolves the virtual base address that pclntab function entries
// are relative to. agy's pclntab header leaves textStart == 0.
func (im *image) calibrate(pclntabOff int) (uint64, error) {
	if ts := binary.LittleEndian.Uint64(im.data[pclntabOff+pclntabOffTextStart:]); ts != 0 && im.inExecutable(ts) {
		return ts, nil
	}
	if ts, ok := im.calibrateFromDynamicAnchor(); ok {
		return ts, nil
	}
	if ts, ok := im.calibrateFromSymtab(); ok {
		return ts, nil
	}
	if ts, ok := im.calibrateFromModuledata(pclntabOff); ok {
		return ts, nil
	}
	return 0, errors.New("cannot calibrate Go text base (no pclntab textStart, dynamic anchor, symtab, or moduledata)")
}

func (im *image) calibrateFromDynamicAnchor() (uint64, bool) {
	symbols, err := im.file.DynamicSymbols()
	if err != nil {
		return 0, false
	}
	fn := im.table.LookupFunc(textAnchor)
	if fn == nil {
		return 0, false
	}
	for _, sym := range symbols {
		if sym.Name == textAnchor && elf.ST_TYPE(sym.Info) == elf.STT_FUNC && sym.Value >= fn.Entry {
			return sym.Value - fn.Entry, true
		}
	}
	return 0, false
}

func (im *image) calibrateFromSymtab() (uint64, bool) {
	symbols, err := im.file.Symbols()
	if err != nil {
		return 0, false
	}
	for _, sym := range symbols {
		if sym.Name == symRuntimeText && im.inExecutable(sym.Value) {
			return sym.Value, true
		}
	}
	return 0, false
}

// calibrateFromModuledata locates runtime.firstmoduledata by its leading
// (pcHeader, funcnametab) pointer pair, either via .rela.dyn (PIE) or direct
// pointers in data (static/musl builds).
func (im *image) calibrateFromModuledata(pclntabOff int) (uint64, bool) {
	textSec := im.file.Section(".text")
	if textSec == nil {
		return 0, false
	}
	pclnVA, ok := im.fileToVA(uint64(pclntabOff))
	if !ok {
		return 0, false
	}
	funcnameVA := pclnVA + binary.LittleEndian.Uint64(im.data[pclntabOff+pclntabOffFuncname:])
	textLo, textHi := textSec.Addr, textSec.Addr+textSec.Size

	if ts, ok := im.moduledataFromRela(pclnVA, funcnameVA, textLo, textHi); ok {
		return ts, true
	}
	return im.moduledataFromData(pclnVA, funcnameVA, textLo, textHi)
}

func (im *image) moduledataFromRela(pclnVA, funcnameVA, textLo, textHi uint64) (uint64, bool) {
	for _, sec := range im.file.Sections {
		if sec.Type != elf.SHT_RELA {
			continue
		}
		rela, err := sec.Data()
		if err != nil {
			continue
		}
		entsize := max(int(sec.Entsize), relaEntryMinSize)
		for pos := 0; pos+2*entsize <= len(rela); pos += entsize {
			if binary.LittleEndian.Uint64(rela[pos+16:]) != pclnVA {
				continue
			}
			modVA := binary.LittleEndian.Uint64(rela[pos:])
			if binary.LittleEndian.Uint64(rela[pos+entsize:]) != modVA+8 ||
				binary.LittleEndian.Uint64(rela[pos+entsize+16:]) != funcnameVA {
				continue
			}
			mText, mEtext := moduledataBoundsFromRela(rela, pos, entsize, modVA)
			if textLo <= mText && mText < mEtext && mEtext <= textHi {
				return mText, true
			}
		}
	}
	return 0, false
}

func moduledataBoundsFromRela(rela []byte, pos, entsize int, modVA uint64) (text, etext uint64) {
	for k := 2; k < relaScanWindow && pos+(k+1)*entsize <= len(rela); k++ {
		target := binary.LittleEndian.Uint64(rela[pos+k*entsize:])
		value := binary.LittleEndian.Uint64(rela[pos+k*entsize+16:])
		switch target {
		case modVA + moduledataOffText:
			text = value
		case modVA + moduledataOffEtext:
			etext = value
		}
	}
	return text, etext
}

func (im *image) moduledataFromData(pclnVA, funcnameVA, textLo, textHi uint64) (uint64, bool) {
	var pair [16]byte
	binary.LittleEndian.PutUint64(pair[0:8], pclnVA)
	binary.LittleEndian.PutUint64(pair[8:16], funcnameVA)

	for from := 0; ; {
		at := bytes.Index(im.data[from:], pair[:])
		if at < 0 {
			return 0, false
		}
		hit := from + at
		from = hit + 1
		if hit+moduledataMinSize > len(im.data) {
			continue
		}
		mText := binary.LittleEndian.Uint64(im.data[hit+moduledataOffText:])
		mEtext := binary.LittleEndian.Uint64(im.data[hit+moduledataOffEtext:])
		if textLo <= mText && mText < mEtext && mEtext <= textHi {
			return mText, true
		}
	}
}

func (im *image) inExecutable(va uint64) bool {
	for _, prog := range im.file.Progs {
		if prog.Type == elf.PT_LOAD && prog.Flags&elf.PF_X != 0 &&
			va >= prog.Vaddr && va < prog.Vaddr+prog.Filesz {
			return true
		}
	}
	return false
}

func (im *image) fileToVA(off uint64) (uint64, bool) {
	for _, prog := range im.file.Progs {
		if prog.Type == elf.PT_LOAD && off >= prog.Off && off < prog.Off+prog.Filesz {
			return prog.Vaddr + (off - prog.Off), true
		}
	}
	return 0, false
}

var errNoFunction = errors.New("function not found")

// function resolves a Go function by one or more trailing package-qualified
// short names (e.g. "model.SelectorModel.offersContextWindow"). Matching by
// short name survives internal package path reorganizations while failing
// closed unless all candidate names together match a single symbol.
func (im *image) function(shortNames ...string) (function, error) {
	var matched []*gosym.Func
	for i := range im.table.Funcs {
		if slices.Contains(shortNames, shortSymbolName(im.table.Funcs[i].Name)) {
			matched = append(matched, &im.table.Funcs[i])
		}
	}
	switch len(matched) {
	case 0:
		return function{}, errNoFunction
	case 1:
		return im.sliceFunction(matched[0])
	default:
		names := make([]string, len(matched))
		for i, fn := range matched {
			names[i] = fn.Name
		}
		return function{}, fmt.Errorf("ambiguous function %s: %s", strings.Join(shortNames, "|"), strings.Join(names, ", "))
	}
}

func (im *image) sliceFunction(fn *gosym.Func) (function, error) {
	addr, end := im.textStart+fn.Entry, im.textStart+fn.End
	if end < addr {
		return function{}, fmt.Errorf("invalid function bounds %#x-%#x", addr, end)
	}
	for _, prog := range im.file.Progs {
		inside := prog.Type == elf.PT_LOAD && prog.Flags&elf.PF_X != 0 &&
			addr >= prog.Vaddr && end <= prog.Vaddr+prog.Filesz
		if !inside {
			continue
		}
		off := prog.Off + addr - prog.Vaddr
		return function{name: fn.Name, addr: addr, body: im.data[off : off+end-addr]}, nil
	}
	return function{}, fmt.Errorf("function %#x-%#x is outside executable segments", addr, end)
}

// shortSymbolName strips the leading import path up to the last slash outside
// generic type parameters.
func shortSymbolName(full string) string {
	prefix := full
	if i := strings.IndexByte(prefix, '['); i >= 0 {
		prefix = prefix[:i]
	}
	if slash := strings.LastIndexByte(prefix, '/'); slash >= 0 {
		return full[slash+1:]
	}
	return full
}

// callsTo returns the offsets inside fn.body of every direct call rel32 (0xe8)
// whose resolved target equals targetShortName's entry address.
func (im *image) callsTo(fn function, targetShortName string) ([]int, error) {
	target, err := im.function(targetShortName)
	if err != nil {
		return nil, fmt.Errorf("call target %s: %w", targetShortName, err)
	}
	var hits []int
	for at := 0; at+5 <= len(fn.body); at++ {
		if fn.body[at] != opCallRel32 {
			continue
		}
		rel := int64(int32(binary.LittleEndian.Uint32(fn.body[at+1:])))
		if int64(fn.addr)+int64(at)+5+rel == int64(target.addr) {
			hits = append(hits, at)
		}
	}
	return hits, nil
}
