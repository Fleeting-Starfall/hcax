package main

// 老版本(v2~v5)归档的读取分支: 所有现存测试打的、解的**全是 v6+ 归档**,
// 那段 90 行的兼容代码从没被执行过一次 —— 它坏掉也无人知晓。
// 这里按 FORMAT.md 手工拼出 v3 / v5 归档的字节流, 直接喂给解包路径。
// 顺带也把 FORMAT.md 里记的布局"用代码对一遍"。

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type legacyFile struct {
	name string
	data []byte
	mode uint32
}

// 造一个 v3(无字典区)或 v5(有字典区, 本例字典数为 0)归档。
// 布局: [头32B][数据区][原样区][块表][文件表][(v5)字典区][尾部20B]
func buildLegacyArchive(t *testing.T, ver byte, comp, rawRegion []byte, files []legacyFile, chunks []chunkMeta) []byte {
	t.Helper()
	var ctEntry int
	switch {
	case ver >= 5:
		ctEntry = 31
	case ver == 4:
		ctEntry = 30
	case ver == 3:
		ctEntry = 29
	default:
		ctEntry = 28
	}
	var b bytes.Buffer

	hdr := make([]byte, 32)
	copy(hdr[0:4], magic)
	hdr[4] = ver
	hdr[5] = 0 // mode code 0 = fast(zstd)
	hdr[6] = 0 // window
	hdr[7] = 0 // flag
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(len(comp)))
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(len(rawRegion)))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(chunks)))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(len(files)))
	b.Write(hdr)
	b.Write(comp)
	b.Write(rawRegion)

	// 块表: hash16 + offset + uncomp + stored[+xform(v4+)][+dictID(v5+)]
	for _, c := range chunks {
		e := make([]byte, ctEntry)
		copy(e[0:16], c.hash[:])
		binary.LittleEndian.PutUint64(e[16:24], c.offset)
		binary.LittleEndian.PutUint32(e[24:28], c.uncomp)
		if c.stored {
			e[28] = 1
		}
		if ver >= 4 {
			e[29] = c.xform
		}
		if ver >= 5 {
			e[30] = c.dictID
		}
		b.Write(e)
	}

	// 文件表: nameLen + name + size + mtime + mode + nChunks + chunks
	var total uint64
	for i, f := range files {
		nb := []byte(f.name)
		binary.Write(&b, binary.LittleEndian, uint16(len(nb)))
		b.Write(nb)
		binary.Write(&b, binary.LittleEndian, uint64(len(f.data)))
		binary.Write(&b, binary.LittleEndian, uint64(1700000000))
		binary.Write(&b, binary.LittleEndian, f.mode)
		binary.Write(&b, binary.LittleEndian, uint32(1)) // 每个文件 1 个块
		binary.Write(&b, binary.LittleEndian, uint32(i))
		total += uint64(len(f.data))
	}

	if ver == 5 { // v5 必有字典区; 本例字典数为 0
		binary.Write(&b, binary.LittleEndian, uint32(0))
	}
	b.WriteString(trailerMag)
	binary.Write(&b, binary.LittleEndian, total)
	binary.Write(&b, binary.LittleEndian, uint32(len(files)))
	binary.Write(&b, binary.LittleEndian, uint32(len(chunks)))
	return b.Bytes()
}

func TestReadLegacyArchive(t *testing.T) {
	// 两个文件: 一个走压缩块(可压文本), 一个走原样区(随机数据)
	text := bytes.Repeat([]byte("hcax legacy format round-trip test. "), 60)
	rnd := make([]byte, 700)
	for i := range rnd {
		rnd[i] = byte(i*7 + i/3)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	if err != nil {
		t.Fatal(err)
	}
	comp := enc.EncodeAll(text, nil)
	enc.Close()

	for _, ver := range []byte{3, 5} {
		h1 := hash16(text)
		h2 := hash16(rnd)
		chunks := []chunkMeta{
			{hash: h1, uncomp: uint32(len(text)), offset: 0, stored: false},
			{hash: h2, uncomp: uint32(len(rnd)), offset: 0, stored: true},
		}
		files := []legacyFile{{name: "a.txt", data: text, mode: 0644}, {name: "b.bin", data: rnd, mode: 0600}}
		blob := buildLegacyArchive(t, ver, comp, rnd, files, chunks)

		dir := t.TempDir()
		arc := filepath.Join(dir, "legacy.hcax")
		if err := os.WriteFile(arc, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		// list 能读出来
		if runCatchingFatal(func() { listArchive(arc, false) }) {
			t.Fatalf("v%d: list 失败", ver)
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, true, nil) }) {
			t.Fatalf("v%d: 解包失败", ver)
		}
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(out, f.name))
			if err != nil {
				t.Fatalf("v%d: 读回 %s: %v", ver, f.name, err)
			}
			if !bytes.Equal(got, f.data) {
				t.Errorf("v%d: %s 内容不符(%d vs %d 字节)", ver, f.name, len(got), len(f.data))
			}
		}
	}
}

// 老版本靠"逐块 16B 哈希"校验(v6 起改成流级)。把块内容改坏必须被抓到。
func TestLegacyChunkHashChecked(t *testing.T) {
	text := bytes.Repeat([]byte("legacy hash check. "), 50)
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	comp := enc.EncodeAll(text, nil)
	enc.Close()

	h := hash16(text)
	h[0] ^= 0xff // 故意写错哈希
	chunks := []chunkMeta{{hash: h, uncomp: uint32(len(text)), offset: 0}}
	files := []legacyFile{{name: "a.txt", data: text, mode: 0644}}
	blob := buildLegacyArchive(t, 3, comp, nil, files, chunks)

	dir := t.TempDir()
	arc := filepath.Join(dir, "bad.hcax")
	os.WriteFile(arc, blob, 0o644)
	// 块哈希不匹配 -> 必须干净报错, 而不是解出错误数据
	if !runCatchingFatal(func() { unpack(arc, filepath.Join(dir, "out"), true, nil) }) {
		t.Error("块哈希不符却没有报错 —— 老版本归档的校验形同虚设")
	}
}
