package main

// read path for legacy (v2-v5) archives: every live test packs/unpacks **v6+ only**,
// so that ~90-line compat code never runs -- a break there would go unnoticed.
// hand-craft v3 / v5 byte streams per FORMAT.md and feed them straight to unpack.
// this also cross-checks the FORMAT.md layout against code.

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

// build a v3 (no dict region) or v5 (dict region, zero dicts here) archive.
// layout: [header 32B][data][raw][chunk table][file table][(v5) dicts][tail 20B]
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

	// chunk table: hash16 + offset + uncomp + stored [+xform(v4+)] [+dictID(v5+)]
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

	// file table: nameLen + name + size + mtime + mode + nChunks + chunks
	var total uint64
	for i, f := range files {
		nb := []byte(f.name)
		binary.Write(&b, binary.LittleEndian, uint16(len(nb)))
		b.Write(nb)
		binary.Write(&b, binary.LittleEndian, uint64(len(f.data)))
		binary.Write(&b, binary.LittleEndian, uint64(1700000000))
		binary.Write(&b, binary.LittleEndian, f.mode)
		binary.Write(&b, binary.LittleEndian, uint32(1)) // 1 chunk per file
		binary.Write(&b, binary.LittleEndian, uint32(i))
		total += uint64(len(f.data))
	}

	if ver == 5 { // v5 always has a dict region; this case uses zero dicts
		binary.Write(&b, binary.LittleEndian, uint32(0))
	}
	b.WriteString(trailerMag)
	binary.Write(&b, binary.LittleEndian, total)
	binary.Write(&b, binary.LittleEndian, uint32(len(files)))
	binary.Write(&b, binary.LittleEndian, uint32(len(chunks)))
	return b.Bytes()
}

func TestReadLegacyArchive(t *testing.T) {
	// two files: one via a compressed chunk (compressible text), one via raw (random)
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
		// list must read it
		if runCatchingFatal(func() { listArchive(arc, false) }) {
			t.Fatalf("v%d: list failed", ver)
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, true, nil) }) {
			t.Fatalf("v%d: unpack failed", ver)
		}
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(out, f.name))
			if err != nil {
				t.Fatalf("v%d: reading back %s: %v", ver, f.name, err)
			}
			if !bytes.Equal(got, f.data) {
				t.Errorf("v%d: %s content mismatch (%d vs %d bytes)", ver, f.name, len(got), len(f.data))
			}
		}
	}
}

// legacy versions verify per-chunk 16B hashes (v6+ moved to stream-level). A flipped chunk must be caught.
func TestLegacyChunkHashChecked(t *testing.T) {
	text := bytes.Repeat([]byte("legacy hash check. "), 50)
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	comp := enc.EncodeAll(text, nil)
	enc.Close()

	h := hash16(text)
	h[0] ^= 0xff // deliberately corrupt the hash
	chunks := []chunkMeta{{hash: h, uncomp: uint32(len(text)), offset: 0}}
	files := []legacyFile{{name: "a.txt", data: text, mode: 0644}}
	blob := buildLegacyArchive(t, 3, comp, nil, files, chunks)

	dir := t.TempDir()
	arc := filepath.Join(dir, "bad.hcax")
	os.WriteFile(arc, blob, 0o644)
	// chunk hash mismatch -> must fail cleanly, not emit wrong data
	if !runCatchingFatal(func() { unpack(arc, filepath.Join(dir, "out"), true, nil) }) {
		t.Error("chunk hash mismatch went unreported -- legacy archive verification is a no-op")
	}
}
