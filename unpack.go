package main

// Unpack side: open the archive, lazily expand the solid stream, restore
// files/directories/symlinks/hard links.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func openArchive(path string) *archive {
	f, err := os.Open(path)
	if err != nil {
		fatal("open %s: %v", path, err)
	}
	// The handle must live through the whole unpack (raw/data regions are ReadAt on
	// demand), so it cannot be defer-closed: defer does not run under fatal(os.Exit),
	// hand it to the unified cleanup hooks.
	addCleanup(func() { f.Close() })
	hdr := make([]byte, 48)
	if _, err := io.ReadFull(f, hdr[:8]); err != nil {
		fatal("read header: %v", err)
	}
	if string(hdr[0:4]) != magic {
		fatal("not a HCAX file")
	}
	ver := hdr[4]
	if ver < 2 || ver > version {
		fatal("unsupported version: %d", ver)
	}
	spec := modeSpec{code: hdr[5], window: int(hdr[6])}
	for _, m := range modes {
		if m.code == spec.code {
			spec.backend, spec.level, spec.solid, spec.mmt = m.backend, m.level, m.solid, m.mmt
		}
	}
	be := newBackend(spec)

	// ================= v6/v7: compressed metadata + compact chunk table =================
	if ver >= 6 {
		if _, err := io.ReadFull(f, hdr[8:48]); err != nil {
			fatal("read header (v6+) failed: %v", err)
		}
		metaCompLen := binary.LittleEndian.Uint64(hdr[8:16])
		metaRawLen := binary.LittleEndian.Uint64(hdr[16:24])
		compLen := binary.LittleEndian.Uint64(hdr[24:32])
		rawLen := binary.LittleEndian.Uint64(hdr[32:40])
		nChunks := binary.LittleEndian.Uint32(hdr[40:44])
		nFiles := binary.LittleEndian.Uint32(hdr[44:48])
		metaOff := int64(48)
		dataOff := metaOff + int64(metaCompLen)
		rawOff := dataOff + int64(compLen)
		dictOff := rawOff + int64(rawLen)

		checkTailMagic(f, rawOff+int64(rawLen))
		checkHeaderBounds(f, metaCompLen, metaRawLen, compLen, rawLen, nChunks, nFiles)

		// 1) Load the training dict first (the metadata frame may have been compressed with it)
		if _, err := f.Seek(dictOff, io.SeekStart); err == nil {
			var dc uint32
			if binary.Read(f, binary.LittleEndian, &dc) == nil {
				for i := 0; i < int(dc); i++ {
					var dl uint32
					if binary.Read(f, binary.LittleEndian, &dl) != nil {
						break
					}
					d := make([]byte, dl)
					if _, err := io.ReadFull(f, d); err != nil {
						break
					}
					if spec.backend == "zstd" && i == 0 {
						if dec, e := zstd.NewReader(bytes.NewReader(nil), zstd.WithDecoderDicts(d)); e == nil {
							be.zDecDict = dec
						}
						be.dictBytes = d // 流式解压时需用它重建解码器
					}
				}
			}
		}
		// 2) Decompress and parse metadata
		f.Seek(metaOff, io.SeekStart)
		metaFrame := make([]byte, metaCompLen)
		if _, err := io.ReadFull(f, metaFrame); err != nil {
			fatal("read metadata: %v", err)
		}
		// The metadata region's uncompressed length is metaRawLen from the header, an
		// **exact** expectation — handed to CM as a bound so a corrupted frame header
		// cannot drive it forever.
		metaRaw := be.decompress(metaFrame, int64(metaRawLen))
		if uint64(len(metaRaw)) != metaRawLen {
			fatal("metadata length mismatch: expected %d got %d", metaRawLen, len(metaRaw))
		}
		chunks, files, sh, rh := parseMeta(metaRaw, nChunks, nFiles, ver)
		// 3) Data region: record positions only, no reading/decompression — lazy
		//    expansion lives in ensureSolid
		// The solid stream's uncompressed size = sum of uncomp across all **non-stored**
		// chunks (stored chunks live in the raw region). Also an exact value, used as the
		// CM decode bound — otherwise a damaged text archive would make it run forever.
		var solidUncomp uint64
		for _, c := range chunks {
			if !c.stored {
				solidUncomp += uint64(c.uncomp)
			}
		}
		a := &archive{spec: spec, be: be, src: f, dataStart: dataOff,
			compLen: int64(compLen), rawOff: rawOff, rawLen: int64(rawLen),
			metaCompLen: int64(metaCompLen), metaRawLen: int64(metaRawLen),
			solidUncomp: int64(solidUncomp),
			chunks: chunks, files: files, hashLen: 0,
			solidHash: sh, rawHash: rh, ver: ver}
		a.checkCounts()
		return a
	}

	// ================= legacy versions v2..v5 =================
	if _, err := io.ReadFull(f, hdr[8:24]); err != nil {
		fatal("read header: %v", err)
	}
	compLen := binary.LittleEndian.Uint64(hdr[8:16])
	var rawLen uint64
	var nChunks, nFiles uint32
	var dataStart int64
	if ver >= 3 {
		rawLen = binary.LittleEndian.Uint64(hdr[16:24])
		if _, err := io.ReadFull(f, hdr[24:32]); err != nil {
			fatal("read header (v3+) failed: %v", err)
		}
		nChunks = binary.LittleEndian.Uint32(hdr[24:28])
		nFiles = binary.LittleEndian.Uint32(hdr[28:32])
		dataStart = 32
	} else {
		nChunks = binary.LittleEndian.Uint32(hdr[16:20])
		nFiles = binary.LittleEndian.Uint32(hdr[20:24])
		dataStart = 24
	}

	// Confirm the file is not truncated before reading the data region (v2~v5 share
	// the v6+ trailer layout)
	checkTailMagic(f, dataStart+int64(compLen)+int64(rawLen))

	// Parse the chunk/file tables first to locate the v5 dict region; the training
	// dict must be loaded before any frame decompression (else zstd reports unknown dictionary)
	ctOff := dataStart + int64(compLen) + int64(rawLen)
	f.Seek(ctOff, io.SeekStart)
	chunks := make([]chunkMeta, nChunks)
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
	cb := make([]byte, ctEntry)
	for i := range chunks {
		io.ReadFull(f, cb)
		copy(chunks[i].hash[:], cb[0:16])
		chunks[i].offset = binary.LittleEndian.Uint64(cb[16:24])
		chunks[i].uncomp = binary.LittleEndian.Uint32(cb[24:28])
		chunks[i].stored = cb[28] == 1
		if ver >= 4 {
			chunks[i].xform = cb[29]
		}
		if ver >= 5 {
			chunks[i].dictID = cb[30]
		}
	}
	// file table (also accumulating length to locate the dict region)
	var ftLen int64
	files := make([]fileEntry, nFiles)
	for i := range files {
		var nl uint16
		binary.Read(f, binary.LittleEndian, &nl)
		ftLen += 2 + int64(nl) + 8 + 8 + 4 + 4 // nameLen + name + size(uint64) + mtime(uint64) + mode(uint32) + nchunks(uint32)
		nb := make([]byte, nl)
		io.ReadFull(f, nb)
		var size, mtime uint64
		var mode uint32
		binary.Read(f, binary.LittleEndian, &size)
		binary.Read(f, binary.LittleEndian, &mtime)
		binary.Read(f, binary.LittleEndian, &mode)
		var nc uint32
		binary.Read(f, binary.LittleEndian, &nc)
		ftLen += 4 * int64(nc)
		idxs := make([]uint32, nc)
		for j := range idxs {
			binary.Read(f, binary.LittleEndian, &idxs[j])
		}
		files[i] = fileEntry{name: string(nb), size: size, mtime: mtime, mode: mode, chunks: idxs}
	}
	// dict region (v5 only): must be loaded before any frame decompression
	if ver == 5 {
		dictOff := ctOff + int64(ctEntry)*int64(nChunks) + ftLen
		f.Seek(dictOff, io.SeekStart)
		var dc uint32
		binary.Read(f, binary.LittleEndian, &dc)
		for i := 0; i < int(dc); i++ {
			var dl uint32
			binary.Read(f, binary.LittleEndian, &dl)
			d := make([]byte, dl)
			io.ReadFull(f, d)
			if spec.backend == "zstd" && i == 0 {
				if dec, e := zstd.NewReader(bytes.NewReader(nil), zstd.WithDecoderDicts(d)); e == nil {
					be.zDecDict = dec
				}
				be.dictBytes = d
			}
		}
	}

	// Data region likewise records positions only (lazy): legacy layout is
	// [data][raw][chunk table][file table][dict region]
	a := &archive{spec: spec, be: be, src: f, dataStart: dataStart,
		compLen: int64(compLen), rawOff: dataStart + int64(compLen), rawLen: int64(rawLen),
		chunks: chunks, files: files, hashLen: 16, ver: ver}
	a.checkCounts()
	return a
}

func checkTailMagic(f *os.File, dataEnd int64) {
	const trailerLen = 20
	fi, err := f.Stat()
	if err != nil {
		return
	}
	sz := fi.Size()
	if sz < 48+trailerLen {
		fatal("archive too short (%d B): not a complete HCAX file", sz)
	}
	off := sz - trailerLen
	if dataEnd > off {
		fatal("archive truncated: data extends to %d B but the file is only %d B", dataEnd, sz)
	}
	tb := make([]byte, 4)
	if _, err := f.ReadAt(tb, off); err != nil {
		fatal("read trailer: %v", err)
	}
	if string(tb) != trailerMag {
		fatal("trailer magic mismatch: archive truncated or corrupt (offset %d is not %s)", off, trailerMag)
	}
}

func checkHeaderBounds(f *os.File, metaCompLen, metaRawLen, compLen, rawLen uint64, nChunks, nFiles uint32) {
	fi, err := f.Stat()
	if err != nil {
		return
	}
	sz := uint64(fi.Size())
	if metaCompLen > sz || compLen > sz || rawLen > sz {
		fatal("header length fields invalid (corrupt archive): metaComp=%d comp=%d raw=%d, file is only %d B",
			metaCompLen, compLen, rawLen, sz)
	}
	if 48+metaCompLen+compLen+rawLen+20 > sz {
		fatal("header layout exceeds file size (corrupt or truncated archive)")
	}
	if metaRawLen > 1<<30 {
		fatal("metadata length invalid (%d B): corrupt archive", metaRawLen)
	}
	// 块表项至少 5B, 文件表项至少 21B —— 据此给条目数设上界
	if uint64(nChunks)*5 > metaRawLen || uint64(nFiles)*16 > metaRawLen {
		fatal("header counts contradict metadata length (nChunks=%d nFiles=%d, metadata %d B): corrupt archive",
			nChunks, nFiles, metaRawLen)
	}
}

// Logical consistency: every file's declared size must equal the sum of its referenced
// chunk lengths. Verifying "the data stream bytes are unchanged" is not enough — the
// mapping "which chunks form which file" lives in metadata; if corrupted (e.g. a
// flipped bit in size), the stream hash still matches and verify reports "OK", leaving
// a self-contradictory archive treated as intact. This check is just addition; cheap.
func (a *archive) checkLogical() {
	for _, fe := range a.files {
		if fe.isDir || fe.isLink {
			continue // 目录不产生块; 链接的 size 记的是目标字符串长度
		}
		var sum uint64
		for _, ix := range fe.chunks {
			sum += uint64(a.chunks[ix].uncomp)
		}
		if sum != fe.size {
			fatal("metadata contradicts data: file %s declares %d B but its chunks total %d B",
				fe.name, fe.size, sum)
		}
	}
}

// Entry-count consistency: the trailer's nFiles/nChunks must match the header (call after parsing metadata)
func (a *archive) checkCounts() {
	const trailerLen = 20
	fi, err := a.src.Stat()
	if err != nil {
		return
	}
	tb := make([]byte, trailerLen)
	if _, err := a.src.ReadAt(tb, fi.Size()-trailerLen); err != nil {
		fatal("read trailer: %v", err)
	}
	nf := binary.LittleEndian.Uint32(tb[12:16])
	nc := binary.LittleEndian.Uint32(tb[16:20])
	if nf != uint32(len(a.files)) || nc != uint32(len(a.chunks)) {
		fatal("trailer disagrees with header (files %d!=%d or chunks %d!=%d): corrupt archive",
			nf, len(a.files), nc, len(a.chunks))
	}
}

// addProgress accumulates decompressed bytes and reports at thresholds. Unpacking
// hundreds of MB takes tens of seconds; total silence reads as a hang (pack side has
// had this for a while; unpack side was missing it).
func (a *archive) addProgress(n uint64) {
	if !a.progShow || a.progTotal == 0 || a.progDone >= a.progTotal {
		return // 已报完 100% 就别再打了(后续 0 字节的目录条目还会进来)
	}
	a.progDone += n
	if a.progDone-a.progLast >= 16<<20 || a.progDone >= a.progTotal {
		a.progLast = a.progDone
		fmt.Fprintf(os.Stderr, "\r  extracting %.0f%%   ", 100.0*float64(a.progDone)/float64(a.progTotal))
		if a.progDone >= a.progTotal {
			fmt.Fprintln(os.Stderr)
		}
	}
}

func (a *archive) ensureSolid(need uint64) {
	if a.solidEOF || int64(need) <= a.solidSize {
		return
	}
	if a.solidFile == nil {
		tf, err := os.CreateTemp("", "hcax-solid-")
		if err != nil {
			fatal("临时文件: %v", err)
		}
		a.solidFile = tf
		addCleanup(func() {
			tf.Close()
			os.Remove(tf.Name())
		})
		var r io.Reader
		var closeFn func()
		if a.spec.backend == "lzma2" {
			fr := io.NewSectionReader(a.src, a.dataStart, a.compLen)
			xr, err := xz.NewReader(fr)
			if err != nil {
				fatal("xz reader: %v", err)
			}
			r, closeFn = xr, nil
		} else {
			fr := io.NewSectionReader(a.src, a.dataStart, a.compLen)
			r, closeFn, err = a.be.decompressReader(fr, a.solidUncomp)
			if err != nil {
				fatal("decompressor: %v", err)
			}
		}
		a.solidR = r
		if closeFn != nil {
			addCleanup(closeFn)
		}
	}
	want := int64(need) - a.solidSize
	n, err := io.CopyN(a.solidFile, a.solidR, want)
	a.solidSize += n
	if err != nil {
		if err == io.EOF {
			a.solidEOF = true
		} else {
			fatal("decompress solid stream: %v", err)
		}
	}
}

// Decompress the solid stream fully (verification and other full-data scenarios)
func (a *archive) drainSolid() {
	for !a.solidEOF {
		a.ensureSolid(uint64(a.solidSize) + 1<<20)
	}
}

// Pre-decompress up to the max offset needed by this batch of files, so later
// readChunk calls never trigger decompression. Extracting a few early files skips the
// cost of decompressing the whole stream.
func (a *archive) prepareFor(files []fileEntry) {
	var maxEnd uint64
	for _, fe := range files {
		for _, ix := range fe.chunks {
			if ix >= uint32(len(a.chunks)) {
				continue
			}
			cm := a.chunks[ix]
			if cm.stored {
				continue
			}
			if e := cm.offset + uint64(cm.uncomp); e > maxEnd {
				maxEnd = e
			}
		}
	}
	if maxEnd > 0 {
		a.ensureSolid(maxEnd)
	}
}

func (a *archive) readChunk(idx uint32, verify bool) []byte {
	cm := a.chunks[idx]
	out := make([]byte, cm.uncomp)
	if cm.stored {
		// Raw region: no decompression, read straight from the archive by offset —
		// zero extra memory
		if _, err := a.src.ReadAt(out, a.rawOff+int64(cm.offset)); err != nil {
			fatal("read raw region: %v", err)
		}
	} else {
		a.ensureSolid(cm.offset + uint64(cm.uncomp))
		if _, err := a.solidFile.ReadAt(out, int64(cm.offset)); err != nil {
			fatal("read solid stream: %v", err)
		}
	}
	out = applyXform(cm.xform, out, true)
	if verify && a.hashLen > 0 { // legacy: per-chunk verify; v6 uses stream-level (verifyStream)
		h := hash16(out)
		n := a.hashLen
		if n > 16 {
			n = 16
		}
		if !bytes.Equal(h[:n], cm.hash[:n]) {
			fatal("chunk %d verify failed (hash mismatch)", idx)
		}
	}
	return out
}

// Stream-level verification (v6): checks the decompressed solid stream and raw region
func (a *archive) verifyStream() {
	if a.hashLen != 0 {
		return
	}
	if a.compLen > 0 {
		a.drainSolid() // 校验需要全量数据
		a.solidFile.Seek(0, io.SeekStart)
		h := hashReader(a.solidFile)
		if !bytes.Equal(h[:8], a.solidHash[:]) {
			fatal("solid stream verify failed (data corruption)")
		}
	}
	if a.rawLen > 0 {
		h := hashReader(io.NewSectionReader(a.src, a.rawOff, a.rawLen))
		if !bytes.Equal(h[:8], a.rawHash[:]) {
			fatal("raw region verify failed (data corruption)")
		}
	}
}

func (a *archive) extractFile(fe fileEntry, outDir string, verify bool) {
	if !safeName(fe.name) {
		fatal("illegal path: %s", fe.name)
	}
	defer a.addProgress(fe.size) // 所有 return 分支都要计入进度
	target := joinOut(outDir, fe.name)
	// Overwrite count: unpacking into a non-empty dir silently replaces same-named
	// entries. tar does too, but the user should know "how much existing state this
	// unpack touched" — otherwise they assume an empty dir and never notice later.
	// Dir entries are excluded: MkdirAll on an existing dir is a no-op (not a replace),
	// and dirs usually already exist from creating file parents, counting them would
	// flag first-time unpacks as overwrites.
	if !fe.isDir {
		if _, err := os.Lstat(target); err == nil {
			a.overwrote++
		}
	}
	// Directory entries (incl. empty dirs): create writable first, remember mode and
	// mtime, apply after all entries (including child files) are written — otherwise
	// writing files fails (read-only dir) and mtime is bumped to "now".
	if fe.isDir {
		if err := mkdirUnderOut(outDir, target); err != nil {
			fatal("mkdir: %v", err)
		}
		a.dirTodos = append(a.dirTodos, dirTodo{path: target, mode: fe.mode, nano: entryNano(fe)})
		return
	}
	// Symlink (v9): rebuild the link itself, no content.
	// Note: Go's stdlib has no lutimes, so a link's own mtime cannot be restored
	// (only regular files/dirs get theirs).
	if fe.isLink {
		if err := mkdirUnderOut(outDir, filepath.Dir(target)); err != nil {
			fatal("mkdir: %v", err)
		}
		os.Remove(target) // 覆盖同名已存在的文件/链接
		if err := os.Symlink(fe.link, target); err != nil {
			fatal("symlink %s: %v", fe.name, err)
		}
		return
	}
	if err := mkdirUnderOut(outDir, filepath.Dir(target)); err != nil {
		fatal("mkdir: %v", err)
	}
	// Hard link (v11): restore as a shared-inode hard link when possible. If the
	// source is outside this unpack's selection (extract of a subset), fall back to a
	// regular file — the chunk indices are still there, content is complete, never a
	// hollow shell.
	if fe.isHard {
		src := joinOut(outDir, fe.link)
		if _, e := os.Lstat(src); e == nil {
			os.Remove(target)
			if e := os.Link(src, target); e == nil {
				return
			}
		}
	}
	// Prevent "write-through a pre-created link": if the archive places a symlink at
	// target first (e.g. -> /etc), a later same-named file entry through os.Create
	// would follow the link and write into the pointed-to directory. Remove any
	// existing symlink before writing the file, keeping writes inside the unpack dir.
	if li, e := os.Lstat(target); e == nil && li.Mode()&os.ModeSymlink != 0 {
		os.Remove(target)
	}
	out, err := os.Create(target)
	if err != nil {
		fatal("create %s: %v", target, err)
	}
	// Close must be checked too: data sits in kernel page cache; a successful Write
	// before Close does not mean it hit the disk
	defer func() {
		if err := out.Close(); err != nil {
			fatal("close %s failed (disk may be full): %v", fe.name, err)
		}
	}()
	// File-level raster transform (v7): the transform spans chunks, so assemble the
	// whole file before inverting
	if len(fe.chunks) > 0 && fe.xform >= xfRasterMed && fe.xform <= xfRasterRowRctMed {
		// This path needs the **whole file in memory**. Pack side gates on
		// rasterMaxBytes, so a written archive cannot exceed it; exceeding it means
		// corruption or malice (declare a huge "raster", content all zeros — a few KB
		// of archive can make someone eat tens of GB of RAM). Reject before reading chunks.
		if fe.size > rasterMaxBytes {
			fatal("file %s declares %d B, over the raster transform cap (%d B): corrupt archive",
				fe.name, fe.size, rasterMaxBytes)
		}
		buf := []byte{}
		for _, ix := range fe.chunks {
			buf = append(buf, a.readChunk(ix, verify)...)
		}
		if !rasterApply(fe.xform, buf, false) {
			fatal("file %s: raster inverse transform failed", fe.name)
		}
		if uint64(len(buf)) != fe.size {
			fatal("file %s size mismatch: expected %d got %d", fe.name, fe.size, len(buf))
		}
		mustWrite(out, buf, fe.name)
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, entryTime(fe), entryTime(fe))
		return
	}
	if len(fe.chunks) > 0 {
		first := a.readChunk(fe.chunks[0], verify)
		// Legacy (v2~v6): old archives have no file-level transform record; invert
		// assuming "BMP was 2-D predicted". Since v7 the transform is explicit in
		// fe.xform — no heuristics (else files deliberately left untransformed would
		// be wrongly inverted).
		if a.ver < 7 && len(first) >= 2 && first[0] == 'B' && first[1] == 'M' {
			buf := append([]byte{}, first...)
			for _, ix := range fe.chunks[1:] {
				buf = append(buf, a.readChunk(ix, verify)...)
			}
			if _, ok := bmpInfoLegacy(buf); ok {
				pred2DLegacy(buf, false) // 旧版用的是二维梯度预测
			}
			if uint64(len(buf)) != fe.size {
				fatal("file %s size mismatch: expected %d got %d", fe.name, fe.size, len(buf))
			}
			mustWrite(out, buf, fe.name)
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, entryTime(fe), entryTime(fe))
			return
		}
		mustWrite(out, first, fe.name)
		if uint64(len(first)) == fe.size {
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, entryTime(fe), entryTime(fe))
			return
		}
		var written2 uint64 = uint64(len(first))
		for _, ix := range fe.chunks[1:] {
			cb := a.readChunk(ix, verify)
			mustWrite(out, cb, fe.name)
			written2 += uint64(len(cb))
		}
		if written2 != fe.size {
			fatal("file %s size mismatch: expected %d got %d", fe.name, fe.size, written2)
		}
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, entryTime(fe), entryTime(fe))
		return
	}
	var written uint64
	if written != fe.size {
		fatal("file %s size mismatch: expected %d got %d", fe.name, fe.size, written)
	}
	os.Chmod(target, os.FileMode(fe.mode))
	os.Chtimes(target, entryTime(fe), entryTime(fe))
}

// Normalize a user-supplied extract target: backslashes as separators, strip trailing
// separators, so "d/sub" and "d/sub/" mean the same thing.
func normAsk(s string) string {
	s = filepath.ToSlash(s)
	// In-archive names are always relative, but shell completion / automation often
	// produces "./" or even "/" prefixes — without normalization, pasting an ls line
	// extracts nothing and "no match" is baffling (the entry is right there in list).
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	s = strings.TrimPrefix(s, "/")
	// 目录名后面的斜杠去掉(原来就有)
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}

// Does an in-archive entry match one of the user's extract targets? Three match kinds
// (precise to loose):
//  1. full path equal:     "d/sub/r.bin"
//  2. basename equal:      "r.bin"   — fallback when the full path is not remembered
//  3. directory prefix:    "d/sub"   — extracts that subtree (incl. dir entries under it)
//
// The old implementation only supported 1)2), so extracting d/sub created an empty
// d/sub dir while none of its files came out; the user assumed success and data in hand.
func matchEntry(name, ask string) bool {
	n := filepath.ToSlash(name)
	if n == ask {
		return true
	}
	if path.Base(n) == ask {
		return true
	}
	return strings.HasPrefix(n, ask+"/")
}

// If no user target matched at all, it is almost certainly a typo — a silent
// "extracted: 0 files" reads as success. Total miss errors out; partial miss warns
// (common with batch extraction).
func reportUnmatched(asks []string, hit map[string]bool) {
	var miss []string
	for _, p := range asks {
		if !hit[p] {
			miss = append(miss, p)
		}
	}
	if len(miss) == 0 {
		return
	}
	msg := "no entries matching " + strings.Join(miss, ", ") + " in the archive"
	if len(miss) == len(asks) {
		fatal("%s (use list to see real in-archive paths)", msg)
	}
	fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
}

// Only show progress when "big enough": tiny archives flash by, progress is noise
func (a *archive) setProgress(items []fileEntry) {
	var tot uint64
	for _, fe := range items {
		tot += fe.size
	}
	if tot < 32<<20 {
		return
	}
	a.progShow = true
	a.progTotal = tot
}

// Directory modes and mtimes are applied at the end (see archive.dirTodos).
// Order: deepest first — if a parent's mode blocks a child dir, the child is already done.
func (a *archive) applyDirMeta() {
	todos := a.dirTodos
	a.dirTodos = nil
	sort.SliceStable(todos, func(i, j int) bool {
		return len(todos[i].path) > len(todos[j].path)
	})
	for _, d := range todos {
		if d.mode != 0 {
			if err := os.Chmod(d.path, os.FileMode(d.mode)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot set directory mode %s: %v\n", d.path, err)
			}
		}
		if d.nano != 0 {
			t := time.Unix(0, d.nano)
			if err := os.Chtimes(d.path, t, t); err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot set directory time %s: %v\n", d.path, err)
			}
		}
	}
}

// Create a directory inside the unpack dir, removing any symlink encountered along
// the way.
//
// os.MkdirAll **follows** symlinks: a malicious archive can place an entry
// link -> /etc (or -> ../..) then a "link/xxx" file/dir entry; MkdirAll walks the
// link and creates directories outside the unpack dir, and the subsequent os.Create
// writes through. The old check only looked at whether target itself was a link
// before writing files; it did not stop "a link in the middle of the path".
//
// "Entries under a symlink" is itself illegal in an archive (pack-side Walk does not
// follow links, so it cannot produce such a structure); removing the blocking link is
// therefore consistent with the file-write handling above.
func mkdirUnderOut(outDir, target string) error {
	root := filepath.Clean(outDir)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes the unpack dir: %s", target)
	}
	cur := root
	rest := strings.TrimPrefix(target, root)
	for _, seg := range strings.Split(rest, string(os.PathSeparator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		if li, e := os.Lstat(cur); e == nil && li.Mode()&os.ModeSymlink != 0 {
			if e := os.Remove(cur); e != nil {
				return e
			}
		}
	}
	return os.MkdirAll(target, 0o755)
}

func overwriteNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("  (overwrote %d existing entries)", n)
}

func unpack(archivePath, outDir string, verify bool, only []string) {
	a := openArchive(archivePath)
	if verify {
		a.verifyStream() // v6 流级校验(旧版为空操作, 由 readChunk 逐块校验)
	}
	os.MkdirAll(outDir, 0o755)
	a.setProgress(a.files)
	if len(only) == 0 {
		// Full unpack: read in order, the solid stream advances on demand
		for _, fe := range a.files {
			a.extractFile(fe, outDir, verify)
		}
		a.applyDirMeta()
		runCleanups()
		fmt.Printf("unpacked: %d entries -> %s  mode=%s verify=%v%s\n",
			len(a.files), outDir, modeName(a.spec.code), verify, overwriteNote(a.overwrote))
		return
	}
	asks := make([]string, 0, len(only))
	for _, o := range only {
		asks = append(asks, normAsk(o))
	}
	var want []fileEntry
	hit := make(map[string]bool, len(asks))
	for _, fe := range a.files {
		for _, p := range asks {
			if matchEntry(fe.name, p) {
				want = append(want, fe)
				hit[p] = true
				break // 一个条目只解一次(多个条件同时命中时不重复)
			}
		}
	}
	reportUnmatched(asks, hit)
	a.setProgress(want)
	// Only decompress up to the selected files' chunk ends: extracting a few early
	// files need not decompress the whole solid stream
	a.prepareFor(want)
	for _, fe := range want {
		a.extractFile(fe, outDir, verify)
	}
	a.applyDirMeta()
	runCleanups()
	fmt.Printf("extracted: %d files -> %s  mode=%s verify=%v%s\n",
		len(want), outDir, modeName(a.spec.code), verify, overwriteNote(a.overwrote))
}
