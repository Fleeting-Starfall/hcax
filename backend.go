package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const storeThresholdPct = 100

// mmtMinBytes: data threshold for parallel solid grouping (mmt).
// Parallel groups cut the solid stream into independently compressed segments,
// losing cross-segment redundancy -> slightly worse ratio. On small/medium corpora
// ratio wins, so parallelism only kicks in when compressible data is large enough
// (>=64MiB, where speed is the bottleneck).

const mmtMinBytes = 64 << 20

// zstd encodes per chunk (chunk cap chunkMax=256KiB); each chunk is EncodeAll'd
// independently, so matches cannot cross chunks. The encoder window is therefore
// capped at 1MiB (far beyond the 256KiB chunk cap); bigger windows just waste memory.
// best used to set window=27 (1<<27=128MiB), keeping two zstd encoders resident at
// ~256MiB — the reason best peaked at ~264MB vs max's ~88MB (zstd mode is the only
// one building encoders). After the cap best sits at ~10MB with identical ratio
// (chunk-local matches never need the big window).
const zstdMaxWindowLog = 20

// dictTrialMaxBytes: cap for the "is the trained dict worth it" trial.
// The dict itself is stored in the archive and can cost more than it saves on small
// corpora — only below this size is a second compression pass worth comparing
// "dict frame + dict" vs "no dict frame". (See the trial logic in archive.go)
const dictTrialMaxBytes = 32 << 20

type modeSpec struct {
	code    byte
	backend string // "zstd" or "lzma2"
	level   int
	window  int  // zstd window log / lzma2 dict hint
	solid   bool // true=whole-file solid (CDC dedup off, like 7z); used by ultra
	mmt     bool // true=parallel solid-group compression; used by max
}

var modes = map[string]modeSpec{
	"fast":  {0, "zstd", 3, 0, false, false},
	"best":  {1, "zstd", 19, 27, false, false},
	"max":   {2, "lzma2", 9, 27, false, true},
	"ultra": {3, "lzma2", 9, 27, false, false}, // v8b: solid off (dedup on), memory drops from ~1.8GB to ~max level (~86MB); still single-stream full trial for ratio (unlike max's parallel)
	"text":  {4, "cm", 0, 0, false, false},     // context mixing: extreme ratio on text/code/source (pure algorithm, slow)
}

type backend struct {
	spec      modeSpec
	zEnc      *zstd.Encoder // no-dict encoder (zstd)
	zDec      *zstd.Decoder
	zEncDict  *zstd.Encoder // trained-dict encoder (zstd best for similar files)
	zDecDict  *zstd.Decoder // trained-dict decoder
	gateEnc   *zstd.Encoder // cheap l1 encoder for incompressibility checks / per-chunk preprocessing selection
	dictParam string        // lzma2 dict size (converges on compressible data size)
	lcParam   string        // lzma2 lc (literal context bits), adaptive
	pbParam   string        // lzma2 pb (position bits), adaptive
	trainDict []byte        // zstd(best) trained dict body (reused to build the output encoder when streaming)
	hasDict   bool
	dictBytes []byte // trained dict body from the archive (rebuilds the dict decoder when streaming **decompression**)
}

func newBackend(spec modeSpec) *backend {
	b := &backend{spec: spec, lcParam: "4", pbParam: "2"} // lzma2 defaults; tuneLZMA2 overrides per data
	if spec.backend == "zstd" {
		var err error
		// v8c: zEnc (no-dict encoder) is created lazily — best always trains a dict and
		// uses zEncDict; the old zEnc was never used yet kept a level-19 encoder resident
		// (~35~69MB; its memory is level-driven, not window-driven). Lazily, only
		// dict-less zstd modes (fast / training fallback) build it.
		b.zDec, err = zstd.NewReader(nil)
		if err != nil {
			fatal("zstd decoder: %v", err)
		}
	}
	// v8: the gate encoder only estimates "is this block compressible" (raw/preprocess
	// choice); no big window or history needed. Fixed 1MiB window avoids zstd streaming
	// encoder history growth (can hit GBs on large files, see ensureHist).
	// Note: only zstd mode needs zEnc/zDec; lzma2 skips them, saving the 128MiB dict window.
	gateOpts := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithWindowSize(1 << 20)}
	g, err := zstd.NewWriter(io.Discard, gateOpts...)
	if err != nil {
		fatal("gate encoder: %v", err)
	}
	b.gateEnc = g
	return b
}

// Per-chunk adaptive preprocessing: try candidate transforms, pick the smallest under
// the gate (zstd-l3) projection. Only adopt a transform that actually shrinks the chunk
// -> never worse; also catches structured data without file headers (raw raster / x86
// code). light=true (best): fewer candidates, lower gate cost; max/ultra try all 6.

func (b *backend) chooseTransform(cb []byte) (byte, []byte) {
	if len(cb) < 4 {
		return xfNone, append([]byte(nil), cb...)
	}
	var cands []byte
	if b.spec.backend == "lzma2" {
		cands = []byte{xfNone, xfDelta1, xfDelta2, xfDelta3, xfDelta4, xfBCJX86}
	} else {
		cands = []byte{xfNone, xfDelta3, xfBCJX86} // best: light candidates for speed
	}
	best, bestSz := xfNone, len(cb)
	// v8: reuse one scratch for all candidates instead of allocating 6 full-size buffers per chunk
	scratch := make([]byte, len(cb))
	for _, t := range cands {
		copy(scratch, cb)
		applyXformInPlace(t, scratch)
		if sz := len(b.gateEnc.EncodeAll(scratch, nil)); sz < bestSz {
			best, bestSz = t, sz
		}
	}
	return best, applyXform(best, cb, false)
}

// Cheap check: is this block incompressible (store raw instead of feeding the
// compressor)? zstd mode always compresses, never stores raw.

func (b *backend) shouldStore(cb []byte) bool {
	if len(cb) == 0 {
		return true
	}
	// CM(text) predicts statistically; high-entropy data (random/already compressed)
	// neither shrinks nor is fast (~1.3MB/s bitwise modeling, measured, see README §8;
	// an early 0.5MB/s estimate was corrected). Such blocks go raw so CM does not burn
	// time on data it cannot compress. (Previously only lzma2 did raw checks; text
	// always forced chunks into CM.)
	if b.spec.backend == "cm" {
		return byteEntropy(cb) > 7.95
	}
	if b.spec.backend != "lzma2" {
		return false
	}
	out := b.gateEnc.EncodeAll(cb, nil)
	if len(out) < len(cb)*storeThresholdPct/100 {
		return false // gate says compressible -> solid stream
	}
	// Gate says "incompressible": confirm it is really random. Image residuals often
	// fail chunk-local compression yet compress well globally (solid stream / large
	// lzma2 dict); storing them raw on the gate's verdict alone would waste ratio.
	// Double-check with byte entropy: only high-entropy (true random) goes raw.
	return byteEntropy(cb) > 7.95
}

// lzma2 dict converges on compressible data size. lzma2 matches are limited by the
// dict; hcax chunks average 64KiB, so 4MiB covers ~64 chunks of history — on mixed
// corpora (text/JSON/source) larger dicts measured zero gain (12.6MB corpus:
// 4/8/16/32MiB compress **identically**), only more memory.
//
// Executables are different: on a 20MB Mach-O, 8MiB->16MiB dict saves another 4.0%
// (6,659,708 -> 6,395,092 B); 32MiB saves nothing. Their repetition is long-range
// (same function bodies / string tables far apart); only a large enough dict connects it.
//
// So >=16MiB of compressible data always gets 16MiB: runtime unchanged (measured
// 3.30s vs 3.36s), at the cost of xz subprocess peak memory ~121MB -> ~208MB (only
// for >=16MiB inputs; smaller inputs stay at 4MiB/2MiB, ~72MB peak).

func dictFor(n int) string {
	switch {
	case n < 4<<20:
		return "2MiB"
	case n < 16<<20:
		return "4MiB"
	default:
		return "16MiB"
	}
}

// lzma2 lc/pb adaptation: sample the solid stream, try candidate parameters, pick the
// smallest projection. Only adopt a genuinely smaller combo -> never worse; depends on
// system xz (falls back to defaults and ulikunitz if missing).
// Measured: pb=4 often beats the default pb=2 on code/structured data (~1-2%), but the
// optimum varies by data, hence adaptive.

func (b *backend) tuneLZMA2(r io.ReaderAt, size int64) {
	if b.spec.backend != "lzma2" || size == 0 {
		return
	}
	if _, err := exec.LookPath("xz"); err != nil {
		return
	}
	// Sample: 8 evenly spaced segments totaling ~1MiB — representative of the whole
	// stream yet fast (avoids misjudging from the head alone)
	if size < 256<<10 {
		return
	}
	sample := stridedSampleFrom(r, size, 1<<20)
	type cand struct{ lc, pb string }
	cands := []cand{{"4", "4"}, {"3", "4"}, {"4", "2"}, {"4", "3"}}
	bestLc, bestPb := b.lcParam, b.pbParam
	bestSz := int64(-1)
	for _, c := range cands {
		if out := b.xzCompress(bytes.NewReader(sample), c.lc, c.pb, b.dictParam); out != nil {
			if sz := int64(len(out)); bestSz < 0 || sz < bestSz {
				bestSz, bestLc, bestPb = sz, c.lc, c.pb
			}
		}
	}
	if bestSz >= 0 {
		b.lcParam, b.pbParam = bestLc, bestPb
	}
}

// Evenly sample 8 segments from an io.ReaderAt into a buffer of at most max bytes
// (same as stridedSample, but reading from a file rather than an in-memory slice).
//
// ReadAt errors here are **deliberately** non-fatal: this is only a sample for lzma2
// parameter selection (pb=2/4). A few missing bytes cost a slightly suboptimal
// parameter (fractions of a percent of ratio), never data corruption, and a sampling
// failure must not sink a whole backup — unlike compressMT, which reads the **data
// being written into the archive**: a short read there is silent corruption and is
// fatal (see mustReadAt).
func stridedSampleFrom(r io.ReaderAt, size int64, max int) []byte {
	if size <= int64(max) {
		buf := make([]byte, size)
		r.ReadAt(buf, 0)
		return buf
	}
	seg := max / 8
	if seg < 4096 {
		seg = 4096
	}
	var out []byte
	stride := (size - int64(seg)) / 7
	for i := 0; i < 8; i++ {
		off := i * int(stride)
		if off+seg > int(size) {
			off = int(size) - seg
		}
		buf := make([]byte, seg)
		r.ReadAt(buf, int64(off))
		out = append(out, buf...)
	}
	return out
}

// xzFallbackWarn tracks whether this process already warned about the xz fallback;
// warn once so max does not print a line per solid stream. Tests assert the warning
// fired via xzWarnCount.
var (
	xzWarnOnce  sync.Once
	xzWarnCount int
)

// warnXZFallback: the user must know when system xz is unavailable.
// This is not cosmetic — measured on the same corpus: with system xz ratio 26.89%,
// falling back to the built-in pure Go lzma2 gives 33.40%, an archive 24% larger.
// It used to be **fully silent**, the user believing they were on the strongest mode.
func warnXZFallback(reason string) {
	xzWarnOnce.Do(func() {
		xzWarnCount++
		fmt.Fprintf(os.Stderr,
			"warning: %s; max/ultra fell back to the built-in pure Go lzma2\n"+
				"       ratio will be noticeably worse (measured 26.89%% -> 33.40%%, 24%% larger archive).\n"+
				"       install system xz to restore: brew install xz / apt-get install xz-utils\n", reason)
	})
}

// compress with system xz at given lc/pb; nil on unavailability or error (fallback path)

func (b *backend) xzCompress(r io.Reader, lc, pb, dict string) []byte {
	xzbin, err := exec.LookPath("xz")
	if err != nil {
		warnXZFallback("xz not found in PATH")
		return nil
	}
	// v8d: dropped the extreme -e mode (nice=273, depth 256MiB) — measured zero ratio
	// gain on 30MB mixed data (identical to -9) while slowing the matcher notably.
	// Now preset=9 + nice=64 (standard -9) with dictFor's converged dict: much lower
	// time and memory. dict caps at 16MiB (see dictFor): beyond that ratio gain is
	// <0.03% while memory/time jump.
	// v8e: takes io.Reader — the solid stream lives in a temp file, xz reads it as a
	// stream instead of loading the whole stream into memory.
	cmd := exec.Command(xzbin, "-c", "-", "--lzma2=preset=9,dict="+dict+",nice=64,lc="+lc+",lp=0,pb="+pb)
	cmd.Stdin = r
	// stdout and stderr must be separate buffers. They used to share one, so anything
	// xz wrote to stderr (even a harmless warning) got appended to the compressed
	// stream — pack "succeeded", the archive silently grew by dozens of bytes, and the
	// failure surfaced only at unpack as invalid header magic bytes. Corruption was
	// silent, hence the split: stdout is data, stderr is diagnostic only.
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		reason := "system xz failed"
		if msg := bytes.TrimSpace(errb.Bytes()); len(msg) > 0 {
			reason += " (xz says: " + string(msg) + ")"
		}
		warnXZFallback(reason)
		return nil
	}
	return out.Bytes()
}

// Small/medium data (single stream) parameter selection: trial pb=2/4 on the full
// stream, keep the smaller and **reuse its compressed output** (no recompression),
// recording the winning parameter for later use (e.g. mmt groups). Based on the real
// full-stream result, more reliable than sampling; costs 2 full-stream compressions.

func (b *backend) compressLZMA2Best(cb []byte) []byte {
	if b.spec.backend != "lzma2" {
		return b.compress(cb) // zstd etc. go straight through
	}
	if len(cb) < 256<<10 {
		return b.compress(cb) // too small to bother tuning
	}
	pbs := []string{"2", "4"}
	outs := make([][]byte, len(pbs))
	var wg sync.WaitGroup
	for i, pb := range pbs {
		wg.Add(1)
		go func(i int, pb string) {
			defer wg.Done()
			outs[i] = b.xzCompress(bytes.NewReader(cb), b.lcParam, pb, b.dictParam)
		}(i, pb)
	}
	wg.Wait()
	var best []byte
	for i, out := range outs {
		if out != nil && (best == nil || len(out) < len(best)) {
			best = out
			b.pbParam = pbs[i]
		}
	}
	if best == nil {
		return b.compress(cb) // xz unavailable -> fallback
	}
	return best
}

// Evenly spaced sampling (in-memory slice): used by the old tuneLZMA2; v8e moved to
// stridedSampleFrom (file source)

func stridedSample(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	seg := max / 8
	if seg < 4096 {
		seg = 4096
	}
	var out []byte
	stride := (len(b) - seg) / 7
	for i := 0; i < 8; i++ {
		off := i * stride
		if off+seg > len(b) {
			off = len(b) - seg
		}
		out = append(out, b[off:off+seg]...)
	}
	return out
}

func (b *backend) compress(cb []byte) []byte {
	if b.spec.backend == "cm" {
		return cmCompress(cb)
	}
	if b.spec.backend == "zstd" {
		if b.hasDict && b.zEncDict != nil {
			return b.zEncDict.EncodeAll(cb, nil)
		}
		if b.zEnc == nil {
			opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level))}
			if b.spec.window > 0 {
				wl := b.spec.window
				if wl > zstdMaxWindowLog {
					wl = zstdMaxWindowLog
				}
				opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
			}
			e, err := zstd.NewWriter(io.Discard, opts...)
			if err != nil {
				fatal("zstd encoder: %v", err)
			}
			b.zEnc = e
		}
		return b.zEnc.EncodeAll(cb, nil)
	}
	// lzma2 mode: system xz (lzma2, xz container) reaches near-7z extreme ratios;
	// ulikunitz xz is the fallback. Tuned lzma2 params (dict/nice/lc/lp/pb) + converged
	// dict beat the -9e preset by ~0.34% at the same speed.
	if b.spec.backend == "lzma2" {
		if out := b.xzCompress(bytes.NewReader(cb), b.lcParam, b.pbParam, b.dictParam); out != nil {
			return out
		}
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			fatal("xz writer: %v", err)
		}
		if _, err := w.Write(cb); err != nil {
			fatal("xz write: %v", err)
		}
		if err := w.Close(); err != nil {
			fatal("xz close: %v", err)
		}
		return buf.Bytes()
	}
	fatal("unknown backend %s", b.spec.backend)
	return nil
}

// Stream-compress the solid stream (zstd modes: fast/best): feed the zstd encoder
// from an io.Reader segment by segment; memory holds only "one chunk + encoder state"
// instead of the whole solid stream (can be hundreds of MiB). Output is a single zstd
// frame (decoding restores the whole stream); unpack logic is unchanged.
func (b *backend) compressZstdStream(r io.Reader) []byte {
	var buf bytes.Buffer
	var enc *zstd.Encoder
	var err error
	if b.hasDict && b.trainDict != nil {
		opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level)), zstd.WithEncoderDict(b.trainDict)}
		enc, err = zstd.NewWriter(&buf, opts...)
	} else {
		opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level))}
		if b.spec.window > 0 {
			wl := b.spec.window
			if wl > zstdMaxWindowLog {
				wl = zstdMaxWindowLog
			}
			opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
		}
		enc, err = zstd.NewWriter(&buf, opts...)
	}
	if err != nil {
		fatal("zstd streaming encoder: %v", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		fatal("zstd streaming compress: %v", err)
	}
	if err := enc.Close(); err != nil {
		fatal("zstd close: %v", err)
	}
	return buf.Bytes()
}

// Parallel solid grouping (max mode): split compressible chunks into G groups in
// order, compress each group with xz in parallel into its own frame, concatenate back
// into one stream. Losing cross-segment redundancy costs a little ratio for a
// multi-fold speedup. Decompression is still one stream (serial xz frames); format
// unchanged.
// Note: data comes from the solid temp file, not cm.data — after v8 set cm.data=nil
// it is empty, and reading c.data here would yield empty groups -> empty frames ->
// out-of-range panic at unpack. That regression hit the mmt path in v8 and only
// triggers on large corpora (>=64MiB).

func (b *backend) compressMT(chunks []*chunkMeta, solid io.ReaderAt, solidSize int64) []byte {
	var comp []*chunkMeta
	for _, c := range chunks {
		if !c.stored {
			comp = append(comp, c)
		}
	}
	if len(comp) == 0 {
		return nil
	}
	G := runtime.NumCPU()
	if G > 8 {
		G = 8
	}
	if G < 1 {
		G = 1
	}
	if len(comp) <= G {
		return b.compress(concatChunksFile(comp, solid))
	}
	per := (len(comp) + G - 1) / G
	type grp struct {
		sub   []byte
		local map[*chunkMeta]uint64
	}
	var groups []grp
	for i := 0; i < len(comp); i += per {
		e := i + per
		if e > len(comp) {
			e = len(comp)
		}
		// v8e: the solid stream lands in a temp file; same-group chunks are read by offset
		// (one copy, only group-size resident) -- no more full stream + per-group duplicates
		start := comp[i].offset
		last := comp[e-1]
		end := last.offset + uint64(last.uncomp)
		buf := make([]byte, end-start)
		mustReadAt(solid, buf, int64(start), "solid stream (parallel groups)")
		local := map[*chunkMeta]uint64{}
		base := start
		for _, c := range comp[i:e] {
			local[c] = c.offset - base
		}
		groups = append(groups, grp{buf, local})
	}
	results := make([][]byte, len(groups))
	var wg sync.WaitGroup
	for gi := range groups {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			results[gi] = b.compressLZMA2Dict(groups[gi].sub, dictFor(len(groups[gi].sub)))
		}(gi)
	}
	wg.Wait()
	var out bytes.Buffer
	ugs := uint64(0) // uncompressed running total (offsets live in the decompressed stream)
	for gi, g := range groups {
		out.Write(results[gi])
		for c, lo := range g.local {
			c.offset = ugs + lo
		}
		ugs += uint64(len(g.sub))
	}
	return out.Bytes()
}

func concatChunksFile(cs []*chunkMeta, solid io.ReaderAt) []byte {
	var buf bytes.Buffer
	for _, c := range cs {
		seg := make([]byte, c.uncomp)
		mustReadAt(solid, seg, int64(c.offset), "solid stream (single-stream concat)")
		buf.Write(seg)
	}
	return buf.Bytes()
}

// Compress with a given lzma2 dict (parallel groups): smaller group -> smaller dict ->
// xz subprocess memory scales down with group size. Falls back to ulikunitz when
// system xz is missing. xz frames are self-describing (dict in the frame header), so
// decompression needs no remembered parameters; format unchanged.

func (b *backend) compressLZMA2Dict(cb []byte, dict string) []byte {
	if b.spec.backend != "lzma2" {
		return b.compress(cb)
	}
	if len(cb) < 256<<10 {
		return b.compress(cb)
	}
	if out := b.xzCompress(bytes.NewReader(cb), b.lcParam, b.pbParam, dict); out != nil {
		return out
	}
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		fatal("xz writer: %v", err)
	}
	if _, err := w.Write(cb); err != nil {
		fatal("xz write: %v", err)
	}
	if err := w.Close(); err != nil {
		fatal("xz close: %v", err)
	}
	return buf.Bytes()
}

// maxOut: expected output bound; only CM needs it (zstd/lzma2 stop at their own
// end-of-stream markers). The CM frame's length field comes from the archive; a
// corrupted value makes the decoder run forever — see cmDecompressTo.
func (b *backend) decompress(frame []byte, maxOut int64) []byte {
	if b.spec.backend == "cm" {
		return cmDecompress(frame, maxOut)
	}
	if b.spec.backend == "zstd" {
		if b.zDecDict != nil {
			out, err := b.zDecDict.DecodeAll(frame, nil)
			if err != nil {
				fatal("zstd dict decode: %v", err)
			}
			return out
		}
		out, err := b.zDec.DecodeAll(frame, nil)
		if err != nil {
			fatal("zstd decode: %v", err)
		}
		return out
	}
	r, err := xz.NewReader(bytes.NewReader(frame))
	if err != nil {
		fatal("xz reader: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		fatal("xz read: %v", err)
	}
	return out
}

// Streaming decompression: returns a backend-wrapped reader for "decompress while
// writing to disk". Unlike decompress (whole output in memory), the output never
// needs to be resident — the premise of bounded memory on the unpack side. The
// returned closeFn releases decoder resources (zstd spawns a background goroutine);
// may be nil.

func (b *backend) decompressReader(r io.Reader, maxOut int64) (io.Reader, func(), error) {
	switch b.spec.backend {
	case "cm":
		// CM needs the whole compressed frame first (self-describing length head), output is streaming:
		// pipe cmDecompressTo's output into a reader; memory stays at one 64KiB buffer.
		frame, err := io.ReadAll(r)
		if err != nil {
			return nil, nil, err
		}
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(cmDecompressTo(pw, frame, maxOut))
		}()
		return pr, func() { pr.Close() }, nil
	case "zstd":
		opts := []zstd.DOption{}
		if len(b.dictBytes) > 0 {
			opts = append(opts, zstd.WithDecoderDicts(b.dictBytes))
		}
		d, err := zstd.NewReader(r, opts...)
		if err != nil {
			return nil, nil, err
		}
		return d, func() { d.Close() }, nil
	default: // lzma2: xz container, natively streaming
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return xr, nil, nil
	}
}

// ---------- container structs ----------

type archive struct {
	spec      modeSpec
	be        *backend
	dataStart int64

	// archive file handle: raw/data regions read on demand via ReadAt, never fully
	// loaded into memory
	src         *os.File
	compLen     int64 // compressed frame bytes
	metaCompLen int64 // metadata frame, compressed length
	metaRawLen  int64 // metadata, decompressed length
	rawOff      int64 // raw region offset within the archive
	rawLen      int64

	// Decompressed solid stream lands in a temp file (symmetric with pack-side v8e),
	// and **lazily**: only decompress up to the chunk being read. openArchive used to
	// unconditionally decompress the whole solid stream + raw region into memory —
	// listing one archive ate memory proportional to all its data, and extracting one
	// small file decompressed the entire stream.
	solidFile *os.File
	solidR    io.Reader // decompressor (kept for continued decoding)
	solidSize int64     // decompressed bytes so far
	solidEOF  bool      // stream exhausted

	// Total **uncompressed** size of the solid stream (sum of uncomp across
	// non-stored chunks). CM decoding needs it as a bound: the CM frame header's
	// declared length is untrusted and can hang the decoder if corrupted. 0 when the
	// old format cannot compute it; cmDecompressTo falls back to a loose absolute cap.
	solidUncomp int64

	// unpack progress (symmetric with pack side): stderr only, keeps stdout machine-readable
	progShow  bool
	progDone  uint64
	progLast  uint64
	progTotal uint64
	overwrote int // entries in the output dir that this unpack overwrote

	// Directory permission/timestamps cannot be applied as entries are extracted:
	// writing files into a directory bumps its mtime to "now", and chmod-ing it
	// read-only (0555) early blocks later child files. All deferred to the end
	// (see applyDirMeta).
	dirTodos []dirTodo

	chunks    []chunkMeta
	files     []fileEntry
	ver       byte // archive format version (v7+ records per-file transforms; older heuristics)
	hashLen   int  // per-chunk hash length: legacy=16, v6=0 (stream-level verification)
	solidHash [8]byte
	rawHash   [8]byte
}

// deferred directory metadata (mode + mtime)
type dirTodo struct {
	path string
	mode uint32
	nano int64
}
