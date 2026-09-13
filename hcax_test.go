package main

// 单元测试: 锁住那些"改坏了会静默损坏数据"的纯函数。
// 端到端的往返测试(见 test/regress.sh)跑一遍要几十秒, 而且一旦失败很难定位;
// 这些用例毫秒级就能指出是哪个变换/哪条规则坏了。
//
//   go test ./...

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"testing"
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
	for _, bpp := range []int{8, 24, 32} {
		src := makeTestBMP(17, 9, bpp)
		for _, xf := range []byte{xfRasterMed, xfRasterRctMed} {
			buf := append([]byte(nil), src...)
			if !rasterApply(xf, buf, true) {
				continue // 该 bpp 不支持此变换(如灰度图无 RCT)
			}
			if bytes.Equal(buf, src) {
				t.Fatalf("bpp=%d xform=%d: 正向变换后数据没变(可能是空操作)", bpp, xf)
			}
			if !rasterApply(xf, buf, false) {
				t.Fatalf("bpp=%d xform=%d: 逆变换失败", bpp, xf)
			}
			if !bytes.Equal(buf, src) {
				t.Fatalf("bpp=%d xform=%d: 逆变换后与原文不一致(图片会损坏)", bpp, xf)
			}
		}
	}
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
func TestWriteVer(t *testing.T) {
	plain := []fileEntry{{name: "a.txt", mode: 0644, nano: 1700000000_123456789}}
	withLink := append([]fileEntry{}, plain...)
	withLink = append(withLink, fileEntry{name: "l", isLink: true, link: "a.txt"})
	sticky := []fileEntry{{name: "s", mode: uint32(os.ModeSticky) | 0755}}
	future := []fileEntry{{name: "f", mode: 0644, nano: (1 << 33) * int64(1e9)}}

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
