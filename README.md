# hcax — High-Ratio Lossless Archive Tool

> Pure-Go CLI lossless archiver combining **CDC dedup + solid streams + content-adaptive preprocessing + adaptive parameters + context mixing**.
> Higher ratio than **xz / 7z** on text/code/structured data (see §4 for measurements); dedicated optimization for uncompressed bitmaps.
> **Lossless = byte-identical**: unpacked files match originals byte-for-byte (verified with `cmp`).

---

## Table of Contents

1. [Quick Start](#1-quick-start)
2. [Choosing a Mode](#2-choosing-a-mode)
3. [Command Reference](#3-command-reference)
4. [Benchmarks](#4-benchmarks)
5. [How It Works](#5-how-it-works)
6. [Archive Format](#6-archive-format)
7. [Code Structure](#7-code-structure)
8. [Performance & Limitations](#8-performance--limitations)
9. [FAQ](#9-faq)
10. [License](#10-license)

---

## 1. Quick Start

```bash
# Pack (default: best mode)
hcax pack out.hcax <file-or-dir>...

# Choose a mode
hcax pack backup.hcax ~/Documents -m max

# Unpack to a directory
hcax unpack backup.hcax ./restore

# List contents (-l shows mode/time/type stats)
hcax list backup.hcax -l

# Verify integrity (stream-level hash; falls back to per-chunk for old archives)
hcax verify backup.hcax

# Inspect internal layout (byte sizes per section)
hcax info backup.hcax

# Extract a single file without full unpack
hcax extract backup.hcax ./out report.pdf

# Extract an entire subtree
hcax extract backup.hcax ./out src/lib

# Exclude paths/globs while packing (repeatable)
hcax pack src.hcax ./proj -m max --exclude .git --exclude node_modules --exclude '*.tmp'
```

**Build** (requires Go 1.27+):

```bash
go build -o hcax .
```

**Install to PATH** (any one):

```bash
# 1. go install (binary lands in $(go env GOPATH)/bin, usually already on PATH)
go install .

# 2. Or copy the built binary to a standard location
sudo cp hcax /usr/local/bin/

# 3. Or add the directory containing hcax to PATH (replace <path-to-hcax>)
echo 'export PATH="<path-to-hcax>:$PATH"' >> ~/.zshrc   # macOS / zsh
echo 'export PATH="<path-to-hcax>:$PATH"' >> ~/.bashrc  # Linux / bash
source ~/.zshrc
```

Verify: `hcax` should print usage from anywhere.

---

## 2. Choosing a Mode

| Your data | Mode | Why |
|---|---|---|
| Whole folder / directory tree | `text` or `max` | Metadata compressed too; better ratio than 7z |
| Text / code / logs / JSON | `text` | Predictive modeling; 8–16% better than xz |
| Mixed data, best ratio | `max` | lzma2 extreme params + adaptive preprocessing |
| Large archives (>64MiB), want speed | `max` | Auto-parallel solid groups, several times faster |
| Many similar files, want speed | `best` | zstd-19 + trained dictionary |
| Fastest possible | `fast` | zstd-3 |
| Uncompressed bitmaps (BMP/TGA/PNM) | `text` > `max` | Auto RCT color decorrelation + MED 2D prediction |
| Already-compressed media (JPG/PNG/MP4) | `max` (any) | At entropy limit; no lossless tool can help |

Quick reference:
- `text` = text/code specialist; smallest output, slowest
- `max` = general-purpose best (recommended default for archives)
- `ultra` = dedup + single-stream full-data parameter selection (extreme ratio, vs `max`'s parallel groups)
- `best` = fast; in-process zstd-19 + trained dict, ~230MB RSS (vs ~90MB for `max`/`ultra` which shell out to xz)
- `fast` = fastest

> **`max` / `ultra` require the system `xz` command.** If missing, hcax falls back to the built-in
> pure-Go lzma2: ratio degrades from 26.89% to 33.40% (archive ~24% larger) and a warning is printed
> to stderr. If your `max` ratio is much worse than the tables here, run `which xz` first.
> Install: `brew install xz` / `apt-get install xz-utils`. The other three modes are pure Go, no deps.

---

## 3. Command Reference

### pack

```
hcax pack <out.hcax> <file-or-dir...> [-m mode] [--exclude glob]... [-T]
```

- `-m fast|best|max|ultra|text` (default `best`)
- `--exclude <glob>`: repeatable. Matched against full path / path relative to pack root / any path
  segment. Excluded directories are skipped entirely (no traversal).
- `-T` / `--precise-times`: store timestamps in nanoseconds (bumps archive to v10; old binaries
  cannot read it. Default: seconds, same as tar).
- Multiple inputs are merged into one archive with **cross-file dedup**.
- Prints: original size → compressed size, ratio, mode, unique chunk counts (compressible/raw).
- **Directory semantics match tar/zip**: `hcax pack out.hcax dir` stores `dir/a/b/c.txt`; unpack
  restores that path. Packing a single file stores just its basename.
- **Symlinks stored as links** (not followed, target not copied); restored as links.
- **Hard links stored as hard links**: multiple names of one inode share one chunk index; unpack
  re-shares the inode (extracting one name yields a complete regular file).
- **Never packs itself**: `hcax pack arch.hcax .` skips `arch.hcax`.
- **Atomic write**: writes to a temp file in the same dir, `rename`s over the target only on
  success. Failed packs (e.g. disk full) leave the old archive untouched. Overwriting preserves
  the existing file's permissions; new archives are `0644`.
- Preserves setuid/setgid/sticky bits (v10+) and mtimes.
- Progress (>32MiB inputs) goes to stderr only; stdout stays clean for scripting.

### unpack

```
hcax unpack <in.hcax> <out-dir> [--verify]
```

- `--verify`: verify integrity while unpacking (stream-level hash; detects corruption).
- Rebuilds directory structure (incl. **empty dirs**), permissions and mtimes. Directory
  permissions/times are applied **after all entries are written** — setting them during extraction
  would be clobbered by later writes, and pre-chmod to 0555 would block child file creation.
- Overwrites same-name entries silently (like tar) but reports the overwrite count.
- **Never writes outside the target dir**: names are validated as "relative, no `..`", then
  prefix-checked after joining; the symlink-then-write-through attack is blocked (path segments
  checked per step; blocking symlinks are removed, see FORMAT.md §8).
- **Corrupt archives fail cleanly** — no hang, no panic. Length fields are treated as untrusted.
  Critical for `text` archives: CM bit-by-bit decoding has no natural end marker; the only
  terminator is the 8-byte frame length. If corrupted, decoding would run forever — now capped by
  the caller's exact expected length (metadata `metaRawLen`, solid stream per-chunk `uncomp` sum);
  exceeding it reports "archive corrupted".

### extract

```
hcax extract <in.hcax> <out-dir> [name...] [--verify]
```

- Targets may be full paths, basenames, or **directory prefixes** (whole subtree).
- `./` or `/` prefixes are normalized before matching (shell completion friendly).
- Zero matches is a **hard error** (not a fake "extracted 0 files"); partial matches warn and
  extract the hits.
- No full decompression needed: solid stream is expanded once, chunks fetched by offset.

### list

```
hcax list <in.hcax> [-l]
```

- Sorted by path by default (pack order is file→dir→link groups, which looks random).
- `-l` / `--long`: permissions + mtime + link targets (`->` symlink, `=>` hardlink).
- Summary line: counts and totals for files/dirs/links.

### info

```
hcax info <in.hcax>
```

Prints version, mode/backend, entry/chunk composition, and byte sizes of
header/metadata/data/raw/dictionary/tail sections. Useful for cross-checking FORMAT.md.

### verify

```
hcax verify <in.hcax>
```

Checks archive integrity. v6+: **stream-level hash** (solid stream + raw section).
Reading v5 and earlier falls back to per-chunk verification.

---

## 4. Benchmarks

> Ratio = compressed ÷ original × 100%, **lower is better**. Measured on this machine.

### 4.1 Mixed corpus (12.3 MB: text / JSON / Go source / binary / random / uncompressed BMP)

| Mode | Ratio | Note |
|---|---|---|
| **text** | **26.74%** | Best here (text-heavy corpus, CM wins) |
| **max** | **26.86%** | General best (0.1% behind text, much faster) |
| ultra | 26.86% | Whole-file solid |
| best | 32.58% | Fast tier |
| fast | 39.65% | Fastest tier |
| — | — | — |
| `xz -9e` | 28.00% | Reference |
| `7z -mx=9` | 28.39% | Reference |
| `zstd -19` | 28.70% | Reference (single stream, no dedup) |
| `zip -9` | 34.27% | Reference |

**`max` beats xz by 4.0% and 7z by 5.4%.** The two BMPs (125 KB total) contribute several
percentage points — uncompressed bitmaps are hcax's strength (§4.3); without them gaps shrink.

### 4.2 Text / code (CM strength)

| Sample | **text** | xz -9e | 7z-PPMd | 7z-LZMA2 |
|---|---|---|---|---|
| Novel 774KB | **24.97%** | 29.46% | 23.65% | 29.46% |
| Source 34KB | **30.33%** | 33.34% | 29.57% | 33.43% |
| Scrambled repeats 768KB | **25.64%** | 30.53% | 24.54% | 30.52% |
| Mixed text 842KB | **22.98%** | 27.10% | 21.83% | 27.10% |

**`text` beats xz by 13–16%**, approaching PPMd (3–5% behind). The "scrambled repeats" row matters:
with long-range redundancy destroyed, it still wins by 13.6% — this is prediction, not matching.

### 4.3 Uncompressed bitmaps (BMP / TGA / PNM, rewritten in v7)

**Supported**: BMP (BI_RGB 8/16/24/32-bit, incl. top-down), TGA (types 2/3, 8/24/32-bit), PNM
(P5/P6, maxval ≤ 255).

**Two-level reversible transforms** (pure arithmetic, byte-wise mod 256, invertible):

1. **RCT color decorrelation** — RGB channels are highly correlated (R≈G≈B); transform to
   `(G, R−G, B−G)`. The two difference channels are near-zero, eliminating a large chunk of
   redundancy — the largest single gain.
2. **MED median edge prediction** (LOCO-I / JPEG-LS predictor) — predicts from left/up/upper-left
   along the edge direction; doesn't cross edges, more stable than "left+up−upper-left" gradient,
   more accurate than Paeth/average.

> **Candidate-based transforms**: during packing, gating compares
> none / MED / RCT+MED and adopts only the smallest — never a regression.
> Transforms span chunk boundaries (prediction depends on whole-image geometry), so they are
> recorded **per-file** (`fe.xform`), not per-chunk.

**v12: row-wise processing, skipping BMP row padding.**
BMP rows pad to 4 bytes; with 24bpp and `width % 4 ∈ {1, 2}`, row stride is not a multiple of 3
(e.g. width 101 → 303 pixel bytes + 1 pad byte = 304 stride). The old flat implementation sliced
3-byte groups across the whole buffer, merging padding with the next row's head bytes into one
"pixel": channel phase drifted per row, MED's up/upper-left neighbors landed on wrong channels,
vertical prediction collapsed — worse than no decorrelation (measured 60% worse; gating fell back
to MED-only). The row-wise version operates only on `width × bpp` bytes per row; padding untouched.

**Measured (v11 → v12, 24bpp + 32bpp photo BMPs width 101, 126 KB total)**

| Mode | v11 | **v12** | Gain |
|---|---|---|---|
| `fast` | 29,605 B | **28,205 B** | **−4.7%** |
| `best` | 24,975 B | **23,360 B** | **−6.5%** |

Images without padding (width multiple of 4, or 32bpp) produce identical results — no regression risk.

**Measured (v6 → v7)**

| File | Original | v6 `max` | **v7 `max`** | **v7 `text`** | Gain |
|---|---|---|---|---|---|
| Photo color.tga | 1.19 MB | 79.44% | **61.38%** | **58.87%** | **+22.7%** |
| Photo color.ppm | 1.19 MB | 79.46% | **61.40%** | **58.87%** | **+22.7%** |
| Photo big.bmp | 1.19 MB | 67.37% | **61.38%** | **58.87%** | **+8.9%** |
| Photo pic.bmp | 1.82 MB | 23.58% | **20.75%** | **19.29%** | **+12.0%** |
| 32-bit alpha BMP | 1.62 MB | 48.51% | **46.32%** | 46.58% | +4.5% |
| Grayscale TGA / PGM | 405 KB | 72.80% | **71.56%** | **69.57%** | +1.7% |

Summary: `RCT` removes inter-channel redundancy, `MED` removes spatial redundancy, the residual
goes to the backend. `text` (CM backend) typically saves another 2–3 points on images.

### 4.4 Mixed corpus with compressed images (1.99 MB)

| Mode | Ratio |
|---|---|
| text | 65.95% |
| ultra | 67.48% |
| max | 67.56% |
| best | 69.45% |

> Compressed images (JPG/PNG) are near entropy limit; no lossless tool can shrink them. That's
> information-theoretic, not a bug.

### 4.4b Folder / directory tree (v6 focus)

Corpus: 880 files (600 similar JSON configs + 200 logs + 80 sources + subdirs), 748 KB total.

| Approach | Archive | Ratio |
|---|---|---|
| **hcax text** | 52,792 | **7.06%** |
| **hcax max** | 55,888 | **7.47%** |
| `7z -mx=9` | 58,134 | 7.77% |
| `tar.xz` | 78,948 | 10.55% |
| hcax max (pre-v6) | 117,976 | 15.77% |

What changed for folders:

1. **Metadata compressed too**: pre-v6, chunk table + file table were plaintext — up to 54.7% of
   archives with many small files. Now compressed with the same backend.
2. **Compact chunk table**: derivable chunk offsets removed (sequential accumulation); per-chunk
   hashes replaced by stream-level checksums (8B random hash per chunk was incompressible —
   880 chunks = 7 KB wasted). Table went from 31 B/entry to **5 B/entry**.
3. **Full directory structure**: empty dirs recorded and rebuilt (lost pre-v6).
4. **Path fix**: a long-standing bug where the common root of absolute paths lost its leading `/`,
   breaking `filepath.Rel` and flattening all nested paths to basenames. `config/a.json` now
   unpacks to `config/a.json`.

### 4.5 Parallel solid groups (large corpora)

142.5 MB corpus (74.8 MB compressible):

| Config | Time | Ratio |
|---|---|---|
| Parallel (auto ≥64MiB) | **8.52s** | 32.81% |
| Forced single-thread | 36.75s | 32.41% |

**4.31× faster**, ratio cost only +0.40pp.

### 4.6 Pack memory fix for large files (v8)

v7 packing a 350MB+ highly-repetitive corpus peaked at **~1.58 GB RSS** in `max` mode (OOM risk).

**Root cause**: the gating zstd encoder (estimates per-chunk compressibility) was created as a
streaming encoder with `1<<27` = **128 MiB** window; its history buffer grew to **~1.79 GB**
(98.6% of live heap). Gating only needs a ratio estimate — no large window required.

**Fix** (ratio and losslessness unchanged):
1. Gating window 128 MiB → **1 MiB**;
2. Chunk buffers pooled (`sync.Pool`), removing per-chunk `make(64K)` GC spikes;
3. Dedup first (skip expensive preprocessing for duplicates) + release `cm.data` promptly.

**Measured (367061536B repetitive corpus, GOMAXPROCS=8)**

| Metric | v7 | v8 | Change |
|---|---|---|---|
| `max` peak RSS | ~1579 MB | **77 MB** | ↓ ~20× |
| `max` ratio / lossless | 0.08% / OK | 0.08% / OK | unchanged |
| `ultra` peak RSS | ~1750 MB | **88 MB** | ↓ ~20× |
| `ultra` ratio / lossless | 0.08% / OK | 0.08% / OK | unchanged |

> `best` (~264MB) peak is zstd-19 dictionary training resident; unchanged.
> `ultra` pre-v8b peaked ~1750MB from "whole-file solid **without dedup**" — all raw chunk data
> resident twice. v8b enabled dedup for `ultra` (same as `max`), keeping only its
> single-stream full-data parameter selection; RSS dropped to **88 MB**, ratio unchanged.
>
> **Others are as bad or worse.** `ultra` uses the same lzma2 big-dict engine as 7z: `xz -9e
> dict=256MiB` on the same file measures **2974 MB** RSS (609 MB decompressing); 7z's backend
> actually uses ~3GB (`/usr/bin/time -l` only sees the 7z frontend's 3MB — the actual work happens in
> a child process). hcax's pre-fix 1750MB was self-inflicted (no dedup → double resident data),
> not inherent to lzma2. Reproduce with `./bbench.sh <bigfile>`.

### 4.7 Further memory & robustness work (v8c)

**A. Fixed `max` unpack out-of-bounds panic on large corpora (≥64MiB compressible) — data loss**
- Cause: after v8's `cm.data = nil` (prompt release), `max`'s parallel `compressMT` still read
  `c.data` to assemble groups → empty group → **empty solid frame** (`compLen=0`) → unpack
  `readChunk` panicked on the empty slice.
- This path only triggers at ≥64MiB compressible (parallel groups), so repetitive corpora (dedup
  <64MiB) never caught it in v8/v8b.
- Fix: `compressMT` slices the retained `compSolid` buffer by `c.offset` (zero-copy), never
  touching released `cm.data`.
- Verified: 331MB non-repetitive corpus (`max`) pack→unpack→`cmp` byte-identical (pre-fix: panic).

**B. Parallel groups zero-copy + per-group dictionaries (lower `max` memory)**
- Zero-copy: same-group chunks are contiguous in `compSolid`; slice directly as group input,
  eliminating double residence of full stream + group copies (Go RSS **2782 MB → 1900 MB** on
  331MB corpus).
- Per-group lzma2 dict: `dictFor(group size)` instead of whole-stream dict — each `xz` child
  process memory scales with group size (~4× drop: 256MiB dict ≈3GB/process → 64MiB ≈700MB).
  xz frames are self-describing; format unchanged.

**C. zstd tier (best/fast) memory tightening**
- Encoder window capped at **1 MiB** (zstd encodes per 256KiB chunk; bigger window is pure waste).
- `best`'s **lazy non-dict encoder**: `best` always trains a dict and uses `zEncDict`; the
  dictionary-less `zEnc` (level-19, ~30MB) was never used yet always resident. Now created on
  first use → `best` peak **264 MB → 233 MB**.
- Dict training samples capped at **64 MiB** (previously `2000×128KiB=256MiB`, double-resident
  with `cm.data` on non-repetitive multi-file inputs; no ratio loss from the cap).

**Measured (367061536B repetitive corpus, GOMAXPROCS=8, byte-identical `cmp`)**

| Mode | Peak RSS | Ratio | Lossless |
|---|---|---|---|
| `fast` | 42 MB | 0.12% | OK |
| `best` | 233 MB | 0.13% | OK |
| `max` | 89 MB | 0.08% | OK |
| `ultra` | 93 MB | 0.08% | OK |

> `best`'s 233MB = Go heap arena scaled by GOMAXPROCS + in-process zstd-19 encoder (~34MB, level-
> dependent, window-independent). `max`/`ultra` push lzma2 to **child-process xz**, ~90MB in Go —
> the inherent tradeoff of in-process high-ratio encoding vs low-memory subprocess encoding.

### 4.8 Solid stream to temp files (v8e) — bounded pack memory

v8c kept the whole solid stream / raw section in **memory buffers** (`bytes.Buffer`): one solid
stream = full input resident. On non-repetitive large files (dedup useless), 331MB corpus hit
~1.8 GB peak RSS (~5× input) — every input byte existed as chunk slice + solid buffer + raw
buffer copies, amplified 2–3× by `bytes.Buffer` doubling growth and lazy GC.

**Root cause** (data-driven, via `HCAX_T` instrumentation + heap profiling):
- `fast` (pure zstd, no xz) also peaked at 1.8 GB → bottleneck is **not the backend**, it's the
  Go-side whole-solid-stream residency.
- `GOGC=20` dropped `fast` from 1641MB to 1134MB → ~500MB was lazy GC garbage, but ~1× input
  live data (solid + raw buffers) remained.

**Fix**: pack phase writes solid stream / raw section to **temp files** (`os.CreateTemp`),
discarding buffers after chunk transform; compression reads **streaming from file**:
- zstd tiers (`fast`/`best`): `compressZstdStream` feeds the encoder per segment — memory is
  "one chunk + encoder state";
- lzma2 parallel groups (`max`): `compressMT` `ReadAt`s each group's slice from the temp file
  (one copy, only group size resident);
- lzma2 single stream (`ultra`/small): solid stream read back to memory (only <64MiB, safe);
- stream hash `hashReader` computes from file — no full-data load.
- **Unpack path unchanged** (still decompresses solid stream to memory once); this optimization
  targets pack peak memory only.

**v8e measured (331MB non-repetitive diverse corpus, GOMAXPROCS=8, byte-identical `cmp`)**

| Mode | v8c peak RSS | v8e peak RSS | Drop | Ratio | Lossless |
|---|---|---|---|---|---|
| `fast` | ~1866 MB | **380 MB** | ↓ 4.9× | 25.90% | OK |
| `best` | ~2100 MB | **515 MB** | ↓ 4.1× | 25.16% | OK |
| `max` | ~1900 MB | **694 MB** | ↓ 2.7× | 20.73% | OK |
| `ultra` | ~1848 MB | **1096 MB** | ↓ 1.7× | 20.62% | OK |

> `ultra` stays high (~1.1GB): whole-file solid, no dedup, single-stream lzma2 requires the full
> stream in memory — inherent to 7-zip-style single-stream extreme ratio, same tradeoff as v8b.
> `max`'s 694MB is mostly 8 parallel `xz` children (~64–80MB each). Repetitive corpora (dedup
> effective) memory is unchanged (§4.7 table): `fast` 39MB / `best` 235MB / `max` 80MB / `ultra` 82MB.

### 4.9 Bounded unpack memory + correctness/robustness fixes (v9)

v8e fixed only the **pack** side. Unpack still decompressed the whole solid stream + raw section
up front, and `openArchive` decompressed **unconditionally** — so `list` cost full-data memory
and `extract` of one 10-byte file decompressed the entire stream. v9 makes unpack symmetric.

**Changes**

1. **Lazy solid-stream decompression to temp file**: decompress only the chunk actually needed;
   `readChunk` uses `ReadAt` — memory drops from "full input" to "one chunk + encoder state".
2. **Raw section zero-memory**: no decompression; `ReadAt` by offset straight from the archive.
3. **Extract early termination**: `extract` computes the target file's max chunk offset and stops
   decompressing there (front-of-archive files don't pay for the whole stream).
4. **No temp-file leaks**: `fatal()` calls `os.Exit`, so Go `defer` never runs — since v8e, a
   failed pack left two temp files (up to hundreds of MB) in TMPDIR forever. Now an explicitly
   registered cleanup hook runs on both success and failure paths.
5. **Directory structure no longer flattened** (data-corruption-level bug): archive root used to
   be the **files'** common parent; when all files sat in subdirs (e.g. `src/main/go/*.go`), root
   collapsed to `src/main/go`, flattening everything above — `src/main/go/main.go` stored as
   `main.go`, and the `go/` level vanished entirely. Now uses the **inputs'** common parent, same
   as tar/zip.
6. **Symlinks**: old code read links as regular files — links to directories failed with EISDIR;
   links to files copied the target's content into the archive. Now stored as links (v9 format).
7. **Legal filenames no longer rejected**: path check used `strings.Contains(name, "..")`,
   rejecting legal names like `a..b.txt` / `..weird.txt` as traversal attacks ("packs fine, won't
   unpack"). Now checks per segment, rejecting only real escapes.
8. **Prevent "create link, then write through"**: before writing files, drop existing same-name
   symlinks (an archive that creates `x -> /etc` then writes `x/passwd` would otherwise write
   outside the unpack dir).

**Measured (150 MB non-repetitive mixed corpus, `fast`, byte-identical `cmp`)**

| Op | v8e | **v9** | Change |
|---|---|---|---|
| `list` (metadata only) | 582 MB | **6 MB** | ↓ 97× |
| `extract` single file | 670 MB | **35 MB** | ↓ 19× |
| `unpack` full | 582 MB | **35 MB** | ↓ 17× |
| `pack` | 366 MB | 354 MB | same (pack path untouched this round) |
| Decompressed bytes for front-of-archive extract | whole stream | stops at target chunk | 2.4× faster |

No ratio regression (12 MB mixed): fast 41.32% / best 33.91% / max 27.47% / ultra 27.47% /
text 27.16%, bit-identical to v8e.

### 4.10 Real executables: lzma2 dictionary tier fix (v12)

All corpora were synthetic — a real 20MB Mach-O was never tested. `max` produced 6,672,621 B,
**4% worse than bare `xz -9e`** (6,401,480 B), despite BCJ, dedup and preprocessing.

Not preprocessing — the dictionary. 20.5 MB fell in the "<64MiB → 8MiB" tier, while `xz -9e`
defaults to 64MiB. Isolating xz on the same data:

| dict | Size | Conclusion |
|---|---|---|
| 8 MiB | 6,659,708 B | old tier |
| **16 MiB** | **6,395,092 B** | **another 4.0% saved** |
| 32 MiB | 6,395,292 B | no gain |

Sweet spot is 16MiB. But on the 12.6MB mixed corpus, 4/8/16/32MiB all gave identical results
(3,526,360 B) — "bigger dict doesn't help" only holds for diverse corpora: executable repeats are
**long-range** (function bodies, string tables far apart); only a large-enough dict connects them.

**Change**: drop the 8MiB tier; ≥16MiB compressible data always uses 16MiB.

| Corpus | Before | After | Change |
|---|---|---|---|
| 20.5 MB executable (2 Mach-Os) | 6,672,621 B | **6,408,973 B** | **−3.95%** |
| 12.6 MB mixed corpus | 3,383,567 B | 3,383,567 B | zero |

Time unchanged (3.30s → 3.36s). Cost: xz child peak 121MB → 208MB, and only for ≥16MiB inputs;
smaller ones still use 4MiB / 2MiB (~72MB peak).

---

## 5. How It Works

In data-flow order:

1. **Content-defined chunking (CDC / FastCDC gear hash)**
   Chunks split by content boundary (avg 64KB); identical content yields identical chunks →
   **cross-file dedup**.
2. **Dedup**
   Identical chunks stored once; large gains on multi-version / multi-copy data.
3. **Content-adaptive preprocessing** (per chunk, picks the smaller result)
   - `DELTA`: 1-D differencing (8/16/24/32-bit)
   - `BCJ-x86`: x86 executable jump-address transform
   - `RCT + MED`: color decorrelation + median edge prediction for uncompressed bitmaps
     (BMP/TGA/PNM; file-level, candidate-based)
   - All **pure Go, reversible, byte-identical**
4. **Incompressibility detection + raw storage**
   Uncompressible chunks (images, random data) stored as-is (saves time). Dual condition:
   "cheap encoder + byte entropy", avoiding misclassifying "locally incompressible but globally
   compressible" image residuals.
5. **Solid stream + lzma2 extreme parameters**
   Compressible chunks concatenated into one solid stream, compressed once (exploits cross-chunk
   redundancy). lzma2 parameters tuned and adapted to data size: `dict` converges by volume, `pb`
   chosen between 2/4 by measurement.
6. **Parallel solid groups** (`max`, compressible ≥64MiB)
   G groups compressed in parallel — ~4× faster, slight ratio cost.
7. **Pack-order clustering**: the solid stream concatenates all files, so "which chunks are
   adjacent" directly affects ratio. Sort by extension → size tier → path to group similar files
   (12MB mixed corpus: fast −0.21% / best −0.15% / max·ultra −0.10%; text-random interleaved
   corpus fast −0.9%). `text` (CM) skips this — reordering made it 7.2% worse; its model adapts
   along the stream.
8. **Context mixing (CM) backend** (`text`)
   No matching — **bit-by-bit prediction**: order-0~6 context models + match model + adaptive
   mixer + two-level SSE, fed to a binary arithmetic coder. Better than LZ-family tools on
   text/code.

**Container**: `HCAX` magic + header + data section (solid frames + raw section) + chunk table +
file table + tail checksum.

---

## 6. Archive Format

- Current version **v12**, reads **v2 ~ v12** (backward compatible)
- **Version upgraded on demand** (as rarely as possible; every bump makes old binaries unable to
  read): plain archives write v8; symlinks → v9; setuid/setgid/sticky or mtime beyond uint32
  seconds → v10; `-T/--precise-times` also → v10 (nanosecond timestamps); hardlinks → v11;
  row-wise raster transforms (into uncompressed bitmaps) → v12. Reading a newer archive with an
  old binary reports "version incompatible" — **explicit failure, never wrong data**.
- Layout: header(48B) → **compressed metadata frame** → solid data frames → raw section → dict
  training area → tail
- **Metadata (chunk table + file table) is compressed too** (plaintext pre-v5; could exceed half
  the archive with many small files)
- Chunk table **5 B/entry**: orig length(4B) + flags(1B: raw / transform type / dict). **No
  offsets stored** — chunks in solid stream and raw section are sequentially packed; unpack
  accumulates.
- **Stream-level checksums**: 8B hash for solid stream and raw section each; `verify` checks the
  whole stream (reading v5 and earlier falls back to per-chunk 16B hashes).
- Compact file table (mtime 4B / mode 2B; 8B nanosecond + 4B full permission bits from v10), and
  **directory entries recorded separately — empty dirs preserved**.
- **Special permission bits preserved**: setuid/setgid/sticky saved since v10 (v9 and earlier
  used `FileMode.Perm()`, only 0777).
- **v7 new**: file-table flag bits1–4 = `xform` — per-file raster transform
  (none / MED / RCT+MED / row-wise MED / row-wise RCT+MED from v12). When reading v6 and earlier
  (field absent), falls back to the old heuristic "BMP → gradient inverse transform";
  **heuristic disabled since v7** (otherwise intentionally-untransformed files get wrongly
  inverse-transformed).
- **v8 fix (large-file memory blowup)**: gated zstd window 128MiB → 1MiB + chunk buffers via
  `sync.Pool` + dedup-first + prompt release of `cm.data`. `max` packing 350MB+ repetitive corpus
  peak RSS dropped **~1.6GB → ~80MB**, ratio and losslessness unchanged (see §4.6).

---

## 7. Code Structure

```
main.go        CLI entry / usage / flag parsing
format.go      container format: entry types + metadata serialize/parse + version decisions
               (byte-level layout in FORMAT.md)
pack.go        pack side: input collection / --exclude / chunking dedup / compression / write
unpack.go      unpack side: open archive / lazy solid-stream expansion / restore files, dirs,
               symlinks, hardlinks + path safety (joinOut / mkdirUnderOut: blocks escapes and
               symlink write-through)
cmds.go        read-only commands: list / info / verify (no data expansion needed)
util.go        global utilities: cleanup hooks / fatal exit / in-archive name validity (safeName)
backend.go     compression backends: zstd / lzma2 / CM dispatch, gating, adaptive params,
               parallel groups
chunker.go     content-defined chunker (FastCDC gear hash)
preprocess.go  preprocessing transforms: DELTA / BCJ-x86 / detection
raster.go      raster images: BMP/TGA/PNM detection + RCT color decorrelation + MED 2D
               prediction + transform selection + byte entropy
cm.go          context-mixing (CM) compressor: arithmetic coding + multi-model + mixer + SSE
fuzz_test.go   randomized tests: random dir trees (random perms/times) round-trip + partial
               extract + --exclude + random corruption must fail cleanly (no panic)
security_test.go malicious archives: path escape / symlink write-through (confirm the attack
               first, then fix)
legacy_test.go hand-built v3/v5 archives per FORMAT.md, covering old-version read paths
FORMAT.md      container format spec (byte-level layout + version history + compat matrix)
test/          regression: mkcorpus.py (corpus gen) + regress.sh (one-shot verify)
               + bench.sh (recompute §8 speed/memory tables, min of N runs)
               + verify3.sh (three-pass: functional regression / big-corpus round-trip /
               compat matrix)
_legacy_prototype/ early v1 prototype and intermediates (kept for reference, deletable)
```

**Regression tests** (run after every change):

```bash
bash test/regress.sh ./hcax /tmp/hcax-regress   # arg 1 = the **binary**
bash test/bench.sh   /tmp/hcaxbench/corpus      # arg 1 = the **corpus dir**
bash test/verify3.sh /tmp/hcax-verify3          # arg 1 = the **work dir**
```

> ⚠️ Each script's first argument means something **different** — passing the wrong one is painful
> to debug: `verify3.sh` receives a work dir it will `rm -rf`; passing `./hcax` would delete the
> freshly built binary (it refuses, guarded).

Coverage: 5 modes × 2 corpus types lossless round-trip, edge cases (empty file / 1 byte /
double-dot filenames / nested empty dirs / symlinks), CLI behavior (list/verify/extract/
--verify/overwrite counts), bitmaps (BMP with row padding) end-to-end, dir perms & mtime restore,
corruption detection (raw section / metadata / magic / version byte each tampered), fallback
warning when system `xz` missing (reported once), external `xz` stderr never mixed into the
compressed stream, atomic archive write (reuse target perms, no half-written temp file left),
truncated/tampered archives must fail cleanly (no panic), resource hygiene (no temp-file leaks).
Currently **104/104 pass**.

Bad-input fuzz coverage (`hcax_test.go`, previously zero): for each of 5 modes, **exhaustively
every truncation prefix** (~49k, of which ~3k also run a full unpack), plus 200 random
bit-flipped archives per mode. Requirement: no panic. Truncation only shrinks data; tampering can
grow length fields — both must be exhaustively covered. Each fuzz case first asserts "the intact
archive lists fine", otherwise tens of thousands of runs are no-ops.

To confirm tests actually catch regressions, run an old binary:

| Binary fed | Result | Note |
|---|---|---|
| Current | 104 pass / 0 fail | — |
| Previous (R30) | 91 pass / **2 fail** | exactly the two new things since: dir mtime, row-wise raster v12 |
| v8 reference | 32 pass / **61 fail** | two years of behavior drift, large red swath |

(`bash test/verify3.sh` adds: big-corpus byte round-trip + real executables + 70MB parallel
groups, plus a compat matrix against old binaries.)

Dependencies: `github.com/klauspost/compress/zstd`, `github.com/ulikunitz/xz` (pure Go).
`max`/`ultra` **compression** calls the system `xz`; if missing, falls back to pure Go with a
**printed warning** (ratio 24% worse — see §2/§8). **Decompression is always pure Go, zero
external deps.**

---

## 8. Performance & Limitations

### Speed (12.00 MiB standard corpus, reproducible via `bash test/bench.sh`)

Corpus: `python3 test/mkcorpus.py /tmp/hcaxbench/corpus` — standard mixed corpus,
**12,582,539 bytes (12.00 MiB)**, 7 files (English text / Go source / JSON / structured binary /
random data / 24bpp+32bpp uncompressed BMP). Fixed seed; byte-reproducible.

| Mode | Pack (wall / CPU) | Unpack (wall / CPU) | Peak RSS | Archive size |
|---|---|---|---|---|
| fast | 0.18s / 0.21s | 0.02s / 0.03s | 94 MB | 4,994,083 B |
| best | 0.95s / 0.98s | 0.02s / 0.03s | 304 MB | 4,103,027 B |
| max | 4.94s / 9.32s | 1.34s / 1.34s | 96 MB | 3,383,567 B |
| ultra | 5.40s / 10.18s | 1.40s / 1.39s | 92 MB | 3,383,567 B |
| text | 9.32s / 9.25s | 10.05s / 9.98s | 326 MB | 3,368,111 B |

**How to read this table:**

- **wall** = time you actually wait; **CPU** = total CPU across process + children.
- `max`/`ultra` CPU ≈ 2× wall — they run **two concurrent `xz` child processes** choosing
  `pb=2/4`. Not slower; using two cores.
- Each row is the **minimum of 5 runs**: other machine load only makes results worse, never
  better, so the min is closest to undisturbed. (This machine's load average is 5~7; single
  samples of `max` range anywhere from 2.9s to 5.9s — the old README's `ultra 3.84s` came from
  that and was not trustworthy.)
- `text` is bit-by-bit context modeling; slowness is inherent (~**1.3 MB/s** on this corpus). It
  Its advantage is ratio, not speed.
- Absolute values change across machines/corpora; **magnitudes matter more than absolutes**.

### Known limitations

1. **Already-compressed data won't shrink**: JPEG/PNG/MP4/random data are near entropy limit;
   lossless cannot compress further.
2. **`text` mode is slow**: bit-by-bit modeling, ~**1.3 MB/s** measured ("0.5 MB/s" in source
   comments was an early estimate, corrected). For text-type data, not big binaries.
3. **CM useless on high-entropy data**: use `text` only for text/code/logs; `max` for binaries.
4. **Memory** (peak RSS, table above): `text` ~**326MB** — model tables alone ~100MB
   (five order-2..6 counter tables: 16MB + 4MB + 4MB + 16MB match table), rest is solid stream +
   Go heap; `best` ~**304MB** (zstd-19 dict training resident); `max`/`ultra` ~**92~96MB** — but
   with ≥16MiB compressible data the lzma2 dict rises to 16MiB and xz children peak ~**210MB**
   (the cost of saving 4% on executables, §4.10); pre-v8 was ~1.6GB/~1.8GB. Decompression is pure
   Go, needs lzma2 dict working memory (~dict size).
5. **`max`/`ultra` depend on system `xz`**: missing → pure-Go fallback, **26.89% → 33.40%
   (archive 24% larger)**, with a printed warning. See §2 and FAQ Q3.
6. **Uncompressed bitmap limits**: BMP only BI_RGB uncompressed 8/16/24/32-bit; TGA only types
   2/3 8/24/32-bit palette-less; PNM only P5/P6 with maxval ≤ 255. Palette, RLE-compressed, or
   >16-bit-per-channel images take the generic path (no raster transforms).
7. **Needs temp disk space**: the price of bounded memory — pack writes solid stream / raw
   section to `TMPDIR` temp files; unpack's lazily-decompressed solid stream likewise. Both peak
   at about **the data size itself** (pack ≈ input size, unpack ≈ decompressed size). Before
   backing up 100 GB, confirm `TMPDIR` has 100 GB; set the `TMPDIR` env var to relocate.

### v12 known gaps

1. **Symlink mtimes can't be restored**: Go stdlib has no `lutimes`; only regular files/dirs.
2. **No owner / ACL / xattr preservation**: only permission bits + mtime (setuid/setgid/sticky
   from v10).
3. **No content re-check on dedup**: chunk dedup relies on 128-bit hashes, collision ~2⁻⁶⁴ —
   theoretical, negligible in practice.
4. **No encryption, no volumes, no incremental updates**.
5. **`text` mode still slow** (~1.3 MB/s, one to two orders slower than LZ): inherent to bit-by-
   bit context modeling. Tried int64→int32 counter updates for speed; on arm64 it was **3~5%
   slower** (32-bit ops need extra sign extension). Reverted; don't try that path again.
6. **Sub-second timestamps dropped by default**: stored in seconds (like tar) for compatibility;
   use `-T` for nanoseconds.

(Already closed since v9~v12, no longer "not done": symlinks, hardlinks, setuid/setgid/sticky,
pack-side `--exclude`, bounded unpack memory, no silent truncation on write failure, row-wise
raster transforms (BMP row padding), atomic archive writes, `verify` no longer leaks temp files.)

---

## 9. FAQ

**Q: Why can't images be compressed?**
A: JPG/PNG are already entropy-coded, near the information-theoretic limit. No further lossless
compression is possible while staying byte-identical (short of reimplementing external codecs —
costly and unreliable).

**Q: Which is smaller, `text` or `max`?**
A: Depends on data. Text/code → `text` (8~16% smaller); binaries/compressed data → `max`.

**Q: Why is my `max` ratio far worse than the README (e.g. 33% not 27%)?**
A: **Almost certainly no `xz` on the system.** `max`/`ultra` lzma2 goes through the system `xz`;
if missing it falls back to the built-in pure-Go implementation, 24% worse ratio. Run `which xz`;
if absent, `brew install xz` (macOS) or `apt-get install xz-utils` (Debian/Ubuntu). hcax prints a
warning to stderr when falling back.

**Q: Do I need xz to unpack?**
A: No. Decompression is pure Go throughout. Only `max`/`ultra` **compression** uses system `xz`.

**Q: Are archives cross-platform?**
A: Yes. The format and byte order are explicit; `hcax` is a static binary. Note path separators
are recorded as packed.

**Q: How do I confirm an archive isn't corrupted?**
A: `hcax verify file.hcax`, or `--verify` during unpack.

**Q: What is `_legacy_prototype/`?**
A: Backup of early artifacts (Python prototype stage). Safe to delete as a whole; doesn't affect
`hcax`.

---

## 10. License

[MIT](LICENSE) — Copyright (c) 2026 Fleeting-Starfall
