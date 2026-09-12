package main

import (
	"bytes"
)

const (
	xfNone   byte = 0
	xfDelta1 byte = 1 // 8-bit  定步长差分 (单声道8位/索引图)
	xfDelta2 byte = 2 // 16-bit 定步长差分 (WAV 16bit/16位数据)
	xfDelta3 byte = 3 // 24-bit 定步长差分 (24位光栅图 BMP/TGA/rgb)
	xfDelta4 byte = 4 // 32-bit 定步长差分 (32位图/32位数据)
	xfBCJX86 byte = 5 // x86-64/386 跳转地址变换 (ELF/PE 可执行)
)

// delta 差分: encode 时各字节减前 d 字节(模256, 用原始值); decode 时加回已解码值. 可逆.

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

// BCJ-x86: 把 E8/E9(及 0F 8x 条件跳)后的 rel32 相对地址改写为"相对块首偏移的近地址",
// 使大量指向相近函数的调用地址聚集, 利于压缩. decode 逆运算. 可逆(mod 2^32).

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

// 必须**倒序**遍历: 前向遍历时 b[i-d] 已经被改写过了, 算出来的就不是
// "减原始值" 而是 "减上一层差分" —— 与 deltaXform(非 in-place)结果不同。
// chooseTransform 用 in-place 版估算大小、却用非 in-place 版实际编码,
// 两者不一致等于在评估一组永远不会产生的字节, 选出来的变换未必最优。
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

// 按文件头魔数探测应使用的预处理. 仅对"未压缩结构化"类型启用, 避免误伤已压缩/变长数据.
// best(zstd 快档) 用此做文件级一次探测保速度; max/ultra 改逐块自适应门控(见 chooseTransform).

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
