package main

import (
	"bytes"
)

const (
	xfNone   byte = 0
	xfDelta1 byte = 1 // 8-bit fixed-step delta (8-bit mono / indexed images)
	xfDelta2 byte = 2 // 16-bit fixed-step delta (WAV 16-bit / 16-bit data)
	xfDelta3 byte = 3 // 24-bit fixed-step delta (24-bit raster BMP/TGA/rgb)
	xfDelta4 byte = 4 // 32-bit fixed-step delta (32-bit images/data)
	xfBCJX86 byte = 5 // x86-64/386 jump-address transform (ELF/PE executables)
)

// delta: encode subtracts the byte d positions earlier (mod 256, using original
// values); decode adds back the already-decoded value. Reversible.

func deltaXform(b []byte, d int, decode bool) []byte {
	out := make([]byte, len(b))
	if decode {
		for i := 0; i < len(b); i++ {
			if i < d {
				out[i] = b[i]
			} else {
				out[i] = b[i] + out[i-d]
			}
		}
	} else {
		for i := 0; i < len(b); i++ {
			if i < d {
				out[i] = b[i]
			} else {
				out[i] = b[i] - b[i-d]
			}
		}
	}
	return out
}

// bcjX86 rewrites rel32 targets following E8/E9 (and 0F 8x conditional jumps) into near
// addresses relative to the block start, clustering call targets for better compression.
// Decode inverts the transform; reversible mod 2^32.

func bcjX86(b []byte, decode bool) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	n := len(out)
	for i := 0; i < n; i++ {
		var relOff int
		switch {
		case out[i] == 0xE8 || out[i] == 0xE9:
			relOff = 1
		case out[i] == 0x0F && i+1 < n && out[i+1] >= 0x80 && out[i+1] <= 0x8F:
			relOff = 2
		default:
			continue
		}
		if i+relOff+4 > n {
			continue
		}
		p := i + relOff
		addr := uint32(out[p]) | uint32(out[p+1])<<8 | uint32(out[p+2])<<16 | uint32(out[p+3])<<24
		if decode {
			addr -= uint32(i)
		} else {
			addr += uint32(i)
		}
		out[p] = byte(addr)
		out[p+1] = byte(addr >> 8)
		out[p+2] = byte(addr >> 16)
		out[p+3] = byte(addr >> 24)
		i += relOff + 3
	}
	return out
}

func applyXform(t byte, b []byte, decode bool) []byte {
	switch t {
	case xfDelta1:
		return deltaXform(b, 1, decode)
	case xfDelta2:
		return deltaXform(b, 2, decode)
	case xfDelta3:
		return deltaXform(b, 3, decode)
	case xfDelta4:
		return deltaXform(b, 4, decode)
	case xfBCJX86:
		return bcjX86(b, decode)
	}
	return b
}

// Must iterate **backwards**: forward iteration sees b[i-d] already modified, so the
// result is "minus the previous delta layer", not "minus the original value" —
// diverging from deltaXform (non-in-place). chooseTransform estimates size in-place
// but encodes with the non-in-place version; any mismatch means it is evaluating bytes
// that will never exist, so the chosen transform may not be optimal.
func deltaInPlace(b []byte, d int) {
	for i := len(b) - 1; i >= d; i-- {
		b[i] -= b[i-d]
	}
}
func bcjX86InPlace(b []byte, decode bool) {
	n := len(b)
	for i := 0; i < n; i++ {
		var relOff int
		switch {
		case b[i] == 0xE8 || b[i] == 0xE9:
			relOff = 1
		case b[i] == 0x0F && i+1 < n && b[i+1] >= 0x80 && b[i+1] <= 0x8F:
			relOff = 2
		default:
			continue
		}
		if i+relOff+4 > n {
			continue
		}
		p := i + relOff
		addr := uint32(b[p]) | uint32(b[p+1])<<8 | uint32(b[p+2])<<16 | uint32(b[p+3])<<24
		if decode {
			addr -= uint32(i)
		} else {
			addr += uint32(i)
		}
		b[p] = byte(addr)
		b[p+1] = byte(addr >> 8)
		b[p+2] = byte(addr >> 16)
		b[p+3] = byte(addr >> 24)
		i += relOff + 3
	}
}
func applyXformInPlace(t byte, b []byte) {
	switch t {
	case xfDelta1:
		deltaInPlace(b, 1)
	case xfDelta2:
		deltaInPlace(b, 2)
	case xfDelta3:
		deltaInPlace(b, 3)
	case xfDelta4:
		deltaInPlace(b, 4)
	case xfBCJX86:
		bcjX86InPlace(b, false)
	}
}

// Probe the needed preprocessing from file-header magic numbers. Only enabled for
// "uncompressed structured" types, avoiding damage to already-compressed/variable data.
// best (zstd fast mode) uses this for one file-level probe for speed; max/ultra use
// per-chunk adaptive gating instead (see chooseTransform).

func detectXform(head []byte) byte {
	if len(head) < 4 {
		return xfNone
	}
	if head[0] == 'B' && head[1] == 'M' {
		if len(head) >= 30 {
			bits := int(head[28]) | int(head[29])<<8
			switch bits {
			case 8:
				return xfDelta1
			case 16:
				return xfDelta2
			case 24:
				return xfDelta3
			case 32:
				return xfDelta4
			}
		}
		return xfDelta3
	}
	if len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")) {
		return xfDelta2
	}
	if head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'E' && len(head) >= 20 {
		machine := int(head[18]) | int(head[19])<<8
		if machine == 0x03 || machine == 0x3E {
			return xfBCJX86
		}
	}
	if head[0] == 'M' && head[1] == 'Z' && len(head) >= 64 {
		lfanew := int(head[60]) | int(head[61])<<8 | int(head[62])<<16 | int(head[63])<<24
		if lfanew > 0 && lfanew+6 <= len(head) && head[lfanew] == 'P' && head[lfanew+1] == 'E' {
			machine := int(head[lfanew+4]) | int(head[lfanew+5])<<8
			if machine == 0x14c || machine == 0x8664 {
				return xfBCJX86
			}
		}
	}
	return xfNone
}

// ---------- content-defined chunker (FastCDC gear hash) ----------
