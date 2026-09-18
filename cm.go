package main

// cm.go - 上下文混合(Context Mixing)压缩器 v2
// 思路: 不"找重复", 而是"预测下一个 bit"。多个不同阶的上下文模型各自给出 P(bit=1),
// 经两级 APM/SSE 校正后交二值算术编码器。对"非重复文本"同样有效(利用符号间统计依赖)。
// 纯算法, 无 AI/大模型, 纯 CPU。编解码完全对称(确定性)。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// ---------------- 二值算术编码器 ----------------
type arEnc struct {
	x1, x2 uint32
	out    []byte
}

// 初始化区间(必须调用; 零值 x2=0 会导致区间塌陷)
func (e *arEnc) init() { e.x1, e.x2, e.out = 0, 0xffffffff, e.out[:0] }

// p: P(bit=1) 的 12-bit 表示(1..4094)
func (e *arEnc) encode(bit int, p uint32) {
	if p < 1 {
		p = 1
	} else if p > 4094 {
		p = 4094
	}
	xmid := e.x1 + ((e.x2-e.x1)>>12)*p
	if bit == 1 {
		e.x2 = xmid
	} else {
		e.x1 = xmid + 1
	}
	for (e.x1^e.x2)&0xff000000 == 0 {
		e.out = append(e.out, byte(e.x2>>24))
		e.x1 <<= 8
		e.x2 = (e.x2 << 8) | 255
	}
}

func (e *arEnc) flush() {
	e.out = append(e.out, byte(e.x1>>24), byte(e.x1>>16), byte(e.x1>>8), byte(e.x1))
}

type arDec struct {
	x1, x2, x uint32
	in        []byte
	pos       int
}

func (d *arDec) init(in []byte) {
	d.x1, d.x2, d.x = 0, 0xffffffff, 0
	d.in, d.pos = in, 0
	for i := 0; i < 4; i++ {
		d.x = (d.x << 8) | uint32(d.nextByte())
	}
}

func (d *arDec) nextByte() byte {
	if d.pos < len(d.in) {
		b := d.in[d.pos]
		d.pos++
		return b
	}
	return 0
}

func (d *arDec) decode(p uint32) int {
	if p < 1 {
		p = 1
	} else if p > 4094 {
		p = 4094
	}
	xmid := d.x1 + ((d.x2-d.x1)>>12)*p
	var bit int
	if d.x <= xmid {
		bit = 1
		d.x2 = xmid
	} else {
		bit = 0
		d.x1 = xmid + 1
	}
	for (d.x1^d.x2)&0xff000000 == 0 {
		d.x1 <<= 8
		d.x2 = (d.x2 << 8) | 255
		d.x = (d.x << 8) | uint32(d.nextByte())
	}
	return bit
}

// ---------------- stretch / squash ----------------
var stretchT [4096]int32
var squashT [4096]int32
var cmInitDone bool

func initCMTables() {
	if cmInitDone {
		return
	}
	for i := 0; i < 4096; i++ {
		p := (float64(i) + 0.5) / 4096.0
		stretchT[i] = int32(math.Round(math.Log(p/(1.0-p)) * 256.0))
	}
	for i := 0; i < 4096; i++ {
		x := float64(i-2048) / 256.0
		squashT[i] = int32(math.Round(4096.0 / (1.0 + math.Exp(-x))))
	}
	cmInitDone = true
}

func squash(x int32) int32 {
	if x > 2047 {
		x = 2047
	} else if x < -2047 {
		x = -2047
	}
	return squashT[x+2048]
}

// ---------------- 自适应概率计数器 ----------------
// 每项 uint32: 高 16 位 = P(1)(0..65535), 低 16 位 = 观测计数(用于自适应学习率)
type counterTbl struct {
	t    []uint32
	mask uint32
}

func newCounterTbl(bits int) *counterTbl {
	n := 1 << uint(bits)
	t := make([]uint32, n)
	for i := range t {
		t[i] = 32768 << 16
	}
	return &counterTbl{t: t, mask: uint32(n - 1)}
}

var cmRcp [1024]int32

func initRcp() {
	for n := 0; n < 1024; n++ {
		cmRcp[n] = int32(65536 / (n + 2))
	}
}

func (c *counterTbl) p16(idx uint32) uint32 {
	return c.t[idx] >> 16
}

func (c *counterTbl) update(idx uint32, bit int) {
	v := c.t[idx]
	p := int32(v >> 16)
	n := int(v & 0xffff)
	var tgt int32
	if bit == 1 {
		tgt = 65535
	}
	if n > 1023 {
		n = 1023
	}
	// 这里保留 int64 中间量: 在 arm64 上改 int32 反而更慢(32 位运算要额外
	// 符号扩展), 实测 4 轮配对比较每轮都慢 3~5%。乘积上界 65534*32768
	// 虽仍在 int32 内, 但没有任何收益, 别"优化"这里。
	p += int32((int64(tgt-p) * int64(cmRcp[n])) >> 16)
	if p < 1 {
		p = 1
	} else if p > 65534 {
		p = 65534
	}
	if n < 1023 {
		n++
	}
	c.t[idx] = uint32(p)<<16 | uint32(n)
}

// ---------------- APM / SSE 二级校正 ----------------
type apm struct {
	t   []uint16
	idx int
	n   int
}

func newAPM(nctx int) *apm {
	a := &apm{t: make([]uint16, nctx*33), n: nctx}
	for i := 0; i < nctx; i++ {
		for j := 0; j < 33; j++ {
			a.t[i*33+j] = uint16(squash(int32(j-16)*128) * 16)
		}
	}
	return a
}

func (a *apm) pp(pr int32, cx int) int32 {
	s := int((pr + 2048) * 32 / 4096)
	if s > 31 {
		s = 31
	}
	w := int((pr + 2048) * 32 % 4096)
	a.idx = cx*33 + s
	return (int32(a.t[a.idx])*int32(4096-w) + int32(a.t[a.idx+1])*int32(w)) >> 16
}

func (a *apm) update(bit int, rate int) {
	if bit == 1 {
		a.t[a.idx] += uint16((65535 - int32(a.t[a.idx])) >> uint(rate))
	} else {
		a.t[a.idx] -= uint16(int32(a.t[a.idx]) >> uint(rate))
	}
}

// ---------------- 上下文混合编解码器 ----------------
const (
	cmNM = 7 // 上下文模型数: order-0,1,2,3,4,5,6
	cmNI = 9 // 混流输入: 7 模型 + 匹配模型 + 偏置
)

var cmOrderList = [cmNM]int{0, 1, 2, 3, 4, 5, 6}
var cmTblBits = [cmNM]int{16, 20, 22, 22, 22, 22, 22}

type cmCodec struct {
	tbls  [cmNM]*counterTbl
	cx    [cmNM]uint32
	match *counterTbl
	w     []int32
	hist  []byte
	// 匹配模型
	mht      []int32
	matchPtr int32
	matchLen int
	// 逐 bit 状态
	c0   uint32
	bpos int
	// 每个 bit 的中间结果缓存: predict 算好给 update 复用。
	// 原实现 update 会把 7 个上下文哈希重算一遍(每 bit 14 次 -> 7 次),
	// 且 st 数组按值返回/传参, 每 bit 多拷两趟 36B。
	// 纯实现优化: 数值路径不变, 压缩输出逐字节一致。
	idx [cmNM]uint32
	st  [cmNI]int32
	// SSE
	apm1 *apm
	apm2 *apm

	enc *arEnc
	dec *arDec
}

func newCMCodec() *cmCodec {
	initCMTables()
	initRcp()
	c := &cmCodec{}
	for i := 0; i < cmNM; i++ {
		c.tbls[i] = newCounterTbl(cmTblBits[i])
	}
	c.match = newCounterTbl(20)
	// 混合器按 bit 位置(bpos 0..7)分组: 每组独立权重(更贴合当前 bit 的上下文)
	c.w = make([]int32, cmNI*8)
	for i := range c.w {
		c.w[i] = (1 << 16) / cmNI
	}
	c.mht = make([]int32, 1<<22)
	for i := range c.mht {
		c.mht[i] = -1
	}
	c.apm1 = newAPM(256)
	c.apm2 = newAPM(1 << 16)
	c.hist = make([]byte, 0, 1<<20)
	c.matchPtr = -1
	return c
}

// 每个模型的表大小掩码预计算好, 免得每 bit 每模型都重算一次移位
var cmMask = func() [cmNM]uint32 {
	var m [cmNM]uint32
	for i, b := range cmTblBits {
		m[i] = uint32((1 << uint(b)) - 1)
	}
	return m
}()

func ctxIdx(cx uint32, i int, c0 uint32) uint32 {
	h := cx*2654435761 + 0x9e3779b9
	h ^= h >> 15
	h *= 2246822519
	h ^= h >> 13
	return (h + c0*0x7feb352d) & cmMask[i]
}

// 返回: 最终 12-bit P(1), 混合器自身 12-bit P(1)(用于训练权重)。
// 上下文索引与各模型 stretch 值缓存在 c.idx / c.st, 供 update 复用。
func (c *cmCodec) predict() (uint32, uint32) {
	st := c.st[:]
	for i := 0; i < cmNM; i++ {
		idx := ctxIdx(c.cx[i], i, c.c0)
		c.idx[i] = idx
		st[i] = stretchT[c.tbls[i].p16(idx)>>4]
	}
	// 匹配模型
	st[cmNM] = 0
	if c.matchPtr >= 0 && c.matchLen > 0 {
		mp := int(c.matchPtr)
		if mp < len(c.hist) {
			pb := c.hist[mp]
			predBit := int((pb >> uint(7-c.bpos)) & 1)
			conf := int32(c.matchLen)
			if conf > 28 {
				conf = 28
			}
			if predBit == 1 {
				st[cmNM] = conf * 110
			} else {
				st[cmNM] = -conf * 110
			}
		}
	}
	st[cmNM+1] = 256 // 偏置

	wb := c.bpos * cmNI
	var dot int32
	for i := 0; i < cmNI; i++ {
		dot += (c.w[wb+i] * st[i]) >> 16
	}
	if dot > 2047 {
		dot = 2047
	} else if dot < -2047 {
		dot = -2047
	}
	pMix := squash(dot) // 混合器自身输出(12-bit), 用于训练权重
	// 两级 APM/SSE 校正
	a1 := c.apm1.pp(dot, int(c.c0&0xff))
	a2 := c.apm2.pp(dot, int((c.cx[1]&0xff)<<8|(c.c0&0xff)))
	p12 := (a1 + a2*3) >> 2
	if p12 < 1 {
		p12 = 1
	} else if p12 > 4094 {
		p12 = 4094
	}
	return uint32(p12), uint32(pMix)
}

func (c *cmCodec) update(bit int, pMix uint32) {
	st := c.st[:]
	for i := 0; i < cmNM; i++ {
		c.tbls[i].update(c.idx[i], bit)
	}
	target := int32(4096)
	if bit == 0 {
		target = 0
	}
	// 用"混合器自身输出"的误差训练权重(lpaq 做法); 原用 APM 输出误差是错的
	err := target - int32(pMix)
	wb := c.bpos * cmNI
	for i := 0; i < cmNI; i++ {
		c.w[wb+i] += (st[i] * err) >> 13
	}
	c.apm1.update(bit, 7)
	c.apm2.update(bit, 7)
	// 匹配模型: 若本 bit 与预测不符, 则本字节内停用(避免误导后续 bit)
	if c.matchLen > 0 && c.matchPtr >= 0 && int(c.matchPtr) < len(c.hist) {
		pb := c.hist[c.matchPtr]
		if int((pb>>uint(7-c.bpos))&1) != bit {
			c.matchLen = 0
		}
	}
	c.c0 = (c.c0 << 1) | uint32(bit)
	c.bpos++
}

func (c *cmCodec) byteDone(b byte) {
	c.hist = append(c.hist, b)
	for i := 0; i < cmNM; i++ {
		k := cmOrderList[i]
		if k == 0 {
			continue
		}
		c.cx[i] = ((c.cx[i] << 8) | uint32(b)) & ((1 << uint(8*k)) - 1)
	}
	c.c0 = 1
	c.bpos = 0
	if len(c.hist) >= 4 {
		h := uint32(c.hist[len(c.hist)-4]) | uint32(c.hist[len(c.hist)-3])<<8 |
			uint32(c.hist[len(c.hist)-2])<<16 | uint32(c.hist[len(c.hist)-1])<<24
		h = (h * 2654435761) >> 10
		h &= (1 << 22) - 1
		if c.matchLen > 0 {
			c.matchPtr++
			c.matchLen++
			if int(c.matchPtr) >= len(c.hist) {
				c.matchLen = 0
			}
		} else {
			prev := c.mht[h]
			if prev >= 0 && int(prev) < len(c.hist) {
				ml := 0
				for ml < 32 && int(prev)-ml >= 0 && len(c.hist)-1-ml >= 0 &&
					c.hist[int(prev)-ml] == c.hist[len(c.hist)-1-ml] {
					ml++
				}
				if ml >= 4 {
					c.matchPtr = prev + 1
					c.matchLen = ml
					if int(c.matchPtr) >= len(c.hist) {
						c.matchLen = 0
					}
				} else {
					c.matchLen = 0
				}
			} else {
				c.matchLen = 0
			}
		}
		c.mht[h] = int32(len(c.hist) - 1)
	}
}

func cmCompress(data []byte) []byte {
	c := newCMCodec()
	c.enc = &arEnc{}
	c.enc.init()
	c.c0 = 1
	for _, b := range data {
		for i := 7; i >= 0; i-- {
			bit := int((b >> uint(i)) & 1)
			p, pm := c.predict()
			c.enc.encode(bit, p)
			c.update(bit, pm)
		}
		c.byteDone(b)
	}
	c.enc.flush()
	out := make([]byte, 0, len(c.enc.out)+9)
	var hdr [9]byte
	hdr[0] = 1
	binary.LittleEndian.PutUint64(hdr[1:], uint64(len(data)))
	out = append(out, hdr[:]...)
	out = append(out, c.enc.out...)
	return out
}

// cmDecompressTo 把解压结果顺序写入 w(而非整份返回)。
// CM 是逐 bit 串行解码, 无法随机访问, 但输出天然是流式的 —— 写 w 即可让解压
// 不再要求"输出全量常驻内存"(解包侧落临时文件所需)。
func cmDecompressTo(w io.Writer, frame []byte, maxOut int64) error {
	if len(frame) < 9 || frame[0] != 1 {
		fatal("cm 帧格式错误")
	}
	n := binary.LittleEndian.Uint64(frame[1:9])
	// 帧头这 8 字节是**归档里给的**。CM 逐 bit 解码没有天然的结束标记 ——
	// 解码器读完输入后会一直补 0, 唯一的终止条件就是这个长度。于是它被改坏成
	// 天文数字时, 解码器会一直跑到地老天荒: 实测一个损坏的 text 归档能让
	// `hcax list` 永久挂死(单核跑满, 调用栈停在 cmCodec.update)。
	// zstd/lzma2 有流结束标记, 不会这样 —— 只有 CM 需要这个兜底。
	limit := maxOut
	if limit <= 0 {
		limit = 1 << 31 // 调用方给不出精确期望值时(老格式)的宽松兜底
	}
	if n > uint64(limit) {
		return fmt.Errorf("cm 帧声明 %d 字节, 超过上限 %d(归档已损坏)", n, limit)
	}
	if n == 0 {
		return nil
	}
	c := newCMCodec()
	c.dec = &arDec{}
	c.dec.init(frame[9:])
	c.c0 = 1
	var buf [64 << 10]byte
	bn := 0
	for j := uint64(0); j < n; j++ {
		var b byte
		for i := 7; i >= 0; i-- {
			p, pm := c.predict()
			bit := c.dec.decode(p)
			c.update(bit, pm)
			b = (b << 1) | byte(bit)
		}
		buf[bn] = b
		bn++
		if bn == len(buf) {
			if _, err := w.Write(buf[:bn]); err != nil {
				return err
			}
			bn = 0
		}
		c.byteDone(b)
	}
	if bn > 0 {
		_, err := w.Write(buf[:bn])
		return err
	}
	return nil
}

// maxOut: 调用方已知的期望输出上限(元数据有 metaRawLen, 固实流有各块 uncomp 之和)。
// CM 帧头的长度字段不可信, 没有它解码器会跑到地老天荒 —— 详见 cmDecompressTo。
func cmDecompress(frame []byte, maxOut int64) []byte {
	var out bytes.Buffer
	if err := cmDecompressTo(&out, frame, maxOut); err != nil {
		fatal("cm 解压: %v", err)
	}
	return out.Bytes()
}
