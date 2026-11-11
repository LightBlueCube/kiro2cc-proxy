package agybin

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// mcpCallTimeout replaces agy's default 180 s MCP tool-call timeout. The
// relay's own wait (parkTTL) ends a call long before this.
const mcpCallTimeout = 24 * time.Hour

// Function names as recorded in agy's Go function table.
const (
	pkgCortex = "google3/third_party/jetski/cortex/"
	pkgCLI    = "google3/third_party/jetski/cli/"

	fnCallToolRaw          = pkgCortex + "utils/mcp/mcpcore/mcpcore.(*McpClientSessionInstance).callToolRaw"
	fnCallToolTimeoutError = pkgCortex + "utils/mcp/mcpcore/mcpcore.(*McpClientSessionInstance).formatCallToolTimeoutError"
	fnOutputTruncation     = pkgCortex + "core/tooldecorators/tooldecorators.newOutputTruncationDecorator.func1"
	fnOffersContextWindow  = pkgCLI + "model/model.SelectorModel.offersContextWindow"
	fnStoreContextWindow   = pkgCLI + "store/store.(*Store).ContextWindow"
	fnStoreWindowChoice    = pkgCLI + "store/store.(*Store).ContextWindowChoice"
	fnCheckpointConfig     = pkgCortex + "utils/utils.GetCheckpointConfig"
	fnCheckpointOverride   = pkgCortex + "config/config.applyFeatureProviderCheckpointOverrideConfig"
	fnMaxTokensForStep     = pkgCortex + "chatconverters/chatconverters.(*TrajectoryChatConverter).getMaxTokensForStep"
	fnStepToMessage        = "google3/third_party/oneharness/internal/core/serializers/serializers.DefaultSerializer.stepToMessage"
)

// patch is one behaviour change, made at one or more sites.
type patch struct {
	name  string
	sites []site
}

// site is one rewrite: signature must match exactly once inside function,
// and rewrite edits the function body at the match.
type site struct {
	function  string
	signature pattern
	// atEntry requires the match at the function's first byte.
	atEntry bool
	rewrite func(im *image, fn function, at int) error
}

// The context patches port realagy's context-unlock gates (realagy
// src/acp/context-unlock.ts) to signatures of the pinned build; the gate
// numbers are realagy's.
var patches = []patch{
	{
		// mov{rcx,rdx}, 180e9 is the default timeout in nanoseconds; the call
		// and its error message must agree.
		name: "mcp-call-timeout",
		sites: []site{
			{function: fnCallToolRaw, signature: mustPattern("48 b9 00 08 d6 e8 29 00 00 00"), rewrite: setImm64(mcpCallTimeout.Nanoseconds())},
			{function: fnCallToolTimeoutError, signature: mustPattern("48 ba 00 08 d6 e8 29 00 00 00"), rewrite: setImm64(mcpCallTimeout.Nanoseconds())},
		},
	},
	{
		// test rdx,rdx; jle return_original_tool; mov [rsp+38],rdx; mov eax,28.
		// The JLE becomes a JMP to the same target, so tools are never
		// wrapped in the truncating decorator.
		name: "full-tool-output",
		sites: []site{
			{function: fnOutputTruncation, signature: mustPattern("48 85 d2 0f 8e ?? ?? ?? ?? 48 89 54 24 38 b8 28 00 00 00"), rewrite: jccToJmp(3)},
		},
	},
	{
		// Gate 1: every model offers a selectable context window.
		name: "context-gate-1",
		sites: []site{
			{function: fnOffersContextWindow, signature: mustPattern("49 89 e4 49 81 ec ?? ?? ?? ?? 0f 82"), atEntry: true, rewrite: write(0, mustBytes("b8 01 00 00 00 c3"))},
		},
	},
	{
		// Gate 2: Store.ContextWindow returns the stored choice without the
		// model whitelist.
		name: "context-gate-2",
		sites: []site{
			{function: fnStoreContextWindow, signature: mustPattern("4c 8d 64 24 ?? 4d 3b 66 10 0f 86"), atEntry: true, rewrite: jmpTo(fnStoreWindowChoice)},
		},
	},
	{
		// Gate 3A: cmp edx,256000; mov esi,256000; cmovb esi,edx becomes
		// mov esi,edx, removing the 256K checkpoint clamp.
		name: "context-gate-3a",
		sites: []site{
			{function: fnCheckpointConfig, signature: mustPattern("81 fa 00 e8 03 00 be 00 e8 03 00 0f 42 f2"), rewrite: write(0, mustBytes("89 d6 90 90 90 90 90 90 90 90 90 90 90 90"))},
		},
	},
	{
		// Gate 3B: call rdx (HasFlag); test al,al; jne apply_override. The
		// JNE becomes NOPs, so experiments never override checkpointing.
		name: "context-gate-3b",
		sites: []site{
			{function: fnCheckpointOverride, signature: mustPattern("ff d2 84 c0 75 ??"), rewrite: write(4, mustBytes("90 90"))},
		},
	},
	{
		// Gate 4A: step types 8 and 0x13 (USER_INPUT per realagy) get a token
		// limit of 0, meaning none: xor eax,eax; jmp to the leave; ret.
		name: "context-gate-4a",
		sites: []site{
			{function: fnMaxTokensForStep, signature: mustPattern("83 f8 08 74 05 83 f8 13 75 ??"), rewrite: zeroReturn(10, mustPattern("48 89 c8 c9 c3"), 3)},
		},
	},
	{
		// Gate 4B: step type 0xe (USER_INPUT) loads its token limit into rdi;
		// the load becomes xor edi,edi, as type 0x17 already does.
		name: "context-gate-4b",
		sites: []site{
			{function: fnStepToMessage, signature: mustPattern("83 fa 0e 75 0a 48 8b bc 24 ?? ?? ?? ?? eb 11 83 fa 17 75 04 31 ff eb 08"), rewrite: write(5, mustBytes("31 ff 90 90 90 90 90 90"))},
		},
	},
	{
		// Every other step type loads the serializer's per-step token limit
		// into rdi, then test rdi,rdi; jg truncate. A 60 KB tool result
		// reached the model cut to its first ~49 KB plus "<truncated N
		// bytes>". The load becomes xor edi,edi.
		name: "full-step-output",
		sites: []site{
			{function: fnStepToMessage, signature: mustPattern("31 ff eb 08 48 8b bc 24 ?? ?? ?? ?? 48 85 ff 7f"), rewrite: write(4, mustBytes("31 ff 90 90 90 90 90 90"))},
		},
	},
}

// pattern is a byte signature; wild marks bytes that match anything.
type pattern struct {
	bytes []byte
	wild  []bool
}

// mustPattern parses hex bytes separated by spaces, "??" for a wildcard.
func mustPattern(spec string) pattern {
	var p pattern
	for _, field := range strings.Fields(spec) {
		if field == "??" {
			p.bytes = append(p.bytes, 0)
			p.wild = append(p.wild, true)
			continue
		}
		b, err := hex.DecodeString(field)
		if err != nil || len(b) != 1 {
			panic(fmt.Sprintf("agybin: bad pattern byte %q", field))
		}
		p.bytes = append(p.bytes, b[0])
		p.wild = append(p.wild, false)
	}
	return p
}

func mustBytes(spec string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(spec, " ", ""))
	if err != nil {
		panic(fmt.Sprintf("agybin: bad bytes %q", spec))
	}
	return b
}

// find returns the offsets of every match in body.
func (p pattern) find(body []byte) []int {
	var matches []int
	for at := 0; at+len(p.bytes) <= len(body); at++ {
		if p.matchAt(body, at) {
			matches = append(matches, at)
		}
	}
	return matches
}

func (p pattern) matchAt(body []byte, at int) bool {
	for i, b := range p.bytes {
		if !p.wild[i] && body[at+i] != b {
			return false
		}
	}
	return true
}

// write copies b over the match, starting delta bytes into it.
func write(delta int, b []byte) func(*image, function, int) error {
	return func(_ *image, fn function, at int) error {
		copy(fn.body[at+delta:], b)
		return nil
	}
}

// setImm64 replaces the 64-bit immediate of a REX.W MOV r64, imm64.
func setImm64(value int64) func(*image, function, int) error {
	return func(_ *image, fn function, at int) error {
		binary.LittleEndian.PutUint64(fn.body[at+2:], uint64(value))
		return nil
	}
}

// jccToJmp turns the 6-byte Jcc rel32 delta bytes into the match into a
// 5-byte JMP rel32 to the same target, plus a NOP.
func jccToJmp(delta int) func(*image, function, int) error {
	return func(_ *image, fn function, at int) error {
		from := at + delta
		target := from + 6 + int(int32(binary.LittleEndian.Uint32(fn.body[from+2:])))
		if target < 0 || target >= len(fn.body) {
			return fmt.Errorf("branch target %+d is outside the function", target)
		}
		fn.body[from] = 0xe9
		binary.LittleEndian.PutUint32(fn.body[from+1:], uint32(int32(target-(from+5))))
		fn.body[from+5] = 0x90
		return nil
	}
}

// jmpTo replaces the function's entry with a JMP rel32 to another
// function with the same signature.
func jmpTo(name string) func(*image, function, int) error {
	return func(im *image, fn function, _ int) error {
		target, err := im.function(name)
		if err != nil {
			return fmt.Errorf("jump target %s: %w", name, err)
		}
		rel := int64(target.addr) - int64(fn.addr+5)
		if rel != int64(int32(rel)) {
			return fmt.Errorf("jump to %s is out of rel32 range", name)
		}
		fn.body[0] = 0xe9
		binary.LittleEndian.PutUint32(fn.body[1:], uint32(int32(rel)))
		return nil
	}
}

// zeroReturn writes xor eax,eax; jmp rel32 delta bytes into the match. The
// jump lands targetDelta bytes into the function's only match of epilogue.
func zeroReturn(delta int, epilogue pattern, targetDelta int) func(*image, function, int) error {
	return func(_ *image, fn function, at int) error {
		ends := epilogue.find(fn.body)
		if len(ends) != 1 {
			return fmt.Errorf("found %d epilogues, want 1", len(ends))
		}
		from := at + delta
		target := ends[0] + targetDelta
		fn.body[from], fn.body[from+1] = 0x31, 0xc0
		fn.body[from+2] = 0xe9
		binary.LittleEndian.PutUint32(fn.body[from+3:], uint32(int32(target-(from+7))))
		return nil
	}
}
