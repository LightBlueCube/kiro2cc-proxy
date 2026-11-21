package agybin

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	// agy's default MCP tool-call timeout (180 s) is replaced with 24 h;
	// the relay's own wait (ParkTTL) ends a call long before this.
	defaultMCPCallTimeout = 180 * time.Second
	patchedMCPCallTimeout = 24 * time.Hour

	// 256K token clamp in utils.GetCheckpointConfig.
	checkpointClampLimit uint32 = 256_000

	// Protobuf step-type enum values for USER_INPUT / CHECKPOINT.
	cortexStepUserInput      int8 = 0x08
	cortexStepUserInputAlt   int8 = 0x13
	oneHarnessStepUserInput  int8 = 0x0e
	oneHarnessStepCheckpoint int8 = 0x17

	overrideScanWindow   = 96
	epilogueScanWindow   = 64
	truncRetScanWindow   = 32
	selectPreScanWindow  = 32
	maxStepLoadSlotBytes = 32
	zeroReturnPatchSize  = 7 // xor eax, eax (2) + jmp rel32 (5)
)

// Trailing package-qualified Go symbol names resolved uniquely via pclntab.
const (
	fnCallToolRaw            = "mcpcore.(*McpClientSessionInstance).callToolRaw"
	fnCallToolTimeoutError   = "mcpcore.(*McpClientSessionInstance).formatCallToolTimeoutError"
	fnOutputTruncation       = "tooldecorators.newOutputTruncationDecorator.func1"
	fnOffersContextWindow    = "model.SelectorModel.offersContextWindow"
	fnStoreContextWindow     = "store.(*Store).ContextWindow"
	fnStoreWindowChoice      = "store.(*Store).ContextWindowChoice"
	fnCheckpointConfig       = "utils.GetCheckpointConfig"
	fnCheckpointOverride     = "config.applyFeatureProviderCheckpointOverrideConfig"
	fnSelectCheckpointExpKey = "config.selectCheckpointExperimentKey"
	fnMaxTokensForStep       = "chatconverters.(*TrajectoryChatConverter).getMaxTokensForStep"
	fnStepToMessage          = "serializers.DefaultSerializer.stepToMessage"
	fnFileWatcherListen      = "file_watcher.(*FileWatcherDefault).listenAndForwardEvents"
	fnRuntimeSelectgo        = "runtime.selectgo"
)

var (
	retTrueX64        = []byte{0xb8, 0x01, 0x00, 0x00, 0x00, opRet} // mov eax, 1; ret
	testBLBLNop       = []byte{0x84, 0xdb, opNOP}                   // test bl, bl; nop
	testALAL          = []byte{0x84, 0xc0}                          // test al, al
	xorEDIEDI         = []byte{0x31, 0xff}                          // xor edi, edi
	movEDI0           = []byte{0xbf, 0x00, 0x00, 0x00, 0x00}        // mov edi, 0
	movESI2           = []byte{0xbe, 0x02, 0x00, 0x00, 0x00}        // mov esi, 2
	movR8D1           = []byte{0x41, 0xb8, 0x01, 0x00, 0x00, 0x00}  // mov r8d, 1
	xorEAXEAX         = []byte{0x31, 0xc0}                          // xor eax, eax
	regRAX      uint8 = 0
)

// patch is one behaviour change, made at one or more sites.
type patch struct {
	name  string
	sites []site
}

// site locates candidate rewrites inside one named Go function.
// locate must not mutate fn.body; applySite runs the rewrite only when
// len(rewrites) == 1.
type site struct {
	function string
	locate   func(im *image, fn function) ([]func() error, error)
}

// Binary patches applied to agy using structural amd64 locators instead
// of register- or prologue-bound byte strings.
var patches = []patch{
	{
		// Replace the 180 s default MCP timeout immediate in both the call
		// and its error message so they agree.
		name: "mcp-call-timeout",
		sites: []site{
			{function: fnCallToolRaw, locate: locateImm64(defaultMCPCallTimeout.Nanoseconds(), patchedMCPCallTimeout.Nanoseconds())},
			{function: fnCallToolTimeoutError, locate: locateImm64(defaultMCPCallTimeout.Nanoseconds(), patchedMCPCallTimeout.Nanoseconds())},
		},
	},
	{
		// Turn `test r64, r64; jle return_original_tool` into an unconditional
		// JMP to the same return path, so tools are never wrapped in the
		// truncating decorator.
		name: "full-tool-output",
		sites: []site{
			{function: fnOutputTruncation, locate: locateOutputTruncation},
		},
	},
	{
		// Gate 1: every model offers a selectable context window.
		name: "context-gate-1",
		sites: []site{
			{function: fnOffersContextWindow, locate: locateReturnTrue},
		},
	},
	{
		// Gate 2: Store.ContextWindow returns the stored choice without the
		// model whitelist.
		name: "context-gate-2",
		sites: []site{
			{function: fnStoreContextWindow, locate: locateTailJump(fnStoreWindowChoice)},
		},
	},
	{
		// Gate 3A: remove the 256K checkpoint clamp (`cmp/mov/cmovb` -> `mov`).
		name: "context-gate-3a",
		sites: []site{
			{function: fnCheckpointConfig, locate: locateClamp256K},
		},
	},
	{
		// Gate 3B: after selectCheckpointExperimentKey, NOP the JNE on
		// HasFlag so experiments never override checkpointing.
		name: "context-gate-3b",
		sites: []site{
			{function: fnCheckpointOverride, locate: locateCheckpointOverride},
		},
	},
	{
		// Gate 4A: step types 0x08 and 0x13 (USER_INPUT) get a token limit of
		// 0 (none): xor eax,eax; jmp to the function epilogue.
		name: "context-gate-4a",
		sites: []site{
			{function: fnMaxTokensForStep, locate: locateUserInputMaxTokens},
		},
	},
	{
		// Gate 4B: step type 0x0e (USER_INPUT) uses token limit 0, matching
		// step type 0x17 (CHECKPOINT).
		name: "context-gate-4b",
		sites: []site{
			{function: fnStepToMessage, locate: locateStepUserInputLimit},
		},
	},
	{
		// Every other step type in stepToMessage also gets token limit 0 so
		// large tool results are not truncated with "<truncated N bytes>".
		name: "full-step-output",
		sites: []site{
			{function: fnStepToMessage, locate: locateStepDefaultLimit},
		},
	},
	{
		// runtime.selectgo returns (chosen in rax, recvOK in bl). Checking
		// rax == 0 alone ignores a closed watcher.Events channel, spinning
		// between watcher.Close() and close(done). Neither done nor a closed
		// Events yields recvOK == 1, so testing bl exits on either close.
		name: "file-watcher-closed-channel",
		sites: []site{
			{function: fnFileWatcherListen, locate: locateFileWatcherSelect},
		},
	},
}

func locateImm64(want, replace int64) func(*image, function) ([]func() error, error) {
	return func(_ *image, fn function) ([]func() error, error) {
		var hits []func() error
		for at := 0; at+10 <= len(fn.body); at++ {
			imm, _, ok := decodeMovAbsImm64(fn.body, at)
			if !ok || imm != uint64(want) {
				continue
			}
			off := at
			hits = append(hits, func() error {
				binary.LittleEndian.PutUint64(fn.body[off+2:], uint64(replace))
				return nil
			})
		}
		return hits, nil
	}
}

func locateOutputTruncation(_ *image, fn function) ([]func() error, error) {
	var hits []func() error
	for at := 0; at < len(fn.body); at++ {
		_, testSize, ok := decodeTestRR64(fn.body, at)
		if !ok {
			continue
		}
		jle, ok := decodeJcc(fn.body, at+testSize, ccLE)
		if !ok || jle.target < 0 || jle.target >= len(fn.body) {
			continue
		}
		if _, ok := findEpilogue(fn.body, jle.target, truncRetScanWindow); !ok {
			continue
		}
		b := jle
		hits = append(hits, func() error {
			return rewriteJccToJmp(fn.body, b)
		})
	}
	return hits, nil
}

func locateReturnTrue(_ *image, fn function) ([]func() error, error) {
	if len(fn.body) < len(retTrueX64) || bytes.IndexByte(fn.body, opRet) < 0 {
		return nil, nil
	}
	return []func() error{func() error {
		copy(fn.body, retTrueX64)
		return nil
	}}, nil
}

func locateTailJump(targetName string) func(*image, function) ([]func() error, error) {
	return func(im *image, fn function) ([]func() error, error) {
		target, err := im.function(targetName)
		if err != nil {
			return nil, fmt.Errorf("jump target %s: %w", targetName, err)
		}
		if len(fn.body) < 5 {
			return nil, nil
		}
		rel := int64(target.addr) - int64(fn.addr+5)
		return []func() error{func() error {
			if err := encodeJmpRel32(fn.body[:5], rel); err != nil {
				return fmt.Errorf("jump to %s: %w", targetName, err)
			}
			return nil
		}}, nil
	}
}

func locateClamp256K(_ *image, fn function) ([]func() error, error) {
	var hits []func() error
	for at := 0; at < len(fn.body); at++ {
		dst, src, totalLen, ok := matchClamp256KAt(fn.body, at)
		if !ok {
			continue
		}
		off, length, d, s := at, totalLen, dst, src
		hits = append(hits, func() error {
			mov := encodeMovR32(d, s)
			copy(fn.body[off:], mov)
			nopFill(fn.body[off+len(mov) : off+length])
			return nil
		})
	}
	return hits, nil
}

// matchClamp256KAt matches either `cmp src, 256K; mov dst, 256K; cmov{b,l} dst, src`
// or `mov dst, 256K; cmp src, 256K; cmov{b,l} dst, src`.
func matchClamp256KAt(body []byte, at int) (dst, src uint8, totalLen int, ok bool) {
	if cReg, cImm, cSize, ok := decodeCmpR32Imm32(body, at); ok && cImm == checkpointClampLimit {
		if mReg, mImm, mSize, ok := decodeMovR32Imm32(body, at+cSize); ok && mImm == checkpointClampLimit {
			if cc, cdst, csrc, cmSize, ok := decodeCmovR32(body, at+cSize+mSize); ok &&
				(cc == ccB || cc == ccL) && cdst == mReg && csrc == cReg {
				return mReg, cReg, cSize + mSize + cmSize, true
			}
		}
	}
	if mReg, mImm, mSize, ok := decodeMovR32Imm32(body, at); ok && mImm == checkpointClampLimit {
		if cReg, cImm, cSize, ok := decodeCmpR32Imm32(body, at+mSize); ok && cImm == checkpointClampLimit {
			if cc, cdst, csrc, cmSize, ok := decodeCmovR32(body, at+mSize+cSize); ok &&
				(cc == ccB || cc == ccL) && cdst == mReg && csrc == cReg {
				return mReg, cReg, mSize + cSize + cmSize, true
			}
		}
	}
	return 0, 0, 0, false
}

func locateCheckpointOverride(im *image, fn function) ([]func() error, error) {
	selectCalls, err := im.callsTo(fn, fnSelectCheckpointExpKey)
	if err != nil {
		return nil, err
	}
	var hits []func() error
	for _, callAt := range selectCalls {
		end := min(len(fn.body), callAt+5+overrideScanWindow)
		for pos := callAt + 5; pos < end; pos++ {
			callSize, ok := decodeIndirectCall(fn.body, pos)
			if !ok {
				continue
			}
			testAt := pos + callSize
			if testAt+len(testALAL) > end || !bytes.Equal(fn.body[testAt:testAt+len(testALAL)], testALAL) {
				continue
			}
			jne, ok := decodeJcc(fn.body, testAt+len(testALAL), ccNE)
			if !ok || jne.target < 0 || jne.target >= len(fn.body) {
				continue
			}
			b := jne
			hits = append(hits, func() error {
				nopFill(fn.body[b.at : b.at+b.size])
				return nil
			})
			break
		}
	}
	return hits, nil
}

func locateUserInputMaxTokens(_ *image, fn function) ([]func() error, error) {
	var hits []func() error
	for at := 0; at < len(fn.body); at++ {
		reg1, imm1, sz1, ok := decodeCmpR32Imm8(fn.body, at)
		if !ok || (imm1 != cortexStepUserInput && imm1 != cortexStepUserInputAlt) {
			continue
		}
		je, ok := decodeJcc(fn.body, at+sz1, ccE)
		if !ok {
			continue
		}
		next := skipAlignNOPs(fn.body, je.at+je.size)
		reg2, imm2, sz2, ok := decodeCmpR32Imm8(fn.body, next)
		want2 := cortexStepUserInputAlt
		if imm1 == cortexStepUserInputAlt {
			want2 = cortexStepUserInput
		}
		if !ok || reg2 != reg1 || imm2 != want2 {
			continue
		}
		jne, ok := decodeJcc(fn.body, next+sz2, ccNE)
		if !ok {
			continue
		}
		userStart, nonUser := jne.at+jne.size, jne.target
		if je.target != userStart || nonUser < userStart+zeroReturnPatchSize || nonUser >= len(fn.body) {
			continue
		}
		epilogue, ok := findEpilogue(fn.body, nonUser, epilogueScanWindow)
		if !ok {
			continue
		}
		from, target := userStart, epilogue
		hits = append(hits, func() error {
			copy(fn.body[from:], xorEAXEAX)
			return encodeJmpRel32(fn.body[from+2:from+zeroReturnPatchSize], int64(target-(from+zeroReturnPatchSize)))
		})
	}
	return hits, nil
}

// stepLimitDispatch describes the step-type switch in DefaultSerializer.stepToMessage:
//
//	switch step.Type {
//	case 0x0e (USER_INPUT):  limit = maxUserInputTokens
//	case 0x17 (CHECKPOINT):  limit = 0
//	default:                 limit = maxStepTokens
//	}
//	if limit > 0 { truncate(...) }
type stepLimitDispatch struct {
	// redirectUser is true in je-layout (where case 0x0e jumps forward);
	// otherwise [userStart, userEnd) is the inline load slot before jmp merge.
	redirectUser bool
	userBranch   branch
	userStart    int
	userEnd      int

	// [defaultStart, merge) is the inline load slot for other step types
	// (-1 when not identified).
	defaultStart int
	merge        int

	zeroAt    int
	zeroBytes []byte
}

func findStepLimitDispatches(body []byte) []stepLimitDispatch {
	var hits []stepLimitDispatch
	for at := 0; at < len(body); at++ {
		if d, ok := matchStepLimitAt(body, at); ok {
			hits = append(hits, d)
		}
	}
	return hits
}

func matchStepLimitAt(body []byte, at int) (stepLimitDispatch, bool) {
	typeReg, imm, sz1, ok := decodeCmpR32Imm8(body, at)
	if !ok || imm != oneHarnessStepUserInput {
		return stepLimitDispatch{}, false
	}
	if d, ok := matchStepLimitFallthrough(body, at+sz1, typeReg); ok {
		return d, true
	}
	return matchStepLimitJE(body, at+sz1, typeReg)
}

func matchStepLimitFallthrough(body []byte, afterCmp int, typeReg uint8) (stepLimitDispatch, bool) {
	jne1, ok := decodeJcc(body, afterCmp, ccNE)
	if !ok {
		return stepLimitDispatch{}, false
	}
	userStart, cmp17At := jne1.at+jne1.size, jne1.target
	if cmp17At <= userStart || cmp17At >= len(body) {
		return stepLimitDispatch{}, false
	}

	reg2, imm2, sz2, ok := decodeCmpR32Imm8(body, cmp17At)
	if !ok || reg2 != typeReg || imm2 != oneHarnessStepCheckpoint {
		return stepLimitDispatch{}, false
	}
	jne2, ok := decodeJcc(body, cmp17At+sz2, ccNE)
	if !ok {
		return stepLimitDispatch{}, false
	}
	zeroAt := jne2.at + jne2.size
	dstReg, zeroSize, ok := decodeZeroXor32(body, zeroAt)
	if !ok {
		return stepLimitDispatch{}, false
	}
	jmp2, ok := decodeJmp(body, zeroAt+zeroSize)
	if !ok {
		return stepLimitDispatch{}, false
	}

	defaultStart, merge := jne2.target, jmp2.target
	if defaultStart < jmp2.at+jmp2.size || merge <= defaultStart || merge >= len(body) {
		return stepLimitDispatch{}, false
	}
	userJmp, ok := trailingJmpTo(body, userStart, cmp17At, merge)
	if !ok {
		return stepLimitDispatch{}, false
	}
	userSlotLen := userJmp.at - userStart
	defaultSlotLen := merge - defaultStart
	if userSlotLen < zeroSize || userSlotLen > maxStepLoadSlotBytes ||
		defaultSlotLen < zeroSize || defaultSlotLen > maxStepLoadSlotBytes {
		return stepLimitDispatch{}, false
	}

	testReg, testSize, ok := decodeTestRR64(body, merge)
	if !ok || testReg != dstReg {
		return stepLimitDispatch{}, false
	}
	jg, ok := decodeJcc(body, merge+testSize, ccG)
	if !ok || jg.target < 0 || jg.target >= len(body) {
		return stepLimitDispatch{}, false
	}

	return stepLimitDispatch{
		userStart:    userStart,
		userEnd:      userJmp.at,
		defaultStart: defaultStart,
		merge:        merge,
		zeroAt:       zeroAt,
		zeroBytes:    bytes.Clone(body[zeroAt : zeroAt+zeroSize]),
	}, true
}

// trailingJmpTo finds a short (2B) or near (5B) JMP ending right at end whose
// target is wantTarget and whose start is after minStart.
func trailingJmpTo(body []byte, minStart, end, wantTarget int) (branch, bool) {
	for _, size := range []int{2, 5} {
		at := end - size
		if at < minStart {
			continue
		}
		if j, ok := decodeJmp(body, at); ok && j.size == size && j.target == wantTarget {
			return j, true
		}
	}
	return branch{}, false
}

func matchStepLimitJE(body []byte, afterCmp int, typeReg uint8) (stepLimitDispatch, bool) {
	je1, ok := decodeJcc(body, afterCmp, ccE)
	if !ok || je1.target < 0 || je1.target >= len(body) {
		return stepLimitDispatch{}, false
	}
	cmp17At := skipAlignNOPs(body, je1.at+je1.size)
	reg2, imm2, sz2, ok := decodeCmpR32Imm8(body, cmp17At)
	if !ok || reg2 != typeReg || imm2 != oneHarnessStepCheckpoint {
		return stepLimitDispatch{}, false
	}
	jne2, ok := decodeJcc(body, cmp17At+sz2, ccNE)
	if !ok {
		return stepLimitDispatch{}, false
	}
	scanStart := jne2.at + jne2.size
	scanEnd := min(len(body), scanStart+48)
	for pos := scanStart; pos < scanEnd; pos++ {
		_, zeroSize, ok := decodeZeroXor32(body, pos)
		if !ok {
			continue
		}
		return stepLimitDispatch{
			redirectUser: true,
			userBranch:   je1,
			defaultStart: -1,
			zeroAt:       pos,
			zeroBytes:    bytes.Clone(body[pos : pos+zeroSize]),
		}, true
	}
	return stepLimitDispatch{}, false
}

func locateStepUserInputLimit(_ *image, fn function) ([]func() error, error) {
	dispatches := findStepLimitDispatches(fn.body)
	var hits []func() error
	for _, d := range dispatches {
		disp := d
		hits = append(hits, func() error {
			if !disp.redirectUser {
				copy(fn.body[disp.userStart:], disp.zeroBytes)
				nopFill(fn.body[disp.userStart+len(disp.zeroBytes) : disp.userEnd])
				return nil
			}
			rel := disp.zeroAt - (disp.userBranch.at + disp.userBranch.size)
			if disp.userBranch.size == 2 {
				if rel < -128 || rel > 127 {
					return fmt.Errorf("je redirect displacement %d out of rel8 range", rel)
				}
				fn.body[disp.userBranch.at+1] = byte(int8(rel))
				return nil
			}
			binary.LittleEndian.PutUint32(fn.body[disp.userBranch.at+2:], uint32(int32(rel)))
			return nil
		})
	}
	return hits, nil
}

func locateStepDefaultLimit(_ *image, fn function) ([]func() error, error) {
	dispatches := findStepLimitDispatches(fn.body)
	var hits []func() error
	for _, d := range dispatches {
		if d.defaultStart < 0 {
			continue
		}
		disp := d
		hits = append(hits, func() error {
			copy(fn.body[disp.defaultStart:], disp.zeroBytes)
			nopFill(fn.body[disp.defaultStart+len(disp.zeroBytes) : disp.merge])
			return nil
		})
	}
	return hits, nil
}

func locateFileWatcherSelect(im *image, fn function) ([]func() error, error) {
	selectCalls, err := im.callsTo(fn, fnRuntimeSelectgo)
	if err != nil {
		return nil, err
	}
	var hits []func() error
	for _, callAt := range selectCalls {
		pre := fn.body[max(0, callAt-selectPreScanWindow):callAt]
		hasNSendsZero := bytes.Contains(pre, xorEDIEDI) || bytes.Contains(pre, movEDI0)
		hasTwoRecvs := bytes.Contains(pre, movESI2)
		hasBlocking := bytes.Contains(pre, movR8D1)
		if !hasNSendsZero || !hasTwoRecvs || !hasBlocking {
			continue
		}

		after := callAt + 5
		testReg, testSize, ok := decodeTestRR64(fn.body, after)
		if !ok || testReg != regRAX || testSize != len(testBLBLNop) {
			continue
		}
		je, ok := decodeJcc(fn.body, after+testSize, ccE)
		if !ok || je.target < 0 || je.target >= len(fn.body) {
			continue
		}

		patchAt := after
		hits = append(hits, func() error {
			copy(fn.body[patchAt:], testBLBLNop)
			return nil
		})
	}
	return hits, nil
}
