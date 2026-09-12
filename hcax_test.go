package main

// 单元测试: 锁住那些"改坏了会静默损坏数据"的纯函数。
// 端到端的往返测试(见 test/regress.sh)跑一遍要几十秒, 而且一旦失败很难定位;
// 这些用例毫秒级就能指出是哪个变换/哪条规则坏了。
//
//   go test ./...

import (
	"bytes"
	"math/rand"
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
		{"a..b.txt", true},      // 合法: 连续两个点只是文件名的一部分
		{"..weird.txt", true},   // 合法: Unix 下这就是个普通文件名
		{"a/../b.txt", false},   // 逃逸
		{"../b.txt", false},     // 逃逸
		{"..", false},           // 逃逸
		{"a/..", false},         // 逃逸
		{"/etc/passwd", false},  // 绝对路径
		{"", false},             // 空名
		{`a\..\b.txt`, false},   // Windows 风格逃逸(Unix 上 \ 是合法字符, 但仍要挡)
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
		{[]string{"dir"}, "."},                        // 打包一个目录 -> 保留目录名
		{[]string{"dir/a.txt"}, "dir"},                // 打包单个文件 -> 只存文件名
		{[]string{"a.txt", "b.txt"}, "."},             //
		{[]string{"d1", "d2"}, "."},                   // 多个顶层目录
		{[]string{"/x/y/z", "/x/y/w"}, "/x/y"},        // 绝对路径: 不能丢掉开头的 /
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
