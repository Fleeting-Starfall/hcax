# hcax Container Format Specification

This document describes the **byte-level layout** of `.hcax` archives. Implementation:
`format.go` (container read/write + version constants), `pack.go` (write), `unpack.go` (read),
`backend.go` (backends + adaptive params), `chunker.go` (CDC chunking),
`raster.go` + `preprocess.go` (transforms).
Current write version **v12**, readable versions **v2 ~ v12**.

Conventions: all integers are **little-endian**, no padding. Path separators are recorded per
the packing platform (Unix: `/`); unpack converts with `filepath.FromSlash`.

---

## 1. Overall Layout

```
+---------------------------+
| Header (v6+: 48B)          |
+---------------------------+
| Metadata frame (metaCompLen)|  compressed chunk table + file table
+---------------------------+
| Data section (compLen)     |  solid stream, backend-compressed (may be multiple
|                           |  concatenated frames)
+---------------------------+
| Raw section (rawLen)       |  chunks judged "incompressible", stored as-is
+---------------------------+
| Dictionary section (opt)   |
+---------------------------+
| Tail (20B)                 |
+---------------------------+
```

---

## 2. Header

### v6 and later (48 bytes)

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 4 | magic | `HCAX` |
| 4 | 1 | version | format version |
| 5 | 1 | code | mode code: 0=fast 1=best 2=max 3=ultra 4=text |
| 6 | 1 | window | zstd window log₂ (decoder must be built with it to read zstd frames). fast/text: 0
  (encoder default/minimal window); others: 27. **Unused by lzma2 tiers** — dict size is decided
  at pack time by `dictFor(compressible size)` and written into the xz frame header; decoder
  reads it from there |
| 7 | 1 | — | reserved, always 0 |
| 8 | 8 | metaCompLen | compressed metadata frame length |
| 16 | 8 | metaRawLen | metadata decompressed length |
| 24 | 8 | compLen | data section length |
| 32 | 8 | rawLen | raw section length |
| 40 | 4 | nChunks | chunk table entry count |
| 44 | 4 | nFiles | file table entry count |

### v2 ~ v5 (shorter, see §6)

---

## 3. Metadata (metaRawLen bytes after decompression)

### 3.1 Stream checksums (16B)

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | solidHash — first 8 bytes of sha256 of the decompressed **solid stream** |
| 8 | 8 | rawHash — first 8 bytes of sha256 of the **raw section** |

> v5 and earlier stored a 16B random hash per chunk. Those hashes were incompressible and could
> reach half the archive with many small files; v6 moved to stream-level checksums
> (see `README.md` §4.4b).

### 3.2 Chunk table (5B each)

| Size | Field | Notes |
|---|---|---|
| 4 | uncomp | decompressed chunk length |
| 1 | flags | bit0 = stored (raw) <br>bits1–4 = xform (preprocessing type) <br>bit5 = dictID (uses trained dictionary) |

**No offsets stored**: compressible chunks are sequentially packed in the solid stream, raw
chunks in the raw section; unpack accumulates `uncomp`. This cut the table from 31 B/entry to
5 B/entry.

### 3.3 File table (variable)

| Size | Field | Notes |
|---|---|---|
| 2 | nameLen | name length |
| nameLen | name | in-archive relative path |
| 1 | flags | bit0 = isDir <br>bits1–4 = xform (v7+, file-level raster transform) <br>bit5 = isLink (v9+) <br>bit6 = isHard (v11+) |
| 8 | size | original size (symlinks: target string length) |
| 4 / 8 | mtime | v≤9: Unix seconds (uint32) <br>v10+: Unix **nanoseconds** (int64; 0 = no timestamp) |
| 2 / 4 | mode | v≤9: low 12 permission bits (uint16) <br>v10+: uint32 incl. setuid / setgid / sticky |
| 4 | nChunks | chunks referenced by this file |
| 4×nChunks | chunks | chunk indices (must be < nChunks) |
| 2 + tl | link | **only isLink (v9+) / isHard (v11+)**: target length + target string. For isHard,
  target is the **in-archive path of the first name**; unpack restores shared inode via `os.Link` |

---

## 4. Data & Raw Sections

- **Data section**: all non-stored chunks concatenated in file order into one **solid stream**,
  compressed as a whole by the backend. `max` mode parallel-groups when compressible data
  ≥ 64 MiB, producing **multiple concatenated xz frames**; decode in stream order (frames are
  self-describing, dict embedded in frame header).
- **Raw section**: stored chunks concatenated as-is — no compression, no frame headers.

### Backends

| code | Backend | Notes |
|---|---|---|
| 0/1 | zstd | may use a trained dictionary (dictionary section) |
| 2/3 | lzma2 | xz container; system `xz` for compression, pure Go for decompression |
| 4 | cm | in-house context mixing (see `cm.go`) |

### Dictionary section

```
uint32 count      // 0 or 1
repeat count times:
  uint32 len
  bytes  dict     // zstd dictionary body
```

Located after the raw section, before the tail. **The dictionary must be loaded before
decompressing the metadata frame**, or zstd reports `unknown dictionary`.

---

## 5. Tail (20B, identical across versions)

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `XACH` |
| 4 | 8 | totalUncomp (original total bytes) |
| 12 | 4 | nFiles |
| 16 | 4 | nChunks |

The tail is the **truncation check**: before reading the data section, verify file length and
magic; after parsing metadata, cross-check counts against the header
(see `checkTailMagic` / `checkCounts`).

---

## 6. Version History

| Version | Change |
|---|---|
| v2 | 24B header; chunk table 28B/entry (hash16 + offset + uncomp + stored) |
| v3 | 32B header, added rawLen (raw section); chunk table 29B/entry |
| v4 | chunk table 30B/entry, added xform |
| v5 | chunk table 31B/entry, added dictID; added dictionary section |
| v6 | 48B header; **metadata compressed**; chunk table cut to 5B/entry, offsets dropped; stream-level checksums |
| v7 | file table gains xform (file-level raster transform); heuristic "BM → inverse transform" removed |
| v8 | no format change (pack memory fix) |
| v9 | file table flag bit5 = isLink + link target field (symlinks) |
| v10 | file table mtime → int64 nanoseconds, mode → uint32 (setuid/setgid/sticky restored) |
| v11 | file table flag bit6 = isHard, link field reused for "first name" (hardlinks) |
| v12 | xform gains 8/9 **row-wise** raster transforms (6/7 flat versions kept read-only) |

### Write-version policy

`version = 12`, `verCompat = 8`, `verLinks = 9`, `verExt = 10`, `verHard = 11`, `verRow = 12`.

**Upgrade only on demand** — every bump makes old binaries unable to read the archive:

- Plain archive → v8 (old binaries still unpack)
- Contains symlinks → v9
- Contains setuid/setgid/sticky, or timestamps outside uint32 seconds (post-2038 / pre-1970) → v10
- User explicitly passes `-T / --precise-times` → v10 (nanosecond timestamps)
- Contains hardlinks → v11
- Contains row-wise raster transforms (xform 8/9, i.e. touched uncompressed bitmaps) → v12

**Sub-second precision alone never triggers an upgrade**: virtually every real file has
nanosecond mtimes (APFS/ext4 both do); upgrading unconditionally would insulate every new
archive from old binaries. So seconds are stored by default (same as tar); nanoseconds are kept
only when the archive is upgrading to v10 anyway. Old binaries reading link-bearing archives get
`version incompatible: 9` — **explicit failure, never wrong data**.

### Compatibility guarantees

- Read: v2 ~ v12 all readable (old chunk tables use per-chunk 16B hash verification; v6+ uses
  stream-level)
- Write: defaults to v8 (compat-first), upgrades to v9/v10/v11/v12 on demand
- Decompression is always pure Go, zero external deps; only `max`/`ultra` **compression** uses
  system `xz`, falling back to pure Go when missing

---

## 7. Transform Types (xform)

| Value | Name | Notes |
|---|---|---|
| 0 | none | no transform |
| 1 | delta1 | 8-bit differencing |
| 2 | delta2 | 16-bit differencing |
| 3 | delta3 | 24-bit differencing |
| 4 | delta4 | 32-bit differencing |
| 5 | bcj-x86 | x86 jump-address transform |
| 6 | raster-med | raster: MED median edge prediction (**file-level only**, old; whole rows incl.
  padding; read-only compat) |
| 7 | raster-rct-med | raster: RCT color decorrelation + MED (**file-level only**, old; whole buffer
  sliced per 3 bytes; read-only compat) |
| 8 | raster-row-med | raster: row-wise MED, skipping row-padding (v12) |
| 9 | raster-row-rct-med | raster: row-wise RCT + row-wise MED, skipping row-padding (v12) |

1–5 are recorded **per-chunk** (each chunk independently invertible); 6–9 are **per-file** —
prediction depends on whole-image geometry, spans chunk boundaries, cannot be inverted per chunk.

The only difference between 6/7 and 8/9 is **whether BMP row padding is crossed**: BMP rows pad
to 4 bytes; with 24bpp and `width % 4 ∈ {1, 2}`, row stride is not a multiple of 3. Transform 7
slices 3-byte groups across the whole buffer, treating padding + next row's head bytes as one
"pixel": channel phase drifts per row, MED's up/upper-left neighbors land on wrong channels,
vertical prediction collapses (measured ~60% worse than no decorrelation). 8/9 operate only on
the first `rowBytes` (= width × bpp) bytes of each row; padding untouched.

---

## 8. Validation & Robustness

On open, checks run in order; any failure is an explicit error (no panic, no silent wrong data):

1. magic == `HCAX`
2. version in 2..12 (below `verCompat` or above `version` → `version incompatible`)
3. `checkTailMagic` — file long enough, tail magic correct, data section in bounds
   (**truncation detection**)
4. `checkHeaderBounds` — length fields within file size, layout within file, entry counts
   cross-validated against metadata length (**anti-OOM / absurd allocation**)
5. metadata decompressed length == `metaRawLen`
6. `parseMeta` boundary-checks every read via `need()`; chunk indices must be < nChunks
7. `checkCounts` — tail nFiles/nChunks match header

Corruption detection: flipping 1 byte is caught by `verify`; truncating 30 bytes is rejected.

### Unpack path safety (archive names are untrusted)

| Risk | Handling |
|---|---|
| Absolute paths, `..` segments, empty names | `safeName` rejects |
| Escape after join | `joinOut` rejects (prefix check) |
| Entry builds symlink at `target`, then writes same-name file | `Lstat` before write; drop if link |
| **Symlink in the middle of a path** (`link` → `/etc`, then entry `link/xxx`) | `mkdirUnderOut` `Lstat`s each segment, drops blocking links |

The last one is the easiest to miss: `os.MkdirAll` **follows** symlinks, so checking only the
final `target` doesn't stop the "link in the middle" case (directories get created outside the
unpack dir through the link, then file writes escape). Entries "below a symlink" are illegal by
construction — pack-side `filepath.Walk` doesn't follow links — so dropping them is safe.
See `security_test.go`.
