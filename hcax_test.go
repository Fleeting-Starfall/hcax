package main

// Unit tests: pin down the pure functions whose breakage silently corrupts data.
// End-to-end round trips (test/regress.sh) take tens of seconds and are hard to debug;
// these cases point at the broken transform/rule in milliseconds.
//
//   go test ./...

import (
	"bytes"
	"encoding/binary"
	"io"
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

// ---------- preprocess transforms must be invertible (else unpack silently corrupts) ----------

func TestDeltaRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, d := range []int{1, 2, 3, 4} {
		src := make([]byte, 4096)
		rng.Read(src)
		enc := deltaXform(src, d, false)
		dec := deltaXform(enc, d, true)
		if !bytes.Equal(dec, src) {
			t.Fatalf("delta d=%d not invertible", d)
		}
	}
}

func TestBCJRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	src := make([]byte, 8192)
	rng.Read(src)
	// plant E8/E9/0F8x opcode prefixes so the transform branch is really taken
	for i := 0; i < 200; i++ {
		src[rng.Intn(len(src)-8)] = 0xE8
		src[rng.Intn(len(src)-8)] = 0x0F
	}
	enc := bcjX86(src, false)
	dec := bcjX86(enc, true)
	if !bytes.Equal(dec, src) {
		t.Fatal("bcj-x86 not invertible")
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
			t.Fatalf("xform %d not invertible", xf)
		}
	}
}

// applyXformInPlace must match the non-in-place variant (chooseTransform uses the
// former to pick params, real writes use the latter; mismatch means "picked A, encoded B")
func TestApplyXformInPlaceMatches(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	src := make([]byte, 1024)
	rng.Read(src)
	for _, xf := range []byte{xfNone, xfDelta1, xfDelta2, xfDelta3, xfDelta4, xfBCJX86} {
		a := append([]byte(nil), src...)
		applyXformInPlace(xf, a)
		b := applyXform(xf, src, false)
		if !bytes.Equal(a, b) {
			t.Fatalf("xform %d: in-place and non-in-place results differ", xf)
		}
	}
}

// ---------- path validation: block escapes without killing legitimate names ----------

func TestSafeName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a.txt", true},
		{"dir/a.txt", true},
		{"a..b.txt", true},     // legit: consecutive dots are just part of a name
		{"..weird.txt", true},  // legit: a plain file name on Unix
		{"a/../b.txt", false},  // escape
		{"../b.txt", false},    // escape
		{"..", false},          // escape
		{"a/..", false},        // escape
		{"/etc/passwd", false}, // absolute path
		{"", false},            // empty name
		{`a\..\b.txt`, false},  // Windows-style escape (\\ is legal on Unix, still blocked)
	}
	for _, c := range cases {
		if got := safeName(c.name); got != c.ok {
			t.Errorf("safeName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

// ---------- archive root: must match tar/zip semantics ----------

func TestCommonRoot(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"dir"}, "."},                 // packing one dir -> keep the dir name
		{[]string{"dir/a.txt"}, "dir"},         // packing one file -> store the file name only
		{[]string{"a.txt", "b.txt"}, "."},      //
		{[]string{"d1", "d2"}, "."},            // multiple top-level dirs
		{[]string{"/x/y/z", "/x/y/w"}, "/x/y"}, // absolute paths: keep the leading /
		{[]string{"/x/y/a.txt", "/x/y/b.txt"}, "/x/y"},
	}
	for _, c := range cases {
		if got := commonRoot(c.in); got != c.want {
			t.Errorf("commonRoot(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// regression: with all files inside one subdir, the archive root must not be pushed too deep
// (src/main/go/main.go used to be stored as main.go, the go/ level vanished)
func TestCommonRootNotTooDeep(t *testing.T) {
	got := commonRoot([]string{"src"})
	if got != "." {
		t.Fatalf("commonRoot([src]) = %q, want \".\" (else the dir tree flattens)", got)
	}
}

// ---------- chunker ----------

// identical input must chunk identically (dedup relies on content addressing:
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
		t.Fatalf("two chunkings differ in count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("chunk %d content differs", i)
		}
	}
}

// chunking must be lossless: reassembled chunks equal the input
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
			t.Fatalf("len %d: reassembled chunks differ from input", n)
		}
		// chunk size must stay within [chunkMin, chunkMax] (last chunk excepted)
		var sizes []int
		ch2 := newChunker(func(c []byte) { sizes = append(sizes, len(c)) })
		ch2.write(data)
		ch2.flush()
		for i, s := range sizes {
			if i < len(sizes)-1 && (s < chunkMin || s > chunkMax) {
				t.Fatalf("len %d: chunk %d size %d out of range", n, i, s)
			}
		}
	}
}

// ---------- byte entropy ----------

func TestByteEntropy(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	rnd := make([]byte, 65536)
	rng.Read(rnd)
	if e := byteEntropy(rnd); e < 7.9 {
		t.Errorf("random data entropy %.3f too low (want ~8)", e)
	}
	same := make([]byte, 65536) // all zero
	if e := byteEntropy(same); e > 0.01 {
		t.Errorf("constant data entropy %.3f too high (want ~0)", e)
	}
}

// ---------- CM: compress then decompress must restore ----------

func TestCMRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	src := make([]byte, 20000)
	// build statistically structured data (CM predicts; pure random neither compresses nor tests)
	words := []byte("the quick brown fox jumps over the lazy dog ")
	for i := 0; i < len(src); i++ {
		src[i] = words[rng.Intn(len(words))]
	}
	enc := cmCompress(src)
	dec := cmDecompress(enc, int64(len(src)))
	if !bytes.Equal(dec, src) {
		t.Fatal("CM decompressed output differs from input")
	}
}

// the 8-byte length in the CM frame header is **untrusted** (arbitrary when tampered),
// and CM decodes bit by bit with no natural end marker -- after input it keeps padding 0,
// so without this cap a corrupt text archive hangs hcax list forever (observed: one core
// pegged, stack stuck in cmCodec.update).
func TestCMRejectsAbsurdDeclaredLength(t *testing.T) {
	src := bytes.Repeat([]byte("abcdefgh"), 64)
	enc := cmCompress(src)
	// set the frame header length to an astronomic number
	binary.LittleEndian.PutUint64(enc[1:9], ^uint64(0)>>4)
	// normal cap: must **error out**, not try to decode 2^60 bytes
	if err := cmDecompressTo(io.Discard, enc, int64(len(src))); err == nil {
		t.Fatal("CM frame claiming astronomic length was accepted -- decoder would run forever")
	}
	// reverse check: an intact frame with the right cap must decode fine (no false positive)
	good := cmCompress(src)
	var out bytes.Buffer
	if err := cmDecompressTo(&out, good, int64(len(src))); err != nil {
		t.Fatalf("intact frame wrongly rejected: %v", err)
	}
	if !bytes.Equal(out.Bytes(), src) {
		t.Fatal("intact frame decoded to wrong content")
	}
}

// ---------- raster transforms invertible (file-level; one bad inversion ruins the image) ----------

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
	// fill pixels with a gradient (all-zero would compress identically and prove nothing)
	rng := rand.New(rand.NewSource(99))
	for i := off; i < len(b); i++ {
		b[i] = byte(rng.Intn(256))
	}
	return b
}

func TestRasterRoundTrip(t *testing.T) {
	// widths 13..20: at 24bpp the row paddings are 3/2/1/0/3/2/1/0 bytes,
	// covering the error-prone "row stride not a multiple of 3" (width%%4 in {1,2})
	for _, bpp := range []int{8, 24, 32} {
		for _, w := range []int{13, 14, 15, 16, 17, 18, 19, 20} {
			src := makeTestBMP(w, 9, bpp)
			for _, xf := range []byte{xfRasterMed, xfRasterRctMed, xfRasterRowMed, xfRasterRowRctMed} {
				buf := append([]byte(nil), src...)
				if !rasterApply(xf, buf, true) {
					continue // transform unsupported for this bpp (e.g. no RCT for grayscale)
				}
				if bytes.Equal(buf, src) {
					t.Fatalf("bpp=%d w=%d xform=%d: forward transform left data unchanged (no-op?)", bpp, w, xf)
				}
				if !rasterApply(xf, buf, false) {
					t.Fatalf("bpp=%d w=%d xform=%d: inverse transform failed", bpp, w, xf)
				}
				if !bytes.Equal(buf, src) {
					t.Fatalf("bpp=%d w=%d xform=%d: inverse differs from input (image would corrupt)", bpp, w, xf)
				}
			}
		}
	}
}

// row transforms (8/9) must keep trailing padding bytes untouched -- padding is always 0,
func TestRasterRowAwareKeepsPadding(t *testing.T) {
	src := makeTestBMP(17, 9, 24) // stride=52, 1 padding byte per row
	g, ok := rasterInfo(src)
	if !ok || g.stride-g.rowBytes != 1 {
		t.Fatalf("test BMP should have 1 padding byte per row, got stride=%d rowBytes=%d", g.stride, g.rowBytes)
	}
	for _, xf := range []byte{xfRasterRowMed, xfRasterRowRctMed} {
		buf := append([]byte(nil), src...)
		if !rasterApply(xf, buf, true) {
			t.Fatalf("xform=%d: forward transform failed", xf)
		}
		for y := 0; y < g.rows; y++ {
			p := g.dataOff + y*g.stride + g.rowBytes
			if buf[p] != src[p] {
				t.Fatalf("xform=%d: padding byte of row %d modified (%d -> %d)", xf, y, src[p], buf[p])
			}
		}
	}
}

// why row transforms exist: on 24bpp BMPs with width%%4 in {1,2}, the old flat RCT
// drifts channel phase per row, killing MED vertical prediction and losing to no decorrelation.
func TestRasterRowAwareBeatsFlat(t *testing.T) {
	for _, w := range []int{101, 102} { // pad=1 / pad=2
		src := makePhotoBMP(w, 120, 24)
		sz := func(xf byte) int {
			if xf == xfNone {
				return gateSize(nil, src)
			}
			b := append([]byte(nil), src...)
			if !rasterApply(xf, b, true) {
				t.Fatalf("w=%d xform=%d: transform failed", w, xf)
			}
			return gateSize(nil, b)
		}
		flat, row := sz(xfRasterRctMed), sz(xfRasterRowRctMed)
		if row >= flat {
			t.Errorf("w=%d: row RCT(%d) not better than flat RCT(%d)", w, row, flat)
		}
		if row >= sz(xfRasterRowMed) {
			t.Errorf("w=%d: row RCT(%d) not better than MED alone(%d)", w, row, sz(xfRasterRowMed))
		}
		// key: once the flat variant is gated out, today can pick at best MED; row should win clearly
		best := flat
		if v := sz(xfRasterRowMed); v < best {
			best = v
		}
		if v := sz(xfNone); v < best {
			best = v
		}
		if float64(row) > float64(best)*0.97 {
			t.Errorf("w=%d: row RCT(%d) gains less than 3%% over old-path best(%d)", w, row, best)
		}
	}
}

// photo-style BMP: highly correlated channels (R~G~B) + low-frequency gradient + a little noise,
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

// ---------- metadata serialize/parse round trip ----------

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
		t.Fatal("stream hash round trip mismatch")
	}
	if len(gotChunks) != len(chunks) || len(gotFiles) != len(files) {
		t.Fatalf("count mismatch: chunks %d/%d files %d/%d", len(gotChunks), len(chunks), len(gotFiles), len(files))
	}
	for i := range chunks {
		if gotChunks[i].uncomp != chunks[i].uncomp || gotChunks[i].stored != chunks[i].stored ||
			gotChunks[i].xform != chunks[i].xform || (gotChunks[i].dictID != 0) != (chunks[i].dictID != 0) {
			t.Errorf("chunk %d round trip mismatch: %+v vs %+v", i, gotChunks[i], chunks[i])
		}
	}
	for i := range files {
		g, w := gotFiles[i], files[i]
		if g.name != w.name || g.size != w.size || g.mtime != w.mtime || g.mode != w.mode ||
			g.isDir != w.isDir || g.isLink != w.isLink || g.link != w.link || g.xform != w.xform {
			t.Errorf("file %d round trip mismatch: %+v vs %+v", i, g, w)
		}
		if len(g.chunks) != len(w.chunks) {
			t.Errorf("file %d chunk index count mismatch", i)
		}
	}
	// chunk offsets: comp and raw regions accumulate separately (not stored since v6)
	var compOff, rawOff uint64
	for i, c := range gotChunks {
		if c.stored {
			if c.offset != rawOff {
				t.Errorf("chunk %d raw offset %d want %d", i, c.offset, rawOff)
			}
			rawOff += uint64(c.uncomp)
		} else {
			if c.offset != compOff {
				t.Errorf("chunk %d comp offset %d want %d", i, c.offset, compOff)
			}
			compOff += uint64(c.uncomp)
		}
	}
}

// ---------- version decisions ----------

// version policy: "bump only when needed" -- a bump strands old binaries, must earn it.
// dictFor tiers are measured, not hand-waved: on mixed corpora, past 4MiB a bigger
// dict gains nothing; executables save 4%% more at 16MiB. Pin the tiers so nobody trims them.
func TestDictFor(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1 << 20, "2MiB"},
		{4 << 20, "4MiB"},
		{15 << 20, "4MiB"},
		{16 << 20, "16MiB"}, // a 20MB Mach-O gets 16MiB here, saving 4%
		{500 << 20, "16MiB"},
	}
	for _, c := range cases {
		if got := dictFor(c.n); got != c.want {
			t.Errorf("dictFor(%d) = %s, want %s", c.n, got, c.want)
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
		{"plain file (even with sub-second mtime) stays v8", plain, false, 8},
		{"symlink bumps to v9", withLink, false, 9},
		{"sticky bit bumps to v10", sticky, false, 10},
		{"post-2038 mtime bumps to v10", future, false, 10},
		{"-T explicitly asks ns, bumps to v10", plain, true, 10},
		{"legacy flat raster transforms (6/7) keep v8", oldXform, false, 8},
		{"row raster transforms (8/9) bump to v12", rowXform, false, 12},
	}
	for _, c := range cases {
		if got := writeVer(c.files, c.precise); got != c.want {
			t.Errorf("%s: got v%d, want v%d", c.name, got, c.want)
		}
	}
}

// v8 archives must persist only metadata the old format can hold (mode to 0777,
// time to seconds); otherwise old builds parse the bytes with a shifted layout.
func TestMetaRoundTripV8(t *testing.T) {
	files := []fileEntry{
		{name: "a.txt", size: 5, mtime: 12345, mode: 0644},
		{name: "s", size: 1, mtime: 12345, mode: uint32(os.ModeSticky) | 0755},
	}
	var sh, rh [8]byte
	raw := serializeMeta(nil, files, sh, rh, 8)
	_, got, _, _ := parseMeta(raw, 0, uint32(len(files)), 8)
	if got[0].mtime != 12345 {
		t.Errorf("v8 mtime round trip mismatch: %d", got[0].mtime)
	}
	if got[1].mode != 0755 {
		t.Errorf("v8 should clamp mode to 0777, got %o", got[1].mode)
	}
}

// ---------- verify logical consistency checks ----------

func TestCheckLogical(t *testing.T) {
	a := &archive{
		chunks: []chunkMeta{{uncomp: 100}, {uncomp: 50}},
		files:  []fileEntry{{name: "f", size: 150, chunks: []uint32{0, 1}}},
	}
	if runCatchingFatal(a.checkLogical) {
		t.Error("a consistent archive must not error")
	}
	a.files[0].size = 149
	if !runCatchingFatal(a.checkLogical) {
		t.Error("size mismatching chunk lengths must error -- else verify passes self-contradictory archives")
	}
	// dirs and links carry no data chunks; skip them in this check
	a.files = []fileEntry{{name: "d", isDir: true}, {name: "l", isLink: true, link: "x"}}
	if runCatchingFatal(a.checkLogical) {
		t.Error("dir/link entries must not be part of chunk-length checks")
	}
}

// ---------- hostile/corrupt archives: a huge declared chunk must not eat 4GB up front ----------

func TestAbsurdChunkSizeRejected(t *testing.T) {
	files := []fileEntry{{name: "a.txt", size: 100, mode: 0644, chunks: []uint32{0}}}
	var sh, rh [8]byte
	// chunk table claims 0xFFFFFFFF (~4GB); real data is 100 bytes
	raw := serializeMeta([]*chunkMeta{{uncomp: 0xFFFFFFFF}}, files, sh, rh, 8)
	if !runCatchingFatal(func() { parseMeta(raw, 1, 1, 8) }) {
		t.Error("4GB-declared chunk accepted -- readChunk would make 4GB then find short data")
	}
	// normal sizes must pass (no false positive)
	raw2 := serializeMeta([]*chunkMeta{{uncomp: 4096}}, files, sh, rh, 8)
	parseMeta(raw2, 1, 1, 8)
}

// ---------- source files modified mid-pack: the archive must stay self-consistent ----------

func TestSizedReadAccountsActualBytes(t *testing.T) {
	unmute := muteStdio(t)
	defer unmute()
	// unchanged -> return the stat size as-is, no warning
	if got := sizedRead("f", 100, 100); got != 100 {
		t.Errorf("unchanged should return 100 as-is, got %d", got)
	}
	// smaller/larger -> account by bytes actually read (keeps the archive self-consistent)
	if got := sizedRead("f", 100, 70); got != 70 {
		t.Errorf("shrunk file should account 70 bytes read, got %d", got)
	}
	if got := sizedRead("f", 100, 140); got != 140 {
		t.Errorf("grown file should account 140 bytes read, got %d", got)
	}
}

func TestPackSurvivesFileChangingUnderfoot(t *testing.T) {
	// files being modified mid-pack is the norm (logs, DBs, in-flight downloads).
	// old code accounted by the stat size at open, writing archives that declare N bytes
	// but hold M -- pack reported success, the explosion waited for unpack.
	//
	// this case does not assert the race happens (that would be flaky), only the invariant:
	// whatever happens to the files, the archive must stay self-consistent (unpacks).
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(src, "growing.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("log line ................\n"), 300000), 0o644); err != nil {
		t.Fatal(err)
	}
	// append in the background so the file changes underfoot. Cap it or it fills the disk.
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

	// invariant: the archive must unpack (verify=true also runs stream-level checks)
	out := filepath.Join(dir, "out")
	unmute = muteStdio(t)
	fataled := runCatchingFatal(func() { unpack(arc, out, true, nil) })
	unmute()
	if fataled {
		t.Fatal("archive packed while sources changed does not unpack -- internally inconsistent")
	}
}

// ---------- missing system xz must warn, not silently degrade ----------

func TestXZFallbackWarns(t *testing.T) {
	// without system xz on PATH, max/ultra fall back to the built-in pure-Go lzma2.
	// this is not merely "slower" -- measured 26.89% -> 33.40% on one corpus, 24% bigger.
	// that path used to be fully silent while users thought they were on max. Pin one warning.
	//
	// exec.LookPath re-reads PATH each call; pointing it at an empty dir guarantees no xz,
	// so the fallback branch triggers reliably on machines with or without xz.
	t.Setenv("PATH", t.TempDir())

	before := xzWarnCount
	xzWarnOnce = sync.Once{} // sync.Once cannot reset; swap in a fresh one for independent counting
	b := &backend{}
	if out := b.xzCompress(bytes.NewReader([]byte("hello, world")), "3", "2", "2MiB"); out != nil {
		t.Fatal("without xz on PATH, xzCompress must return nil so the caller falls back")
	}
	if xzWarnCount != before+1 {
		t.Errorf("missing system xz must warn exactly once (got %d new)", xzWarnCount-before)
	}
}

// ---------- extract targets must tolerate a ./ prefix ----------

func TestNormAsk(t *testing.T) {
	// note: "d\\sub" is not tested -- filepath.ToSlash converts only on Windows (backslash
	// is a legal filename char on Unix), where it is already the identity.
	cases := [][2]string{
		{"d/sub/r.bin", "d/sub/r.bin"}, // as-is
		{"./d/sub/r.bin", "d/sub/r.bin"},
		{"././d", "d"},
		{"/d/sub", "d/sub"},
		{"d/sub/", "d/sub"}, // trailing slash on a dir
		{"", ""},
	}
	for _, c := range cases {
		if got := normAsk(c[0]); got != c[1] {
			t.Errorf("normAsk(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// ---------- reads must check the return: short reads leave zeros in the buffer (silent corruption) ----------

func TestMustReadAt(t *testing.T) {
	unmute := muteStdio(t)
	defer unmute()
	data := []byte("0123456789")
	r := bytes.NewReader(data)

	// normal read: no fatal
	buf := make([]byte, 4)
	if runCatchingFatal(func() { mustReadAt(r, buf, 2, "test") }) {
		t.Fatal("a normal read must not fatal")
	}
	if string(buf) != "2345" {
		t.Errorf("read %q, want \"2345\"", buf)
	}
	// short read: past-end read leaves zeros in the buffer; must fatal
	over := make([]byte, 20)
	if !runCatchingFatal(func() { mustReadAt(r, over, 5, "test") }) {
		t.Error("short read must fatal -- leftover zeros would be written as data")
	}
	// empty read: a 0-length read must not error
	if runCatchingFatal(func() { mustReadAt(r, nil, 0, "test") }) {
		t.Error("a 0-byte read must not fatal")
	}
}

// ---------- truncated archives must fail cleanly, never panic ----------

// mute: thousands of parses below; printing each to test output is pointless
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
	// truncated archives are the easiest panic input: the header claims N metadata bytes
	// and M data bytes that the file does not have. A panic is DoS when auto-processing
	// foreign archives. Such bugs often need a specific cut length, unreachable by hand --
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// enumerate prefixes instead. Three contents: compressible text (solid stream) /
	// random (raw region) / medium-redundant, so metadata + data + raw all exist to cut.
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
	// the five modes are **five different codec implementations** (zstd / lzma2-xz /
	// hand-written CM); testing one tests a third. Enumerate them all.
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
				t.Fatalf("archive only %d bytes; prefix enumeration would test nothing", len(full))
			}
			// prior: the intact archive must list fine. Else the thousands of rounds below
			// all fail at step one and no real code path is exercised.
			if runCatchingFatal(func() { listArchive(arc, false) }) {
				t.Fatal("intact archive does not list -- the enumeration below would be vacuous")
			}
			t.Logf("archive %d bytes, enumerating %d prefixes (full unpack every 16)", len(full), len(full)+1)
			// runCatchingFatal converts fatal into a caught panic, while **real panics
			// propagate**, so a crash surfaces as a test failure -- exactly what we want.
			for n := 0; n <= len(full); n++ {
				p := filepath.Join(dir, "trunc.hcax")
				if err := os.WriteFile(p, full[:n], 0o644); err != nil {
					t.Fatal(err)
				}
				runCatchingFatal(func() { listArchive(p, false) })
				// unpack goes far deeper than metadata reads (create files/decompress/transform);
				if n%16 == 0 {
					out := filepath.Join(dir, "out")
					os.RemoveAll(out)
					runCatchingFatal(func() { unpack(p, out, false, nil) })
				}
			}
		})
	}
}

// ---------- randomly tampered archives must also never panic ----------

func TestCorruptedArchiveNeverPanics(t *testing.T) {
	// truncation only shrinks data; random tampering can grow length fields (chunk size
	// 100 -> 1e9), a different out-of-bounds class truncation never hits.
	// archive bytes are **someone else's**; handling them must not crash on one bad byte.
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
			// prior: same as TestTruncatedArchiveNeverPanics; keep the 200 rounds honest
			if runCatchingFatal(func() { listArchive(arc, false) }) {
				t.Fatal("intact archive does not list -- the tamper cases below would be vacuous")
			}
			const rounds = 200
			t0 := time.Now()
			for i := 0; i < rounds; i++ {
				b := append([]byte(nil), full...)
				for k, n := 0, 1+rng.Intn(3); k < n; k++ { // flip 1-3 bits per round
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
			// no "merely slow": 200 archives of tens of KB finish in seconds when healthy.
			// real trap once: a corrupted CM frame length makes the decoder never stop,
			// hanging hcax list forever on one corrupt text archive (core pegged).
			// that hangs rather than panics, hence this separate time gate.
			if d := time.Since(t0); d > 60*time.Second {
				t.Errorf("%s mode 200 rounds took %v -- some input trapped the decoder", mode, d)
			}
		})
	}
}

// ---------- archive writes must be atomic: a failure must not destroy the old archive ----------

func TestArchiveWriterKeepsOldOnFailure(t *testing.T) {
	// old code os.Create(outPath) truncated the old archive to 0 on open. A failure at
	// the final write stage (disk full) lost the old backup and left half a new one.
	dir := t.TempDir()
	out := filepath.Join(dir, "backup.hcax")
	const old = "previous good archive"
	if err := os.WriteFile(out, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	w := openArchiveForWrite(out)
	w.f.Write([]byte("wrote half, then crashed")) // simulate a mid-write failure
	w.abort()                       // this is exactly what fatal() does

	got, err := os.ReadFile(out)
	if err != nil || string(got) != old {
		t.Errorf("old archive destroyed on write failure: %q (err=%v)", got, err)
	}
	// no half-written temp file may be left behind
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("%d files left in dir; partial artifacts not cleaned up", len(ents))
	}
}

func TestArchiveWriterCommitsAtomically(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "a.hcax")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := openArchiveForWrite(out)
	w.f.Write([]byte("new content"))
	w.f.Close()
	w.commit()

	got, _ := os.ReadFile(out)
	if string(got) != "new content" {
		t.Errorf("archive content wrong after commit: %q", got)
	}
	// overwriting an existing archive keeps its mode; do not bump 0600 to 0644
	fi, _ := os.Stat(out)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("overwrite changed the archive mode: %v (want 0600)", fi.Mode().Perm())
	}
	// abort with tmp cleared must be a no-op (the cleanup hook calls it unconditionally)
	w.abort()
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("%d extra files after commit", len(ents))
	}
}

func TestArchiveWriterNewFilePerm(t *testing.T) {
	// a fresh archive keeping CreateTemp's 0600 is unreadable to others when backed up
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
		t.Errorf("fresh archive mode %v, want 0644", fi.Mode().Perm())
	}
}

// ---------- system xz stderr must never leak into the compressed stream ----------

func TestXZStderrNotMixedIntoOutput(t *testing.T) {
	// build an xz that warns on stderr yet compresses fine. If cmd.Stderr shares the
	// stdout buffer, the warning lands in the stream header -- pack succeeds, archives
	// grow bytes, and unpack finally reports invalid header magic bytes. Silent corruption.
	real, err := exec.LookPath("xz")
	if err != nil {
		t.Skip("no system xz on this machine (or not on PATH); the case needs a real xz to wrap")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "xz")
	script := "#!/bin/sh\necho 'xz: (simulated) one harmless warning' >&2\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	b := &backend{}
	out := b.xzCompress(bytes.NewReader([]byte("hello, world, hello, world")), "3", "2", "2MiB")
	if out == nil {
		t.Fatal("xz succeeded, must not return nil")
	}
	const xzMagic = "\xFD" + "7zXZ" + "\x00"
	if len(out) < 6 || string(out[:6]) != xzMagic {
		t.Errorf("stream does not start with xz magic; stderr leaked in: %q", out[:min(len(out), 32)])
	}
}
