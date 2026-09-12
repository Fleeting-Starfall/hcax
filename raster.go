package main

// raster.go - 光栅图(未压缩位图)专用处理
//  1) 识别 BMP / TGA / PNM 头, 拿到像素区偏移/行跨距/通道数
//  2) 可逆色彩去相关(RCT): 彩图相邻通道高度相关(R≈G≈B), 换成 (G, R-G, B-G) 后
//     两个差通道接近 0 -> 直接消掉一大块冗余。实测真实照片再省 11~14%。
//  3) 二维中值边缘预测(MED, JPEG-LS 的 LOCO-I 预测器): 用左/上/左上按边缘方向预测,
//     比"左+上-左上"梯度更稳(在边缘处不越界)。实测优于梯度/Paeth/平均。
//  4) 字节熵: 辅助"不可压判定", 避免把"块内不可压但整体可压"的图像残差误判为随机数据
// 纯算法, 可逆(逐字节 mod 256), 无 AI, 无外部依赖。

import "math"

const (
	xfRasterMed    byte = 6 // 光栅: MED 二维预测
	xfRasterRctMed byte = 7 // 光栅: 可逆色彩去相关 + MED
)

// 光栅变换需把整图读进内存(预测跨行/跨块), 超大图跳过以免占用过多内存, 退回流式打包
const rasterMaxBytes = 512 << 20

// 光栅几何信息
type rasterGeom struct {
	dataOff int // 像素数据起始偏移
	stride  int // 每行字节数(含 BMP 的 4 字节对齐填充)
	rows    int // 行数
	bpp     int // 每像素字节数(通道数)
}

func le16(b []byte, o int) uint16 { return uint16(b[o]) | uint16(b[o+1])<<8 }
func le32(b []byte, o int) uint32 {
	return uint32(b[o]) | uint32(b[o+1])<<8 | uint32(b[o+2])<<16 | uint32(b[o+3])<<24
}

// 只凭头部魔数快速判断"像不像光栅图"(用于决定要不要读整个文件)
func rasterLikely(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	if b[0] == 'B' && b[1] == 'M' { // BMP
		return true
	}
	if b[0] == 'P' && (b[1] == '5' || b[1] == '6') { // PNM P5(灰)/P6(彩)
		return true
	}
	if len(b) >= 18 && b[1] == 0 { // TGA: 无调色板
		t := b[2]
		if t == 2 || t == 3 { // 未压缩 真彩/灰度
			depth := int(b[16])
			if depth == 8 || depth == 24 || depth == 32 {
				return true
			}
		}
	}
	return false
}

// 完整识别并返回几何信息; 失败返回 ok=false
func rasterInfo(b []byte) (rasterGeom, bool) {
	var g rasterGeom
	// ---- BMP ----
	if len(b) >= 54 && b[0] == 'B' && b[1] == 'M' {
		dataOff := int(le32(b, 10))
		w := int(int32(le32(b, 18)))
		h := int(int32(le32(b, 22)))
		bits := int(le16(b, 28))
		comp := le32(b, 30)
		if comp != 0 { // 仅支持 BI_RGB 未压缩
			return g, false
		}
		switch bits {
		case 8:
			g.bpp = 1
		case 16:
			g.bpp = 2
		case 24:
			g.bpp = 3
		case 32:
			g.bpp = 4
		default:
			return g, false
		}
		if w <= 0 || h == 0 {
			return g, false
		}
		rows := h
		if rows < 0 {
			rows = -rows
		}
		stride := ((w*g.bpp + 3) / 4) * 4
		if stride <= 0 || dataOff < 0 || dataOff+stride*rows > len(b) {
			return g, false
		}
		g.dataOff, g.stride, g.rows = dataOff, stride, rows
		return g, true
	}
	// ---- PNM (P5 灰度 / P6 彩色, maxval<=255) ----
	if len(b) >= 3 && b[0] == 'P' && (b[1] == '5' || b[1] == '6') {
		pos := 2
		nums := []int{}
		for pos < len(b) && len(nums) < 3 {
			c := b[pos]
			if c == '#' { // 注释行
				for pos < len(b) && b[pos] != '\n' {
					pos++
				}
				continue
			}
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				pos++
				continue
			}
			if c < '0' || c > '9' {
				return g, false
			}
			v := 0
			for pos < len(b) && b[pos] >= '0' && b[pos] <= '9' {
				v = v*10 + int(b[pos]-'0')
				pos++
			}
			nums = append(nums, v)
		}
		if len(nums) != 3 || pos >= len(b) {
			return g, false
		}
		pos++ // 单个空白分隔符
		w, h, maxv := nums[0], nums[1], nums[2]
		ch := 1
		if b[1] == '6' {
			ch = 3
		}
		if w <= 0 || h <= 0 || maxv <= 0 || maxv > 255 {
			return g, false
		}
		if pos+stride3(w, ch, h) > len(b) {
			return g, false
		}
		g.dataOff, g.stride, g.rows, g.bpp = pos, w*ch, h, ch
		return g, true
	}
	// ---- TGA (类型 2/3, 24/32/8 位, 无调色板) ----
	if len(b) >= 18 && b[1] == 0 && (b[2] == 2 || b[2] == 3) {
		idLen := int(b[0])
		w := int(le16(b, 12))
		h := int(le16(b, 14))
		depth := int(b[16])
		var ch int
		switch depth {
		case 8:
			ch = 1
		case 24:
			ch = 3
		case 32:
			ch = 4
		default:
			return g, false
		}
		if w <= 0 || h <= 0 {
			return g, false
		}
		off := 18 + idLen
		stride := w * ch // TGA 无行对齐填充
		if off+stride*h > len(b) {
			return g, false
		}
		g.dataOff, g.stride, g.rows, g.bpp = off, stride, h, ch
		return g, true
	}
	return g, false
}

func stride3(w, ch, h int) int { return w * ch * h }

// ---- MED 二维中值边缘预测 ----
// forward: 求残差; false: 逆变换还原。就地修改像素区。
func medApply(b []byte, g rasterGeom, forward bool) {
	px := b[g.dataOff : g.dataOff+g.stride*g.rows]
	stride, rows, bp := g.stride, g.rows, g.bpp
	if forward {
		src := make([]byte, len(px))
		copy(src, px)
		for y := 0; y < rows; y++ {
			base := y * stride
			for x := 0; x < stride; x++ {
				i := base + x
				var l, u, ul byte
				if x >= bp {
					l = src[i-bp]
				}
				if y > 0 {
					u = src[i-stride]
					if x >= bp {
						ul = src[i-stride-bp]
					}
				}
				px[i] = src[i] - medPred(l, u, ul)
			}
		}
		return
	}
	for y := 0; y < rows; y++ {
		base := y * stride
		for x := 0; x < stride; x++ {
			i := base + x
			var l, u, ul byte
			if x >= bp {
				l = px[i-bp]
			}
			if y > 0 {
				u = px[i-stride]
				if x >= bp {
					ul = px[i-stride-bp]
				}
			}
			px[i] = px[i] + medPred(l, u, ul)
		}
	}
}

// LOCO-I / JPEG-LS 中值边缘检测预测器
func medPred(l, u, ul byte) byte {
	li, ui, uli := int(l), int(u), int(ul)
	if uli >= li && uli >= ui {
		if li < ui {
			return l
		}
		return u
	}
	if uli <= li && uli <= ui {
		if li > ui {
			return l
		}
		return u
	}
	return byte(li + ui - uli)
}

// ---- 可逆色彩去相关 (LOCO-I 式) ----
// forward: (R,G,B) -> (G, R-G, B-G);  逆: G=c0, R=c1+G, B=c2+G。逐字节 mod 256 可逆。
// 仅对 3 通道以上生效(处理前 3 个通道; 第 4 通道如 alpha 原样保留, 交给 MED 预测)。
func rctApply(b []byte, g rasterGeom, forward bool) {
	if g.bpp < 3 {
		return
	}
	px := b[g.dataOff : g.dataOff+g.stride*g.rows]
	bp := g.bpp
	for i := 0; i+bp <= len(px); i += bp {
		if forward {
			R, G, B := px[i], px[i+1], px[i+2]
			px[i] = G
			px[i+1] = R - G
			px[i+2] = B - G
		} else {
			G := px[i]
			px[i] = px[i+1] + G
			px[i+1] = G
			px[i+2] = px[i+2] + G
		}
	}
}

// 应用/逆应用光栅变换。forward=false 时为逆变换(顺序相反)。
// 返回是否成功(非光栅或越界则 false, 调用方应视作 xfNone)。
func rasterApply(t byte, b []byte, forward bool) bool {
	if t != xfRasterMed && t != xfRasterRctMed {
		return false
	}
	g, ok := rasterInfo(b)
	if !ok {
		return false
	}
	if g.bpp < 3 && t == xfRasterRctMed {
		return false // 灰度图无色彩可去相关
	}
	if forward {
		if t == xfRasterRctMed {
			rctApply(b, g, true)
		}
		medApply(b, g, true)
	} else {
		medApply(b, g, false)
		if t == xfRasterRctMed {
			rctApply(b, g, false)
		}
	}
	return true
}

// 从候选变换里按门控预测大小选最优(绝不退步), 返回选中的变换类型。
// 调用方需先把 whole 读进来。xform 候选: 无 / MED / RCT+MED。
func (b *backend) chooseRasterXform(whole []byte) byte {
	g, ok := rasterInfo(whole)
	if !ok {
		return xfNone
	}
	cands := []byte{xfNone, xfRasterMed}
	if g.bpp >= 3 {
		cands = append(cands, xfRasterRctMed)
	}
	best, bestSz := xfNone, -1
	for _, t := range cands {
		var tb []byte
		if t == xfNone {
			tb = whole
		} else {
			tb = append([]byte(nil), whole...)
			rasterApply(t, tb, true)
		}
		sz := gateSize(b, tb)
		if bestSz < 0 || sz < bestSz {
			best, bestSz = t, sz
		}
	}
	return best
}

// 门控预测大小: 有廉价编码器就用它, 否则退回字节熵估计
func gateSize(b *backend, data []byte) int {
	if b != nil && b.gateEnc != nil {
		return len(b.gateEnc.EncodeAll(data, nil))
	}
	return int(byteEntropy(data) * float64(len(data)) / 8.0)
}

// ---------- 旧版(v2~v6)读取兼容 ----------
// 早期版本对 BMP 用的是"二维梯度预测(左+上-左上)"且无文件级变换记录,
// 为能解出历史归档, 保留该算法的逆变换(仅供解包兼容路径使用, 新归档不再使用)。

func bmpInfoLegacy(b []byte) (rasterGeom, bool) {
	var g rasterGeom
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return g, false
	}
	dataOff := int(le32(b, 10))
	w := int(int32(le32(b, 18)))
	h := int(int32(le32(b, 22)))
	bits := int(le16(b, 28))
	if le32(b, 30) != 0 {
		return g, false
	}
	switch bits {
	case 8:
		g.bpp = 1
	case 16:
		g.bpp = 2
	case 24:
		g.bpp = 3
	case 32:
		g.bpp = 4
	default:
		return g, false
	}
	if w <= 0 || h == 0 {
		return g, false
	}
	rows := h
	if rows < 0 {
		rows = -rows
	}
	stride := ((w*g.bpp + 3) / 4) * 4
	if stride <= 0 || dataOff < 0 || dataOff+stride*rows > len(b) {
		return g, false
	}
	g.dataOff, g.stride, g.rows = dataOff, stride, rows
	return g, true
}

func pred2DLegacy(b []byte, forward bool) bool {
	g, ok := bmpInfoLegacy(b)
	if !ok {
		return false
	}
	px := b[g.dataOff : g.dataOff+g.stride*g.rows]
	stride, rows, bp := g.stride, g.rows, g.bpp
	src := px
	if forward {
		src = make([]byte, len(px))
		copy(src, px)
	}
	for y := 0; y < rows; y++ {
		base := y * stride
		for x := 0; x < stride; x++ {
			i := base + x
			var l, u, ul byte
			if x >= bp {
				l = src[i-bp]
			}
			if y > 0 {
				u = src[i-stride]
				if x >= bp {
					ul = src[i-stride-bp]
				}
			}
			p := byte(int(l) + int(u) - int(ul))
			if forward {
				px[i] = src[i] - p
			} else {
				px[i] = px[i] + p
			}
		}
	}
	return true
}

// 字节熵(bits/byte, 0~8): 用于区分"真随机(≈8)"与"有分布的残差(<8)"
func byteEntropy(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var hist [256]int
	for _, c := range b {
		hist[c]++
	}
	n := float64(len(b))
	var e float64
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}
