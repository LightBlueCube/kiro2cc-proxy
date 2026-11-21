package agybin

import (
	"encoding/binary"
	"fmt"
)

// x86-64 opcode and REX constants for the instruction forms Go emits around
// our patch sites.
const (
	opNOP       = 0x90
	opRet       = 0xc3
	opLeave     = 0xc9
	opPopRBP    = 0x5d
	opCallRel32 = 0xe8
	opJmpRel32  = 0xe9
	opJmpRel8   = 0xeb
	opJccRel8   = 0x70 // + cc
	opTwoByte   = 0x0f
	opJccRel32  = 0x80 // after 0x0f, + cc
	opCmovcc    = 0x40 // after 0x0f, + cc
	opMovImm32  = 0xb8 // + (reg & 7)
	opCmpEAXImm = 0x3d
	opAluImm32  = 0x81
	opAluImm8   = 0x83
	opTestRM    = 0x85
	opMovRMR32  = 0x89
	opXorRMR32  = 0x31
	opXorR32RM  = 0x33
	opGroupFF   = 0xff

	rexBase = 0x40
	rexB    = 0x41
	rexR    = 0x44
	rexRB   = 0x45
	rexW    = 0x48
	rexWB   = 0x49
	rexWRB  = 0x4d

	modrmRegMask = 0xc0
	modrmCmpBase = 0xf8 // mod=3, reg=/7 (CMP)
	modrmCallMin = 0xd0 // mod=3, reg=/2 (CALL r64)
	modrmCallMax = 0xd7
)

// x86-64 condition codes (low nibble of Jcc / CMOVcc).
const (
	ccB  uint8 = 0x2 // below (unsigned <)
	ccE  uint8 = 0x4 // equal / zero
	ccNE uint8 = 0x5 // not equal / not zero
	ccL  uint8 = 0xc // less (signed <)
	ccLE uint8 = 0xe // less or equal (signed <=)
	ccG  uint8 = 0xf // greater (signed >)
)

// branch is a decoded relative jump (Jcc or JMP) within a function body.
type branch struct {
	at     int
	size   int
	target int
}

// decodeJcc decodes a short (rel8) or near (rel32) conditional jump with
// condition code cc at body[at].
func decodeJcc(body []byte, at int, cc uint8) (branch, bool) {
	if at+2 <= len(body) && body[at] == opJccRel8+cc {
		target := at + 2 + int(int8(body[at+1]))
		return branch{at: at, size: 2, target: target}, true
	}
	if at+6 <= len(body) && body[at] == opTwoByte && body[at+1] == opJccRel32+cc {
		target := at + 6 + int(int32(binary.LittleEndian.Uint32(body[at+2:])))
		return branch{at: at, size: 6, target: target}, true
	}
	return branch{}, false
}

// decodeJmp decodes a short (0xeb rel8) or near (0xe9 rel32) unconditional jump.
func decodeJmp(body []byte, at int) (branch, bool) {
	if at+2 <= len(body) && body[at] == opJmpRel8 {
		target := at + 2 + int(int8(body[at+1]))
		return branch{at: at, size: 2, target: target}, true
	}
	if at+5 <= len(body) && body[at] == opJmpRel32 {
		target := at + 5 + int(int32(binary.LittleEndian.Uint32(body[at+1:])))
		return branch{at: at, size: 5, target: target}, true
	}
	return branch{}, false
}

// rewriteJccToJmp turns a 2-byte or 6-byte Jcc into an unconditional JMP to
// the same intra-function target, NOP-padding any leftover byte.
func rewriteJccToJmp(body []byte, b branch) error {
	if b.target < 0 || b.target >= len(body) {
		return fmt.Errorf("branch target %+d is outside the function", b.target)
	}
	switch b.size {
	case 2:
		rel := b.target - (b.at + 2)
		if rel < -128 || rel > 127 {
			return fmt.Errorf("short jump displacement %d out of range", rel)
		}
		body[b.at] = opJmpRel8
		body[b.at+1] = byte(int8(rel))
		return nil
	case 6:
		rel := b.target - (b.at + 5)
		body[b.at] = opJmpRel32
		binary.LittleEndian.PutUint32(body[b.at+1:], uint32(int32(rel)))
		body[b.at+5] = opNOP
		return nil
	default:
		return fmt.Errorf("unexpected jcc instruction size %d", b.size)
	}
}

// encodeJmpRel32 writes a 5-byte JMP rel32 at dst for relative displacement rel.
func encodeJmpRel32(dst []byte, rel int64) error {
	if rel != int64(int32(rel)) {
		return fmt.Errorf("jump displacement %d is out of rel32 range", rel)
	}
	dst[0] = opJmpRel32
	binary.LittleEndian.PutUint32(dst[1:], uint32(int32(rel)))
	return nil
}

// nopFill overwrites dst with NOP (0x90) bytes.
func nopFill(dst []byte) {
	for i := range dst {
		dst[i] = opNOP
	}
}

// decodeMovAbsImm64 matches MOV r64, imm64 (48/49 b8..bf <imm64>) for any r64.
func decodeMovAbsImm64(body []byte, at int) (imm uint64, size int, ok bool) {
	if at+10 > len(body) {
		return 0, 0, false
	}
	if body[at] != rexW && body[at] != rexWB {
		return 0, 0, false
	}
	if body[at+1] < opMovImm32 || body[at+1] > opMovImm32+7 {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint64(body[at+2:]), 10, true
}

// decodeTestRR64 matches TEST r64, r64 against itself (48/4d 85 ModRM).
func decodeTestRR64(body []byte, at int) (reg uint8, size int, ok bool) {
	if at+3 > len(body) || body[at+1] != opTestRM {
		return 0, 0, false
	}
	rex, modrm := body[at], body[at+2]
	if modrm < modrmRegMask || ((modrm>>3)&7) != (modrm&7) {
		return 0, 0, false
	}
	switch rex {
	case rexW:
		return modrm & 7, 3, true
	case rexWRB:
		return 8 + (modrm & 7), 3, true
	default:
		return 0, 0, false
	}
}

// decodeZeroXor32 matches XOR r32, r32 against itself (zeroing r64).
func decodeZeroXor32(body []byte, at int) (reg uint8, size int, ok bool) {
	if at+2 <= len(body) && (body[at] == opXorRMR32 || body[at] == opXorR32RM) {
		modrm := body[at+1]
		if modrm >= modrmRegMask && ((modrm>>3)&7) == (modrm&7) {
			return modrm & 7, 2, true
		}
	}
	if at+3 <= len(body) && body[at] == rexRB && (body[at+1] == opXorRMR32 || body[at+1] == opXorR32RM) {
		modrm := body[at+2]
		if modrm >= modrmRegMask && ((modrm>>3)&7) == (modrm&7) {
			return 8 + (modrm & 7), 3, true
		}
	}
	return 0, 0, false
}

// decodeCmpR32Imm8 matches CMP r32, imm8 for any r32 (0..15).
func decodeCmpR32Imm8(body []byte, at int) (reg uint8, imm int8, size int, ok bool) {
	if at+3 <= len(body) && body[at] == opAluImm8 && body[at+1] >= modrmCmpBase {
		return body[at+1] - modrmCmpBase, int8(body[at+2]), 3, true
	}
	if at+4 <= len(body) && body[at] == rexB && body[at+1] == opAluImm8 && body[at+2] >= modrmCmpBase {
		return 8 + (body[at+2] - modrmCmpBase), int8(body[at+3]), 4, true
	}
	return 0, 0, 0, false
}

// decodeCmpR32Imm32 matches CMP r32, imm32 (including the 5-byte CMP EAX form).
func decodeCmpR32Imm32(body []byte, at int) (reg uint8, imm uint32, size int, ok bool) {
	if at+5 <= len(body) && body[at] == opCmpEAXImm {
		return 0, binary.LittleEndian.Uint32(body[at+1:]), 5, true
	}
	if at+6 <= len(body) && body[at] == opAluImm32 && body[at+1] >= modrmCmpBase {
		return body[at+1] - modrmCmpBase, binary.LittleEndian.Uint32(body[at+2:]), 6, true
	}
	if at+7 <= len(body) && body[at] == rexB && body[at+1] == opAluImm32 && body[at+2] >= modrmCmpBase {
		return 8 + (body[at+2] - modrmCmpBase), binary.LittleEndian.Uint32(body[at+3:]), 7, true
	}
	return 0, 0, 0, false
}

// decodeMovR32Imm32 matches MOV r32, imm32 for any r32 (0..15).
func decodeMovR32Imm32(body []byte, at int) (reg uint8, imm uint32, size int, ok bool) {
	if at+5 <= len(body) && body[at] >= opMovImm32 && body[at] <= opMovImm32+7 {
		return body[at] - opMovImm32, binary.LittleEndian.Uint32(body[at+1:]), 5, true
	}
	if at+6 <= len(body) && body[at] == rexB && body[at+1] >= opMovImm32 && body[at+1] <= opMovImm32+7 {
		return 8 + (body[at+1] - opMovImm32), binary.LittleEndian.Uint32(body[at+2:]), 6, true
	}
	return 0, 0, 0, false
}

// decodeCmovR32 matches CMOVcc r32, r32 for any register pair (0..15).
func decodeCmovR32(body []byte, at int) (cc, dst, src uint8, size int, ok bool) {
	var rex uint8
	off := at
	if off < len(body) && (body[off] == rexB || body[off] == rexR || body[off] == rexRB) {
		rex = body[off]
		off++
	}
	if off+3 > len(body) || body[off] != opTwoByte {
		return 0, 0, 0, 0, false
	}
	op := body[off+1]
	if op < opCmovcc || op > opCmovcc+0x0f {
		return 0, 0, 0, 0, false
	}
	modrm := body[off+2]
	if modrm < modrmRegMask {
		return 0, 0, 0, 0, false
	}
	dst = ((rex & 4) << 1) | ((modrm >> 3) & 7)
	src = ((rex & 1) << 3) | (modrm & 7)
	return op - opCmovcc, dst, src, off + 3 - at, true
}

// encodeMovR32 encodes MOV r32(dst), r32(src); returns nil when dst == src.
func encodeMovR32(dst, src uint8) []byte {
	if dst == src {
		return nil
	}
	modrm := byte(modrmRegMask) | ((src & 7) << 3) | (dst & 7)
	if dst < 8 && src < 8 {
		return []byte{opMovRMR32, modrm}
	}
	rex := byte(rexBase) | ((src >> 3) << 2) | (dst >> 3)
	return []byte{rex, opMovRMR32, modrm}
}

// decodeIndirectCall matches CALL r64 (ff d0..d7 or 41 ff d0..d7).
func decodeIndirectCall(body []byte, at int) (size int, ok bool) {
	if at+2 <= len(body) && body[at] == opGroupFF && body[at+1] >= modrmCallMin && body[at+1] <= modrmCallMax {
		return 2, true
	}
	if at+3 <= len(body) && body[at] == rexB && body[at+1] == opGroupFF && body[at+2] >= modrmCallMin && body[at+2] <= modrmCallMax {
		return 3, true
	}
	return 0, false
}

// skipAlignNOPs advances past 1-to-9-byte x86-64 alignment NOP padding.
func skipAlignNOPs(body []byte, at int) int {
	for at < len(body) {
		switch {
		case body[at] == opNOP:
			at++
		case at+2 <= len(body) && body[at] == 0x66 && body[at+1] == opNOP:
			at += 2
		case at+3 <= len(body) && body[at] == opTwoByte && body[at+1] == 0x1f && body[at+2] == 0x00:
			at += 3
		case at+4 <= len(body) && body[at] == opTwoByte && body[at+1] == 0x1f && body[at+2] == 0x40:
			at += 4
		case at+5 <= len(body) && body[at] == opTwoByte && body[at+1] == 0x1f && body[at+2] == 0x44:
			at += 5
		case at+7 <= len(body) && body[at] == opTwoByte && body[at+1] == 0x1f && body[at+2] == 0x80:
			at += 7
		case at+8 <= len(body) && body[at] == opTwoByte && body[at+1] == 0x1f && body[at+2] == 0x84:
			at += 8
		case at+9 <= len(body) && body[at] == 0x66 && body[at+1] == opTwoByte && body[at+2] == 0x1f && body[at+3] == 0x84:
			at += 9
		default:
			return at
		}
	}
	return at
}

// findEpilogue searches up to maxScan bytes from start for a Go function
// epilogue: either `leave; ret` or `[add rsp, imm;] pop rbp; ret`.
func findEpilogue(body []byte, start, maxScan int) (int, bool) {
	end := min(len(body), start+maxScan)
	leaveAt, popAt := -1, -1
	for i := start; i+2 <= end; i++ {
		if leaveAt < 0 && body[i] == opLeave && body[i+1] == opRet {
			leaveAt = i
		}
		if popAt < 0 && body[i] == opPopRBP && body[i+1] == opRet {
			popAt = i
		}
	}
	if leaveAt >= 0 && (popAt < 0 || leaveAt < popAt) {
		return leaveAt, true
	}
	if popAt < 0 {
		return 0, false
	}
	// If preceded by `add rsp, imm8/imm32`, jump there so SP is restored too.
	scanLo := max(start, popAt-16)
	for i := scanLo; i < popAt; i++ {
		if i+4 == popAt && body[i] == rexW && body[i+1] == opAluImm8 && body[i+2] == 0xc4 {
			return i, true
		}
		if i+7 == popAt && body[i] == rexW && body[i+1] == opAluImm32 && body[i+2] == 0xc4 {
			return i, true
		}
	}
	return popAt, true
}
