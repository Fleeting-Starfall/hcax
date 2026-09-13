package main

// 单元测试: 锁住那些"改坏了会静默损坏数据"的纯函数。
// 端到端的往返测试(见 test/regress.sh)跑一遍要几十秒, 而且一旦失败很难定位;
// 这些用例毫秒级就能指出是哪个变换/哪条规则坏了。
//
//   go test ./...

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 预处理变换必须可逆(否则解包就是静默的数据损坏) ----------

func TestDeltaRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, d := range []int{1, 2, 3, 4} {
		src := make([]byte, 4096)
		rng.Read(src)
		enc := deltaXform(src, d, false)
		dec := deltaXform(enc, d, true)
		if !bytes.Equal(dec, src) {
			t.Fatalf("delta d=%d 不可逆", d)
		}
	}
}

func TestBCJRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	src := make([]byte, 8192)
	rng.Read(src)
	// 人为埋一些 E8/E9/0F8x 指令前缀, 保证真的走到变换分支
	for i := 0; i < 200; i++ {
		src[rng.Intn(len(src)-8)] = 0xE8
		src[rng.Intn(len(src)-8)] = 0x0F
	}
	enc := bcjX86(src, false)
	dec := bcjX86(enc, true)
	if !bytes.Equal(dec, src) {
		t.Fatal("bcj-x86 不可逆")
	}
}

func TestApplyXformRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, xf := range []byte{xfNone, xfDelta1, xfDelta2, xfDelta3, xfDelta4, xfBCJX86} {
		src := make([]byte, 2048)
		rng.Read(src)
		enc := applyXform(xf, src, false)
		dec := applyXform(xf, enc, true)
		if !bytes.Equal(dec, src) {
			t.Fatalf("xform %d 不可逆", xf)
		}
	}
}

// applyXformInPlace 必须与非 in-place 版本结果一致(chooseTransform 用前者选参,
// 真正落盘用后者; 两者不一致会导致"选了 A 却按 B 编码")
func TestApplyXformInPlaceMatches(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	src := make([]byte, 1024)
	rng.Read(src)
	for _, xf := range []byte{xfNone, xfDelta1, xfDelta2, xfDelta3, xfDelta4, xfBCJX86} {
		a := append([]byte(nil), src...)
		applyXformInPlace(xf, a)
		b := applyXform(xf, src, false)
		if !bytes.Equal(a, b) {
			t.Fatalf("xform %d: in-place 与非 in-place 结果不一致", xf)
		}
	}
}

// ---------- 路径校验: 既要挡住逃逸, 也不能误杀合法文件名 ----------

func TestSafeName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a.txt", true},
		{"dir/a.txt", true},
		{"a..b.txt", true},     // 合法: 连续两个点只是文件名的一部分
		{"..weird.txt", true},  // 合法: Unix 下这就是个普通文件名
		{"a/../b.txt", false},  // 逃逸
		{"../b.txt", false},    // 逃逸
		{"..", false},          // 逃逸
		{"a/..", false},        // 逃逸
		{"/etc/passwd", false}, // 绝对路径
		{"", false},            // 空名
		{`a\..\b.txt`, false},  // Windows 风格逃逸(Unix 上 \ 是合法字符, 但仍要挡)
	}
	for _, c := range cases {
		if got := safeName(c.name); got != c.ok {
			t.Errorf("safeName(%q) = %v, 期望 %v", c.name, got, c.ok)
		}
	}
}

// ---------- 归档根: 必须与 tar/zip 语义一致 ----------

func TestCommonRoot(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"dir"}, "."},                 // 打包一个目录 -> 保留目录名
		{[]string{"dir/a.txt"}, "dir"},         // 打包单个文件 -> 只存文件名
		{[]string{"a.txt", "b.txt"}, "."},      //
		{[]string{"d1", "d2"}, "."},            // 多个顶层目录
		{[]string{"/x/y/z", "/x/y/w"}, "/x/y"}, // 绝对路径: 不能丢掉开头的 /
		{[]string{"/x/y/a.txt", "/x/y/b.txt"}, "/x/y"},
	}
	for _, c := range cases {
		if got := commonRoot(c.in); got != c.want {
			t.Errorf("commonRoot(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 回归: 目录里的文件全在子目录中时, 归档根不能被推得太深
// (src/main/go/main.go 曾存成 main.go, go/ 这一层直接消失)
func TestCommonRootNotTooDeep(t *testing.T) {
	got := commonRoot([]string{"src"})
	if got != "." {
		t.Fatalf("commonRoot([src]) = %q, 期望 \".\"(否则目录结构会被拍平)", got)
	}
}

// ---------- 分块器 ----------

// 同样的输入必须切出同样的块(去重依赖内容寻址: 切法变了, 旧归档就读不对了)
func TestChunkerDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	data := make([]byte, 300000)
	rng.Read(data)

	var a, b [][]byte
	for _, dst := range [](*[][]byte){&a, &b} {
		ch := newChunker(func(c []byte) {
			*dst = append(*dst, append([]byte(nil), c...))
		})
		ch.write(data)
		ch.flush()
	}
	if len(a) != len(b) {
		t.Fatalf("两次分块块数不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("第 %d 块内容不同", i)
		}
	}
}

// 分块必须无损: 所有块拼回来要等于原始输入
func TestChunkerLossless(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for _, n := range []int{0, 1, 100, 8192, 8193, 300000} {
		data := make([]byte, n)
		rng.Read(data)
		var out []byte
		ch := newChunker(func(c []byte) { out = append(out, c...) })
		ch.write(data)
		ch.flush()
		if !bytes.Equal(out, data) {
			t.Fatalf("长度 %d: 分块拼接后与输入不一致", n)
		}
		// 块大小必须在 [chunkMin, chunkMax] 之间(最后一块除外)
		var sizes []int
		ch2 := newChunker(func(c []byte) { sizes = append(sizes, len(c)) })
		ch2.write(data)
		ch2.flush()
		for i, s := range sizes {
			if i < len(sizes)-1 && (s < chunkMin || s > chunkMax) {
				t.Fatalf("长度 %d: 第 %d 块大小 %d 越界", n, i, s)
			}
		}
	}
}

// ---------- 字节熵 ----------

func TestByteEntropy(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	rnd := make([]byte, 65536)
	rng.Read(rnd)
	if e := byteEntropy(rnd); e < 7.9 {
		t.Errorf("随机数据熵 %.3f 偏低(应接近 8)", e)
	}
	same := make([]byte, 65536) // 全 0
	if e := byteEntropy(same); e > 0.01 {
		t.Errorf("常量数据熵 %.3f 偏高(应接近 0)", e)
	}
}

// ---------- CM: 压缩再解压必须还原 ----------

func TestCMRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	src := make([]byte, 20000)
	// 造一段有统计结构的数据(CM 靠预测, 纯随机数据压不动也没意义)
	words := []byte("the quick brown fox jumps over the lazy dog ")
	for i := 0; i < len(src); i++ {
		src[i] = words[rng.Intn(len(words))]
	}
	enc := cmCompress(src)
	dec := cmDecompress(enc)
	if !bytes.Equal(dec, src) {
		t.Fatal("CM 解压结果与原文不一致")
	}
}

// ---------- 光栅变换可逆(文件级, 一旦不可逆整个图片就废了) ----------

func makeTestBMP(w, h, bpp int) []byte {
	rowSize := ((w*bpp/8 + 3) / 4) * 4
	pixSize := rowSize * h
	off := 14 + 40
	b := make([]byte, off+pixSize)
	b[0], b[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(b[2:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[10:], uint32(off))
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], uint32(w))
	binary.LittleEndian.PutUint32(b[22:], uint32(h))
	binary.LittleEndian.PutUint16(b[26:], 1) // planes
	binary.LittleEndian.PutUint16(b[28:], uint16(bpp))
	// 像素填成带梯度的内容(全 0 的话压不压都一样, 测不出问题)
	rng := rand.New(rand.NewSource(99))
	for i := off; i < len(b); i++ {
		b[i] = byte(rng.Intn(256))
	}
	return b
}

func TestRasterRoundTrip(t *testing.T) {
	// 宽度取 13..20: 24bpp 时行填充分别是 3/2/1/0/3/2/1/0 字节,
	// 覆盖"行跨距不是 3 的倍数"(宽%4 ∈ {1,2})这个最容易出错的情形
	for _, bpp := range []int{8, 24, 32} {
		for _, w := range []int{13, 14, 15, 16, 17, 18, 19, 20} {
			src := makeTestBMP(w, 9, bpp)
			for _, xf := range []byte{xfRasterMed, xfRasterRctMed, xfRasterRowMed, xfRasterRowRctMed} {
				buf := append([]byte(nil), src...)
				if !rasterApply(xf, buf, true) {
					continue // 该 bpp 不支持此变换(如灰度图无 RCT)
				}
				if bytes.Equal(buf, src) {
					t.Fatalf("bpp=%d w=%d xform=%d: 正向变换后数据没变(可能是空操作)", bpp, w, xf)
				}
				if !rasterApply(xf, buf, false) {
					t.Fatalf("bpp=%d w=%d xform=%d: 逆变换失败", bpp, w, xf)
				}
				if !bytes.Equal(buf, src) {
					t.Fatalf("bpp=%d w=%d xform=%d: 逆变换后与原文不一致(图片会损坏)", bpp, w, xf)
				}
			}
		}
	}
}

// 逐行变换(8/9)必须保留行尾填充字节原样 —— 填充恒为 0, 被残差化反而变大。
func TestRasterRowAwareKeepsPadding(t *testing.T) {
	src := makeTestBMP(17, 9, 24) // stride=52, 每行 1 字节填充
	g, ok := rasterInfo(src)
	if !ok || g.stride-g.rowBytes != 1 {
		t.Fatalf("构造的 BMP 应当有 1 字节行填充, 实际 stride=%d rowBytes=%d", g.stride, g.rowBytes)
	}
	for _, xf := range []byte{xfRasterRowMed, xfRasterRowRctMed} {
		buf := append([]byte(nil), src...)
		if !rasterApply(xf, buf, true) {
			t.Fatalf("xform=%d: 正向变换失败", xf)
		}
		for y := 0; y < g.rows; y++ {
			p := g.dataOff + y*g.stride + g.rowBytes
			if buf[p] != src[p] {
				t.Fatalf("xform=%d: 第 %d 行填充字节被改动了(%d -> %d)", xf, y, src[p], buf[p])
			}
		}
	}
}

// 逐行变换存在的意义: 宽%4∈{1,2} 的 24bpp BMP 上, 旧的扁平 RCT 会让通道相位逐行漂移,
// MED 的竖直预测因此失效, 结果比"不做色彩去相关"还差一大截。这里锁住这个收益。
func TestRasterRowAwareBeatsFlat(t *testing.T) {
	for _, w := range []int{101, 102} { // pad=1 / pad=2
		src := makePhotoBMP(w, 120, 24)
		sz := func(xf byte) int {
			if xf == xfNone {
				return gateSize(nil, src)
			}
			b := append([]byte(nil), src...)
			if !rasterApply(xf, b, true) {
				t.Fatalf("w=%d xform=%d: 变换失败", w, xf)
			}
			return gateSize(nil, b)
		}
		flat, row := sz(xfRasterRctMed), sz(xfRasterRowRctMed)
		if row >= flat {
			t.Errorf("w=%d: 逐行 RCT(%d) 未优于扁平 RCT(%d)", w, row, flat)
		}
		if row >= sz(xfRasterRowMed) {
			t.Errorf("w=%d: 逐行 RCT(%d) 未优于只做 MED(%d)", w, row, sz(xfRasterRowMed))
		}
		// 关键: 扁平版本被门控淘汰后, 现状最多只能选 MED; 逐行版应当明显更好
		best := flat
		if v := sz(xfRasterRowMed); v < best {
			best = v
		}
		if v := sz(xfNone); v < best {
			best = v
		}
		if float64(row) > float64(best)*0.97 {
			t.Errorf("w=%d: 逐行 RCT(%d) 相对旧路径最优值(%d) 收益不足 3%%", w, row, best)
		}
	}
}

// 照片风格 BMP: 通道间高度相关(R≈G≈B) + 低频渐变 + 少量噪声, 用来测压率而非只测可逆性
func makePhotoBMP(w, h, bpp int) []byte {
	ch := bpp / 8
	if ch < 3 {
		ch = 3
	}
	stride := ((w*ch + 3) / 4) * 4
	off := 14 + 40
	b := make([]byte, off+stride*h)
	b[0], b[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(b[2:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[10:], uint32(off))
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], uint32(w))
	binary.LittleEndian.PutUint32(b[22:], uint32(h))
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], uint16(bpp))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 128 + 60*math.Sin(float64(x)/13+float64(y)/29)*math.Cos(float64(y)/41) +
				20*math.Sin(float64(x)/3+7+float64(y)/5)
			nz := float64((x*31+y*17)%7 - 3)
			o := off + y*stride + x*ch
			b[o] = byte(clampByte(v - 10 + nz/2))
			b[o+1] = byte(clampByte(v + nz))
			b[o+2] = byte(clampByte(v + 8 + nz))
		}
	}
	return b
}

func clampByte(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return int(v)
}

// ---------- 元数据序列化/解析往返 ----------

func TestMetaRoundTrip(t *testing.T) {
	chunks := []*chunkMeta{
		{uncomp: 100, xform: xfDelta3, offset: 0},
		{uncomp: 200, stored: true, offset: 0, dictID: 1},
		{uncomp: 300, xform: xfBCJX86},
	}
	files := []fileEntry{
		{name: "a/b.txt", size: 100, mtime: 12345, mode: 0644, chunks: []uint32{0, 1}},
		{name: "dir", isDir: true, mode: 0755},
		{name: "link", isLink: true, link: "a/b.txt", size: 8},
		{name: "img.bmp", size: 900, mode: 0644, chunks: []uint32{2}, xform: xfRasterRctMed},
	}
	var sh, rh [8]byte
	copy(sh[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	copy(rh[:], []byte{8, 7, 6, 5, 4, 3, 2, 1})
	raw := serializeMeta(chunks, files, sh, rh, 10)

	gotChunks, gotFiles, gotSH, gotRH := parseMeta(raw, uint32(len(chunks)), uint32(len(files)), version)

	if gotSH != sh || gotRH != rh {
		t.Fatal("流级哈希往返不一致")
	}
	if len(gotChunks) != len(chunks) || len(gotFiles) != len(files) {
		t.Fatalf("条目数不符: 块 %d/%d 文件 %d/%d", len(gotChunks), len(chunks), len(gotFiles), len(files))
	}
	for i := range chunks {
		if gotChunks[i].uncomp != chunks[i].uncomp || gotChunks[i].stored != chunks[i].stored ||
			gotChunks[i].xform != chunks[i].xform || (gotChunks[i].dictID != 0) != (chunks[i].dictID != 0) {
			t.Errorf("块 %d 往返不一致: %+v vs %+v", i, gotChunks[i], chunks[i])
		}
	}
	for i := range files {
		g, w := gotFiles[i], files[i]
		if g.name != w.name || g.size != w.size || g.mtime != w.mtime || g.mode != w.mode ||
			g.isDir != w.isDir || g.isLink != w.isLink || g.link != w.link || g.xform != w.xform {
			t.Errorf("文件 %d 往返不一致: %+v vs %+v", i, g, w)
		}
		if len(g.chunks) != len(w.chunks) {
			t.Errorf("文件 %d 块索引数不一致", i)
		}
	}
	// 块偏移: 可压块与原样块各自顺序累加(v6 起不再存偏移)
	var compOff, rawOff uint64
	for i, c := range gotChunks {
		if c.stored {
			if c.offset != rawOff {
				t.Errorf("块 %d 原样偏移 %d 期望 %d", i, c.offset, rawOff)
			}
			rawOff += uint64(c.uncomp)
		} else {
			if c.offset != compOff {
				t.Errorf("块 %d 固实偏移 %d 期望 %d", i, c.offset, compOff)
			}
			compOff += uint64(c.uncomp)
		}
	}
}

// ---------- 版本决策 ----------

// 版本策略是"能不升就不升": 升级意味着旧版二进制读不了, 必须有真理由。
// dictFor 的档位是有实测依据的, 不是随手写的: 多样语料上 4MiB 之后加字典零收益,
// 可执行文件上 8→16MiB 还能再省 4%。这里把档位钉住, 免得以后被"顺手调小"。
func TestDictFor(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1 << 20, "2MiB"},
		{4 << 20, "4MiB"},
		{15 << 20, "4MiB"},
		{16 << 20, "16MiB"}, // 20MB 的 Mach-O 从这里拿到 16MiB, 省 4%
		{500 << 20, "16MiB"},
	}
	for _, c := range cases {
		if got := dictFor(c.n); got != c.want {
			t.Errorf("dictFor(%d) = %s, 期望 %s", c.n, got, c.want)
		}
	}
}

func TestWriteVer(t *testing.T) {
	plain := []fileEntry{{name: "a.txt", mode: 0644, nano: 1700000000_123456789}}
	withLink := append([]fileEntry{}, plain...)
	withLink = append(withLink, fileEntry{name: "l", isLink: true, link: "a.txt"})
	sticky := []fileEntry{{name: "s", mode: uint32(os.ModeSticky) | 0755}}
	future := []fileEntry{{name: "f", mode: 0644, nano: (1 << 33) * int64(1e9)}}
	rowXform := []fileEntry{{name: "p.bmp", mode: 0644, xform: xfRasterRowRctMed}}
	oldXform := []fileEntry{{name: "p.bmp", mode: 0644, xform: xfRasterRctMed}}

	cases := []struct {
		name    string
		files   []fileEntry
		precise bool
		want    byte
	}{
		{"普通文件(哪怕带亚秒时间戳)仍写 v8", plain, false, 8},
		{"含符号链接升 v9", withLink, false, 9},
		{"含 sticky 位升 v10", sticky, false, 10},
		{"2038 之后的时间戳升 v10", future, false, 10},
		{"-T 显式要求纳秒则升 v10", plain, true, 10},
		{"旧的扁平光栅变换(6/7)不升版本", oldXform, false, 8},
		{"逐行光栅变换(8/9)升 v12", rowXform, false, 12},
	}
	for _, c := range cases {
		if got := writeVer(c.files, c.precise); got != c.want {
			t.Errorf("%s: 得到 v%d, 期望 v%d", c.name, got, c.want)
		}
	}
}

// v8 归档必须只落地"老格式装得下"的元数据(权限截到 0777、时间截到秒),
// 否则写出去的字节流老版本按自己的布局解析会错位。
func TestMetaRoundTripV8(t *testing.T) {
	files := []fileEntry{
		{name: "a.txt", size: 5, mtime: 12345, mode: 0644},
		{name: "s", size: 1, mtime: 12345, mode: uint32(os.ModeSticky) | 0755},
	}
	var sh, rh [8]byte
	raw := serializeMeta(nil, files, sh, rh, 8)
	_, got, _, _ := parseMeta(raw, 0, uint32(len(files)), 8)
	if got[0].mtime != 12345 {
		t.Errorf("v8 时间往返不一致: %d", got[0].mtime)
	}
	if got[1].mode != 0755 {
		t.Errorf("v8 应把权限截到 0777, 实际 %o", got[1].mode)
	}
}

// ---------- verify 的逻辑一致性检查 ----------

func TestCheckLogical(t *testing.T) {
	a := &archive{
		chunks: []chunkMeta{{uncomp: 100}, {uncomp: 50}},
		files:  []fileEntry{{name: "f", size: 150, chunks: []uint32{0, 1}}},
	}
	if runCatchingFatal(a.checkLogical) {
		t.Error("一致的归档不该报错")
	}
	a.files[0].size = 149
	if !runCatchingFatal(a.checkLogical) {
		t.Error("size 与块长不符必须报错 —— 否则 verify 会把自相矛盾的归档判为通过")
	}
	// 目录与链接不产生数据块, 不参与这项检查
	a.files = []fileEntry{{name: "d", isDir: true}, {name: "l", isLink: true, link: "x"}}
	if runCatchingFatal(a.checkLogical) {
		t.Error("目录/链接条目不该参与块长校验")
	}
}

// ---------- 恶意/损坏归档: 声明一个巨额块, 不能先吃掉 4GB 内存 ----------

func TestAbsurdChunkSizeRejected(t *testing.T) {
	files := []fileEntry{{name: "a.txt", size: 100, mode: 0644, chunks: []uint32{0}}}
	var sh, rh [8]byte
	// 块表里声明 0xFFFFFFFF(~4GB): 真实数据只有 100 字节
	raw := serializeMeta([]*chunkMeta{{uncomp: 0xFFFFFFFF}}, files, sh, rh, 8)
	if !runCatchingFatal(func() { parseMeta(raw, 1, 1, 8) }) {
		t.Error("声明 4GB 的块没有被拒绝 —— readChunk 会先 make 出 4GB 再发现数据不够")
	}
	// 正常大小必须放行(别把这条检查写成误伤)
	raw2 := serializeMeta([]*chunkMeta{{uncomp: 4096}}, files, sh, rh, 8)
	parseMeta(raw2, 1, 1, 8)
}

// ---------- 打包期间源文件被改写: 归档必须仍自洽 ----------

func TestSizedReadAccountsActualBytes(t *testing.T) {
	unmute := muteStdio(t)
	defer unmute()
	// 没变 -> 原样返回 stat 的大小, 且不报警告
	if got := sizedRead("f", 100, 100); got != 100 {
		t.Errorf("大小没变时应原样返回 100, 实际 %d", got)
	}
	// 变小/变大 -> 按实读记账(归档内部才自洽)
	if got := sizedRead("f", 100, 70); got != 70 {
		t.Errorf("文件变小时应按实读记 70, 实际 %d", got)
	}
	if got := sizedRead("f", 100, 140); got != 140 {
		t.Errorf("文件变大时应按实读记 140, 实际 %d", got)
	}
}

func TestPackSurvivesFileChangingUnderfoot(t *testing.T) {
	// 打包期间源文件被别的过程改写是常态(日志、数据库、正在下载的文件)。
	// 老代码一律按打开时 stat 的大小记账, 于是写出"声明 N 字节、实际 M 字节"
	// 的归档 —— 打包报成功, 要等解包才炸。
	//
	// 这条不断言"竞态一定发生"(那会变成 flaky 用例), 只断言**不变式**:
	// 文件在变也好、没变也好, 打出来的归档都必须自洽(解得开)。
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(src, "growing.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("log line ................\n"), 300000), 0o644); err != nil {
		t.Fatal(err)
	}
	// 后台持续追加, 制造"文件在脚下变化"。必须设上限, 否则几秒就能把盘写满。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			f.Write(bytes.Repeat([]byte("more log ................\n"), 2000))
			f.Close()
			time.Sleep(time.Millisecond)
		}
	}()

	arc := filepath.Join(dir, "a.hcax")
	unmute := muteStdio(t)
	pack([]string{src}, arc, "fast", nil, false)
	unmute()
	close(stop)
	wg.Wait()

	// 不变式: 归档必须解得开(verify=true 会连流级校验一起做)
	out := filepath.Join(dir, "out")
	unmute = muteStdio(t)
	fataled := runCatchingFatal(func() { unpack(arc, out, true, nil) })
	unmute()
	if fataled {
		t.Fatal("源文件在打包期间被改写后, 打出的归档解不开 —— 归档内部不自洽")
	}
}

// ---------- 系统 xz 缺失时必须报警, 不能静默降级 ----------

func TestXZFallbackWarns(t *testing.T) {
	// 系统 xz 不在 PATH 时, max/ultra 会回退到内置的纯 Go lzma2。
	// 这不是"慢一点"而已 —— 同一语料实测 26.89% -> 33.40%, 归档凭空大 24%。
	// 此前这条路径完全静默, 用户以为自己在用最强档。这里锁住"必须报一次警"。
	//
	// exec.LookPath 每次调用都现读 PATH, 把它指向一个空目录就必然找不到 xz,
	// 于是这条用例在"装了 xz / 没装 xz"的机器上都能稳定触发回退分支。
	t.Setenv("PATH", t.TempDir())

	before := xzWarnCount
	xzWarnOnce = sync.Once{} // sync.Once 触发后无法重置, 换一个新的让本用例独立计数
	b := &backend{}
	if out := b.xzCompress(bytes.NewReader([]byte("hello, world")), "3", "2", "2MiB"); out != nil {
		t.Fatal("PATH 里没有 xz, xzCompress 必须返回 nil, 好让上层走到回退实现")
	}
	if xzWarnCount != before+1 {
		t.Errorf("系统 xz 缺失时必须恰好警告一次(实际新增 %d 次)", xzWarnCount-before)
	}
}

// ---------- 截断归档必须"干净失败", 不许 panic ----------

// 静音: 下面要跑几千次解析, 让它们往测试输出里打印毫无意义
func muteStdio(t *testing.T) func() {
	t.Helper()
	dn, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = dn, dn
	return func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		dn.Close()
	}
}

func TestTruncatedArchiveNeverPanics(t *testing.T) {
	// 截断归档是最容易撞出 panic 的输入: 头部说"元数据 N 字节 / 数据 M 字节",
	// 而文件实际没那么长。panic 在"自动处理别人发来的归档"的场景里就是 DoS。
	// 而且这类 bug 往往是"刚好截断到某个长度才崩", 手搓用例撞不到 —— 只能穷举前缀。
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// 三种内容: 可压文本(进固实流) / 随机(判为原样, 进原样区) / 中等重复,
	// 这样归档里"元数据帧 + 数据帧 + 原样区"三段都有实体, 截断才有覆盖意义。
	rng := rand.New(rand.NewSource(20260913))
	words := strings.Fields("the quick brown fox compress data streams context mixing " +
		"predicts bits adaptive model archive solid chunk dedup")
	txt := make([]byte, 0, 24000)
	for len(txt) < 24000 {
		txt = append(txt, words[rng.Intn(len(words))]...)
		txt = append(txt, ' ')
	}
	rnd := make([]byte, 6000)
	rng.Read(rnd)
	mid := bytes.Repeat([]byte("repeat me "), 900)
	for name, data := range map[string][]byte{
		"a.txt": txt, "b.bin": rnd, "c.dat": mid,
	} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 五种模式是**五套不同的编解码实现**(zstd / lzma2-xz / 自写 CM),
	// 只测一种等于只测了三分之一。逐个穷举。
	unmute := muteStdio(t)
	defer unmute()
	for _, mode := range []string{"fast", "best", "max", "ultra", "text"} {
		t.Run(mode, func(t *testing.T) {
			arc := filepath.Join(dir, mode+".hcax")
			pack([]string{src}, arc, mode, nil, false)
			full, err := os.ReadFile(arc)
			if err != nil {
				t.Fatal(err)
			}
			if len(full) < 512 {
				t.Fatalf("归档只有 %d 字节, 穷举前缀测不到东西", len(full))
			}
			// 先验: 完整归档必须能正常列出。否则下面几万轮全是"一开始就失败",
			// 用例看着在跑, 其实一行有效代码都没执行到。
			if runCatchingFatal(func() { listArchive(arc, false) }) {
				t.Fatal("完好归档反而列不出来 —— 后面的穷举全是空转")
			}
			t.Logf("归档 %d 字节, 穷举 %d 个前缀(每 16 个做一次完整解包)", len(full), len(full)+1)
			// runCatchingFatal 把 fatal 转成 panic 接住, 而**真正的 panic 会原样
			// 抛出**, 于是"崩了"直接体现为测试失败 —— 正是想要的效果。
			for n := 0; n <= len(full); n++ {
				p := filepath.Join(dir, "trunc.hcax")
				if err := os.WriteFile(p, full[:n], 0o644); err != nil {
					t.Fatal(err)
				}
				runCatchingFatal(func() { listArchive(p, false) })
				// 解包路径比只读元数据深得多(建文件/解压/变换), 抽样覆盖
				if n%16 == 0 {
					out := filepath.Join(dir, "out")
					os.RemoveAll(out)
					runCatchingFatal(func() { unpack(p, out, false, nil) })
				}
			}
		})
	}
}

// ---------- 随机篡改的归档同样不许 panic ----------

func TestCorruptedArchiveNeverPanics(t *testing.T) {
	// 截断只让数据"变少"; 随机篡改能让长度字段"变大"(比如块长从 100 变成 10 亿),
	// 触发的是另一类越界 —— 光靠截断用例撞不到。
	// 归档里的字节是**别人给的**, 处理它的人不该因为一个坏字节就整个崩掉。
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(4242))
	words := strings.Fields("alpha beta gamma delta archive chunk dedup solid model")
	txt := make([]byte, 0, 20000)
	for len(txt) < 20000 {
		txt = append(txt, words[rng.Intn(len(words))]...)
		txt = append(txt, ' ')
	}
	rnd := make([]byte, 4000)
	rng.Read(rnd)
	for name, data := range map[string][]byte{
		"a.txt": txt, "b.bin": rnd, "c.dat": bytes.Repeat([]byte("xyz "), 800),
	} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	unmute := muteStdio(t)
	defer unmute()
	for _, mode := range []string{"fast", "best", "max", "ultra", "text"} {
		t.Run(mode, func(t *testing.T) {
			arc := filepath.Join(dir, mode+".hcax")
			pack([]string{src}, arc, mode, nil, false)
			full, err := os.ReadFile(arc)
			if err != nil {
				t.Fatal(err)
			}
			// 先验: 同 TestTruncatedArchiveNeverPanics, 防止 200 轮全是空转
			if runCatchingFatal(func() { listArchive(arc, false) }) {
				t.Fatal("完好归档反而列不出来 —— 后面的篡改用例全是空转")
			}
			const rounds = 200
			for i := 0; i < rounds; i++ {
				b := append([]byte(nil), full...)
				for k, n := 0, 1+rng.Intn(3); k < n; k++ { // 每轮翻 1~3 个 bit
					b[rng.Intn(len(b))] ^= byte(1 << uint(rng.Intn(8)))
				}
				p := filepath.Join(dir, "corrupt.hcax")
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
				runCatchingFatal(func() { listArchive(p, false) })
				if i%8 == 0 {
					out := filepath.Join(dir, "out")
					os.RemoveAll(out)
					runCatchingFatal(func() { unpack(p, out, false, nil) })
				}
			}
		})
	}
}

// ---------- 归档落盘必须原子: 失败时不能把旧归档一起毁掉 ----------

func TestArchiveWriterKeepsOldOnFailure(t *testing.T) {
	// 老代码 os.Create(outPath) 一打开就把旧归档截成 0。打包跑到最后写盘阶段
	// 失败(磁盘满)时, 旧备份已经没了, 新的只写了一半 —— 一次失败毁两份。
	dir := t.TempDir()
	out := filepath.Join(dir, "backup.hcax")
	const old = "上一版的好归档"
	if err := os.WriteFile(out, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	w := openArchiveForWrite(out)
	w.f.Write([]byte("写了一半就崩了")) // 模拟写到一半失败
	w.abort()                       // fatal() 走的就是这一步

	got, err := os.ReadFile(out)
	if err != nil || string(got) != old {
		t.Errorf("写盘失败后旧归档被毁了: %q (err=%v)", got, err)
	}
	// 半成品临时文件不能留在目录里当垃圾
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("目录里还剩 %d 个文件, 半成品没清理干净", len(ents))
	}
}

func TestArchiveWriterCommitsAtomically(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "a.hcax")
	if err := os.WriteFile(out, []byte("旧"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := openArchiveForWrite(out)
	w.f.Write([]byte("新内容"))
	w.f.Close()
	w.commit()

	got, _ := os.ReadFile(out)
	if string(got) != "新内容" {
		t.Errorf("commit 后目标归档内容不对: %q", got)
	}
	// 覆盖已有归档时要沿用它的权限, 别顺手把 0600 改成 0644
	fi, _ := os.Stat(out)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("覆盖了已有归档却改了它的权限: %v(应为 0600)", fi.Mode().Perm())
	}
	// tmp 置空后 abort 必须是空操作(清理钩子会无条件再调一次)
	w.abort()
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("commit 后又多出文件: %d 个", len(ents))
	}
}

func TestArchiveWriterNewFilePerm(t *testing.T) {
	// 新建的归档如果留着 CreateTemp 的 0600, 备份出来别人读不了
	dir := t.TempDir()
	out := filepath.Join(dir, "new.hcax")
	w := openArchiveForWrite(out)
	w.f.Write([]byte("x"))
	w.f.Close()
	w.commit()
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("新建归档权限 %v, 应为 0644", fi.Mode().Perm())
	}
}

// ---------- 系统 xz 的 stderr 绝不能混进压缩流 ----------

func TestXZStderrNotMixedIntoOutput(t *testing.T) {
	// 构造一个"往 stderr 打一条警告、但照常完成压缩"的 xz。若把 cmd.Stderr 接到
	// 与 stdout 同一个 buffer, 这条警告会拼进压缩流头部 —— 打包成功、归档大几十
	// 字节, 解包时才报 invalid header magic bytes。数据损坏是静默发生的。
	real, err := exec.LookPath("xz")
	if err != nil {
		t.Skip("本机没有系统 xz(或不在 PATH), 这条用例需要一个真实的 xz 来转发")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "xz")
	script := "#!/bin/sh\necho 'xz: (模拟) 一条无害的警告' >&2\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	b := &backend{}
	out := b.xzCompress(bytes.NewReader([]byte("hello, world, hello, world")), "3", "2", "2MiB")
	if out == nil {
		t.Fatal("xz 明明成功了, 不该返回 nil")
	}
	const xzMagic = "\xFD" + "7zXZ" + "\x00"
	if len(out) < 6 || string(out[:6]) != xzMagic {
		t.Errorf("压缩流开头不是 xz magic, stderr 混进来了: %q", out[:min(len(out), 32)])
	}
}
