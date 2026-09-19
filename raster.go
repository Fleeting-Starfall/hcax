package main

// raster.go - handling for raster images (uncompressed bitmaps)
//  1) Recognize BMP / TGA / PNM headers; get pixel offset, row stride, channel count
//  2) Reversible color transform (RCT): channels are highly correlated (R≈G≈B), so
//     (G, R-G, B-G) pushes two difference channels near 0, removing bulk redundancy.
//     Measured 11-14% extra savings on real photos.
//  3) 2-D median edge prediction (MED, JPEG-LS LOCO-I): predicts from left/up/up-left
//     along the edge direction; more stable than "left+up-up-left" gradient at edges.
//     Beats gradient/Paeth/average in practice.
//  4) Byte entropy: helps "incompressible" decisions, so image residuals that are
//     incompressible per chunk but compressible overall are not mistaken for random.
// Pure algorithm, reversible (byte-wise mod 256), no AI, no external deps.

import "math"

const (
	xfRasterMed    byte = 6 // raster: MED 2-D prediction (legacy, whole row incl. padding)
	xfRasterRctMed byte = 7 // raster: reversible color transform + MED (legacy, flat rows)
	// 8/9 are row-wise. BMP rows have 0-3 bytes of 4-byte alignment padding; at 24bpp
	// with width%4 in {1,2} the stride is not a multiple of 3 — legacy transform 7
	// treated padding plus the next row's first two bytes as one "pixel" for color
	// decorrelation, so channel phase drifted every row and MED's up/up-left neighbors
	// landed on other channels, killing vertical prediction (measured 60% worse than no
	// transform). 8/9 operate only on the first rowBytes of each row, skip padding, and
	// keep the channel phase fixed. Measured 4-7% better than the legacy path on such
	// images.
	xfRasterRowMed    byte = 8 // raster: MED 2-D prediction (row-wise, skip row padding)
	xfRasterRowRctMed byte = 9 // raster: row-wise color transform + row-wise MED
)

// Raster transforms need the whole image in memory (prediction spans rows/chunks);
// skip oversized images to bound memory, falling back to streaming pack.
const rasterMaxBytes = 512 << 20

// Raster geometry
type rasterGeom struct {
	dataOff  int // offset of pixel data
	stride   int // bytes per row (incl. BMP 4-byte alignment padding)
	rowBytes int // real pixel bytes per row (= width * bpp, no padding)
	rows     int // number of rows
	bpp      int // bytes per pixel (channels)
}

func le16(b []byte, o int) uint16 { return uint16(b[o]) | uint16(b[o+1])<<8 }
func le32(b []byte, o int) uint32 {
	return uint32(b[o]) | uint32(b[o+1])<<8 | uint32(b[o+2])<<16 | uint32(b[o+3])<<24
}

// Quick header-magic check whether b looks like a raster (decides whether to read
// the whole file).
func rasterLikely(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	if b[0] == 'B' && b[1] == 'M' { // BMP
		return true
	}
	if b[0] == 'P' && (b[1] == '5' || b[1] == '6') { // PNM P5 (gray) / P6 (color)
		return true
	}
	if len(b) >= 18 && b[1] == 0 { // TGA: no palette
		t := b[2]
		if t == 2 || t == 3 { // uncompressed truecolor/grayscale
			depth := int(b[16])
			if depth == 8 || depth == 24 || depth == 32 {
				return true
			}
		}
	}
	return false
}

// Full recognition; returns geometry, ok=false on failure.
func rasterInfo(b []byte) (rasterGeom, bool) {
	var g rasterGeom
	// ---- BMP ----
	if len(b) >= 54 && b[0] == 'B' && b[1] == 'M' {
		dataOff := int(le32(b, 10))
		w := int(int32(le32(b, 18)))
		h := int(int32(le32(b, 22)))
		bits := int(le16(b, 28))
		comp := le32(b, 30)
		if comp != 0 { // BI_RGB uncompressed only
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
		rb := w * g.bpp
		if stride <= 0 || rb > stride || dataOff < 0 || dataOff+stride*rows > len(b) {
			return g, false
		}
		g.dataOff, g.stride, g.rowBytes, g.rows = dataOff, stride, rb, rows
		return g, true
	}
	// ---- PNM (P5 gray / P6 color, maxval<=255) ----
	if len(b) >= 3 && b[0] == 'P' && (b[1] == '5' || b[1] == '6') {
		pos := 2
		nums := []int{}
		for pos < len(b) && len(nums) < 3 {
			c := b[pos]
			if c == '#' { // comment line
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
		pos++ // single whitespace separator
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
		g.dataOff, g.stride, g.rowBytes, g.rows, g.bpp = pos, w*ch, w*ch, h, ch
		return g, true
	}
	// ---- TGA (type 2/3, 24/32/8-bit, no palette) ----
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
		stride := w * ch // TGA has no row alignment padding
		if off+stride*h > len(b) {
			return g, false
		}
		g.dataOff, g.stride, g.rowBytes, g.rows, g.bpp = off, stride, stride, h, ch
		return g, true
	}
	return g, false
}

func stride3(w, ch, h int) int { return w * ch * h }

// ---- MED 2-D median edge prediction ----
// forward: compute residuals; false: inverse. Modifies the pixel region in place.
func medApply(b []byte, g rasterGeom, forward bool, rowAware bool) {
	px := b[g.dataOff : g.dataOff+g.stride*g.rows]
	stride, rows, bp := g.stride, g.rows, g.bpp
	// rowAware: process only the first rowBytes of each row; otherwise include row
	// padding (legacy behavior)
	rb := stride
	if rowAware {
		rb = g.rowBytes
	}
	if forward {
		src := make([]byte, len(px))
		copy(src, px)
		for y := 0; y < rows; y++ {
			base := y * stride
			for x := 0; x < rb; x++ {
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
		for x := 0; x < rb; x++ {
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

// LOCO-I / JPEG-LS median edge detection predictor
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

// ---- reversible color transform (LOCO-I style) ----
// forward: (R,G,B) -> (G, R-G, B-G); inverse: G=c0, R=c1+G, B=c2+G. Reversible
// byte-wise mod 256. Only applies to 3+ channel images (first 3 channels; channel 4
// like alpha is left to MED). rowAware=true processes per row (skips padding); false
// slides 3 bytes over the whole buffer (legacy, decode-only for old archives; mixes
// padding with the next row when stride%bpp!=0)
func rctApply(b []byte, g rasterGeom, forward bool, rowAware bool) {
	if g.bpp < 3 {
		return
	}
	px := b[g.dataOff : g.dataOff+g.stride*g.rows]
	bp := g.bpp
	if !rowAware {
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
		return
	}
	for y := 0; y < g.rows; y++ {
		base := y * g.stride
		for i := base; i+bp <= base+g.rowBytes; i += bp {
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
}

// Apply or invert a raster transform (forward=false inverts, reverse order).
// Returns success (false for non-raster or out-of-bounds; caller treats as xfNone).
func rasterApply(t byte, b []byte, forward bool) bool {
	var rowAware, rct bool
	switch t {
	case xfRasterMed:
	case xfRasterRctMed:
		rct = true
	case xfRasterRowMed:
		rowAware = true
	case xfRasterRowRctMed:
		rowAware, rct = true, true
	default:
		return false
	}
	g, ok := rasterInfo(b)
	if !ok {
		return false
	}
	if rct && g.bpp < 3 {
		return false // grayscale has no color to decorrelate
	}
	if forward {
		if rct {
			rctApply(b, g, true, rowAware)
		}
		medApply(b, g, true, rowAware)
	} else {
		medApply(b, g, false, rowAware)
		if rct {
			rctApply(b, g, false, rowAware)
		}
	}
	return true
}

// Pick the best candidate transform by gated predicted size (never worse); returns
// the chosen type. Caller must have whole in memory. Candidates: none / MED / RCT+MED.
func (b *backend) chooseRasterXform(whole []byte) byte {
	g, ok := rasterInfo(whole)
	if !ok {
		return xfNone
	}
	// Only write row-wise versions (8/9); 6/7 decode old archives only, never generated
	cands := []byte{xfNone, xfRasterRowMed}
	if g.bpp >= 3 {
		cands = append(cands, xfRasterRowRctMed)
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

// Gated size estimate: use the cheap encoder if available, else byte entropy.
func gateSize(b *backend, data []byte) int {
	if b != nil && b.gateEnc != nil {
		return len(b.gateEnc.EncodeAll(data, nil))
	}
	return int(byteEntropy(data) * float64(len(data)) / 8.0)
}

// ---------- legacy (v2~v6) read compatibility ----------
// Early versions used 2-D gradient prediction (left+up-up-left) on BMP with no
// file-level transform record. Keep the inverse for historical archives (compat
// unpack path only; never used for new archives).

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
	g.dataOff, g.stride, g.rowBytes, g.rows = dataOff, stride, stride, rows
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

// Byte entropy (bits/byte, 0-8): distinguishes true randomness (≈8) from residual
// data with structure (<8).
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
