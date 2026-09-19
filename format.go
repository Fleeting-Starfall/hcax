package main

// Container format read/write: entry types, metadata serialization/parsing, version
// decisions. Byte-level layout is in FORMAT.md.

import (
	"bytes"
	"encoding/binary"
	"os"
)

type chunkMeta struct {
	hash   [16]byte
	offset uint64 // comp blocks: offset in compSolid; stored blocks: offset in rawRegion
	uncomp uint32
	stored bool // true = store raw (skip the compressor)
	xform  byte // preprocessing type (DELTA/BCJ), inverted on decode
	dictID byte // training dict index (best for similar files); 0 = none
	data   []byte
	idx    uint32
}

// File identity: (device, inode) uniquely identifies a file; nlink>1 means it has
// multiple names (hard links). Single-name files are not recorded — otherwise every
// file would store a useless key.
type fileIDKey struct {
	dev, ino, nlink uint64
}

type fileEntry struct {
	name   string
	size   uint64
	mtime  uint64 // Unix seconds (display only; v10 precise time lives in nano)
	nano   int64  // v10: Unix nanoseconds. 0 = no timestamp available for this entry
	mode   uint32 // permission bits + setuid/setgid/sticky (Go os.FileMode bit layout)
	chunks []uint32
	isDir  bool // directory entry (empty dirs are kept)
	isLink bool // v9: symlink entry (stores target, not content)
	isHard bool // v11: hard link entry (reuses the first entry's chunk indices)
	link   string
	// xform: file-level preprocessing flag (v7). Raster images (BMP/TGA/PNM) get
	// 2-D prediction / color decorrelation before chunking; the transform spans chunks,
	// so it must live at file level, not chunk level. Non-raster files are xfNone.
	xform byte
}

// Metadata serialization (v6): [stream hash 16B] + chunk table (5B/entry: len4 + flags1)
// + file table.
//   - Chunk offsets are no longer stored: comp chunks in the solid stream and stored
//     chunks in the raw region are packed sequentially; unpack accumulates offsets.
//   - No per-chunk hashes (8B of random data per chunk is incompressible and costly
//     with many small files); instead stream-level checks: 8B hash each for the solid
//     stream and the raw region, verified over the whole stream on verify.
func serializeMeta(chunks []*chunkMeta, files []fileEntry, solidHash, rawHash [8]byte, ver byte) []byte {
	var b bytes.Buffer
	b.Write(solidHash[:])
	b.Write(rawHash[:])
	for _, cm := range chunks {
		binary.Write(&b, binary.LittleEndian, cm.uncomp)
		var f byte
		if cm.stored {
			f |= 1
		}
		f |= (cm.xform & 0x0f) << 1
		if cm.dictID != 0 {
			f |= 0x20
		}
		b.WriteByte(f)
	}
	for _, fe := range files {
		nb := []byte(fe.name)
		binary.Write(&b, binary.LittleEndian, uint16(len(nb)))
		b.Write(nb)
		var f byte
		if fe.isDir {
			f |= 1
		}
		f |= (fe.xform & 0x0f) << 1 // v7: file-level raster transform
		if fe.isLink {
			f |= 0x20 // v9: symlink
		}
		if fe.isHard {
			f |= 0x40 // v11: hard link
		}
		b.WriteByte(f)
		binary.Write(&b, binary.LittleEndian, fe.size)
		if ver >= 10 {
			// v10: nanosecond timestamp + full mode bits (incl. setuid/setgid/sticky)
			binary.Write(&b, binary.LittleEndian, entryNano(fe))
			binary.Write(&b, binary.LittleEndian, fe.mode)
		} else {
			binary.Write(&b, binary.LittleEndian, uint32(fe.mtime))
			binary.Write(&b, binary.LittleEndian, uint16(fe.mode&0o777))
		}
		binary.Write(&b, binary.LittleEndian, uint32(len(fe.chunks)))
		for _, ix := range fe.chunks {
			binary.Write(&b, binary.LittleEndian, ix)
		}
		if fe.isLink || fe.isHard { // v9 link target / v11 in-archive path of the hard link
			lb := []byte(fe.link)
			binary.Write(&b, binary.LittleEndian, uint16(len(lb)))
			b.Write(lb)
		}
	}
	return b.Bytes()
}

func need(m []byte, pos, n int, what string) {
	if pos < 0 || n < 0 || pos+n > len(m) {
		fatal("corrupt metadata: %s parse out of bounds (offset %d needs %d bytes, metadata has %d)", what, pos, n, len(m))
	}
}

func parseMeta(m []byte, nChunks, nFiles uint32, ver byte) ([]chunkMeta, []fileEntry, [8]byte, [8]byte) {
	pos := 0
	var sh, rh [8]byte
	copy(sh[:], m[pos:pos+8])
	pos += 8
	copy(rh[:], m[pos:pos+8])
	pos += 8
	chunks := make([]chunkMeta, nChunks)
	var compOff, rawOff uint64
	for i := range chunks {
		need(m, pos, 5, "chunk table entry")
		chunks[i].uncomp = binary.LittleEndian.Uint32(m[pos : pos+4])
		pos += 4
		// A chunk's declared length must fit our own chunker's bounds (CDC max 256KB,
		// ultra whole-file solid max 256MB). Corrupt or malicious archives can set
		// 0xFFFFFFFF, and readChunk does make([]byte, uncomp) before discovering short
		// data — a 1KB archive could force a 4GB allocation. This check rejects huge
		// allocations before any read.
		if chunks[i].uncomp > maxChunkUncomp {
			fatal("chunk %d declares %d B, over the sane bound (%d B): corrupt archive",
				i, chunks[i].uncomp, maxChunkUncomp)
		}
		f := m[pos]
		pos++
		chunks[i].stored = f&1 != 0
		chunks[i].xform = (f >> 1) & 0x0f
		if f&0x20 != 0 {
			chunks[i].dictID = 1
		}
		if chunks[i].stored {
			chunks[i].offset = rawOff
			rawOff += uint64(chunks[i].uncomp)
		} else {
			chunks[i].offset = compOff
			compOff += uint64(chunks[i].uncomp)
		}
	}
	files := make([]fileEntry, nFiles)
	for i := range files {
		need(m, pos, 2, "file name length")
		nl := int(binary.LittleEndian.Uint16(m[pos : pos+2]))
		pos += 2
		need(m, pos, nl+1+8+4+2+4, "file table entry")
		files[i].name = string(m[pos : pos+nl])
		pos += nl
		f := m[pos]
		pos++
		files[i].isDir = f&1 != 0
		if ver >= 7 {
			files[i].xform = (f >> 1) & 0x0f
		}
		files[i].isLink = ver >= 9 && f&0x20 != 0
		files[i].isHard = ver >= 11 && f&0x40 != 0
		files[i].size = binary.LittleEndian.Uint64(m[pos : pos+8])
		pos += 8
		if ver >= 10 {
			need(m, pos, 12, "file table entry (v10 time/mode)")
			files[i].nano = int64(binary.LittleEndian.Uint64(m[pos : pos+8]))
			pos += 8
			files[i].mode = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
			if files[i].nano != 0 {
				files[i].mtime = uint64(files[i].nano / 1e9)
			}
		} else {
			files[i].mtime = uint64(binary.LittleEndian.Uint32(m[pos : pos+4]))
			pos += 4
			files[i].mode = uint32(binary.LittleEndian.Uint16(m[pos : pos+2]))
			pos += 2
		}
		nc := int(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		need(m, pos, 4*nc, "file chunk indices")
		// Do not compare the file's chunk count with the chunk table size: after
		// dedup one chunk is referenced many times, so references can exceed unique
		// chunks (a 35MB all-zero file has 2 unique chunks but 134 references). The old
		// check called this corruption, locking archives with high internal redundancy
		// out of unpack. Only bound-check each index (loop below).
		idxs := make([]uint32, nc)
		for j := range idxs {
			idxs[j] = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
			if idxs[j] >= nChunks {
				fatal("corrupt metadata: file %s references nonexistent chunk %d (of %d)", files[i].name, idxs[j], nChunks)
			}
		}
		files[i].chunks = idxs
		if files[i].isLink || files[i].isHard {
			need(m, pos, 2, "link target length")
			tl := int(binary.LittleEndian.Uint16(m[pos : pos+2]))
			pos += 2
			need(m, pos, tl, "link target")
			files[i].link = string(m[pos : pos+tl])
			pos += tl
		}
	}
	return chunks, files, sh, rh
}

const (
	magic      = "HCAX"
	trailerMag = "XACH"

	// Reads v2~v12 for compatibility. When writing, avoid bumping versions whenever
	// possible — each bump breaks older binaries:
	//   plain archives write v8; symlinks write v9; setuid/setgid/sticky or post-2038
	//   timestamps write v10; hard links write v11; row raster transforms (8/9) write
	//   v12 — older binaries treat unknown transforms as errors, so reject at open time.
	version   = 12
	verCompat = 8
	verLinks  = 9  // requires symlink entries
	verExt    = 10 // requires extended time/permissions
	verHard   = 11 // requires hard link entries
	verRow    = 12 // requires row raster transforms (xform 8/9)

	headerSize = 24 // v2 header length; v3+ is 32 (reads 8 more bytes of rawLen)

	// Upper bound for a single chunk's declared length, from ultra mode
	// newChunkerMinMax(nil, 4MB, 256MB) — our own chunks cannot exceed it.
	maxChunkUncomp = 256 << 20
)

// Which version to write? Scan every entry: if any uses a feature that needs a newer
// version, bump the whole container (there is one version number, not per-entry).
// precise = user explicitly requested nanosecond timestamps (-T/--precise-times)
func writeVer(files []fileEntry, precise bool) byte {
	if precise {
		return verExt
	}
	v := byte(verCompat)
	for i := range files {
		fe := &files[i]
		if fe.isLink && v < verLinks {
			v = verLinks
		}
		if fe.isHard && v < verHard {
			v = verHard
		}
		if fe.xform >= xfRasterRowMed && v < verRow {
			v = verRow
		}
		if v >= verExt {
			continue
		}
		// v9 and earlier: mode is 0777 only and time is uint32 seconds — bump only when
		// these cannot hold the value
		if fe.mode&^uint32(os.ModePerm) != 0 || !fitsOldTime(fe.nano) {
			v = verExt
		}
	}
	return v
}

// Sub-second precision is not a reason to bump the version: nearly every real file has
// a sub-second timestamp (APFS/ext4 are nanosecond), so always writing v10 would cut
// every new archive off from older binaries — not worth it for a bit of precision.
// Store seconds by default (like tar); keep nanoseconds only with explicit -T or when
// the archive already needs v10.
func fitsOldTime(nano int64) bool {
	if nano == 0 {
		return true // no timestamp available; storing 0 needs no version bump
	}
	sec := nano / 1e9
	return sec >= 0 && sec <= 0xFFFFFFFF
}
