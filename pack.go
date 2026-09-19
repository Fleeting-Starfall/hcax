package main

// Packing side: collect inputs, exclusion rules, chunk dedup, compression, container write.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Exclusion rules (glob): matching entries are left out of the archive; matching
// directories are skipped entirely (no traversal — backing up with .git / node_modules
// excluded saves a lot of stat and read I/O). Any of the three matches excludes:
//
//	"*.tmp"       — file with that name at any depth (matched by base name)
//	".git"        — directory named .git at any depth (matched per path segment)
//	"build/*"     — entries under build/ relative to the pack root (matched by rel path)
//
// full = real path on disk, rel = path relative to this pack root. Both are compared:
// users write patterns thinking of the in-archive path (rel), while ".git" should
// match at any depth (per segment).
func excluded(full, rel string, excl []string) bool {
	if len(excl) == 0 {
		return false
	}
	norm := filepath.ToSlash(full)
	cands := []string{norm}
	if rel != "" && rel != "." {
		cands = append(cands, filepath.ToSlash(rel))
	}
	for _, pat := range excl {
		if pat == "" {
			continue
		}
		for _, c := range cands {
			if ok, _ := filepath.Match(pat, c); ok {
				return true
			}
		}
		for _, seg := range strings.Split(norm, "/") {
			if ok, _ := filepath.Match(pat, seg); ok {
				return true
			}
		}
	}
	return false
}

// The same input given twice (pack out.hcax dup dup): the old implementation stored
// every file under dup twice, leaving duplicate entries in the archive; on unpack the
// later one silently overwrote the earlier — wasteful and misleading. Dedup by cleaned
// path (./dup, dup and dup/ all count as the same).
func dedupPaths(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		k := filepath.Clean(p)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// A solid stream concatenates all files before compressing, so the neighbors of a
// chunk directly affect the ratio: a text run followed by random binary resets the
// compressor's context; grouping similar files lets lzma2/zstd save bits.
// Sort key: extension -> size bucket -> original path. The last term keeps output
// stable for identical input (reproducible). Directories/links are not involved
// (no data chunks; order only affects metadata, traversal order stays intuitive).
func orderForCompression(files []string, spec modeSpec) []string {
	if os.Getenv("HCAX_NO_ORDER") != "" { // A/B comparison hook
		return files
	}
	// CM(text) skips this: its model adapts along the stream, reordering measured
	// 7% worse (12MB mixed corpus 3.39MB -> 3.64MB); the "similar adjacent" intuition
	// does not hold for it.
	if spec.backend == "cm" {
		return files
	}
	type ent struct {
		p    string
		ext  string
		size int64
	}
	es := make([]ent, 0, len(files))
	for _, p := range files {
		var sz int64
		if fi, e := os.Lstat(p); e == nil {
			sz = fi.Size()
		}
		es = append(es, ent{p: p, ext: strings.ToLower(filepath.Ext(p)), size: sz})
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].ext != es[j].ext {
			return es[i].ext < es[j].ext
		}
		bi, bj := sizeBucket(es[i].size), sizeBucket(es[j].size)
		if bi != bj {
			return bi < bj
		}
		return es[i].p < es[j].p
	})
	out := make([]string, len(es))
	for i := range es {
		out[i] = es[i].p
	}
	return out
}

// Bucket sizes by powers of two (keep files of the same magnitude together) to
// avoid 1KB / 100MB alternation.
func sizeBucket(n int64) int {
	b := 0
	for n > 1<<20 && b < 40 {
		n >>= 1
		b++
	}
	return b
}

// Names can still collide after input dedup: pack out.hcax dup dup/f.txt — dup/f.txt
// is collected both by walking dup and by explicit mention. Storing both makes the
// later one silently overwrite the earlier on unpack while the user thinks both are
// there. The same in-archive name can only come from the same data, so keeping one
// copy (with a notice) is enough; no error needed.
func dropDupNames(files, dirs, links []string, root string) ([]string, []string, []string) {
	seen := make(map[string]bool, len(files)+len(dirs)+len(links))
	dropped := 0
	keep := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			k := storedName(p, root)
			if seen[k] {
				dropped++
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
		return out
	}
	files, dirs, links = keep(files), keep(dirs), keep(links)
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "warning: skipped %d duplicate entries (overlapping inputs mapped the same in-archive name to multiple sources)\n", dropped)
	}
	return files, dirs, links
}

// The archive itself must not enter the archive: `hcax pack arch.hcax .` would fold
// the previous arch.hcax in, growing it on every pack (154B -> 278B -> 389B...) and
// storing the previous version as new data. tar skips this case with a notice; do
// the same here.
func excludeSelf(files, dirs, links []string, outPath string) ([]string, []string, []string) {
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return files, dirs, links
	}
	n := 0
	drop := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			if abs, e := filepath.Abs(p); e == nil && abs == outAbs {
				n++
				continue
			}
			out = append(out, p)
		}
		return out
	}
	files, dirs, links = drop(files), drop(dirs), drop(links)
	if n > 0 {
		fmt.Fprintf(os.Stderr, "warning: skipped the archive itself %s (an archive cannot contain itself)\n", outPath)
	}
	return files, dirs, links
}

var nExcluded int // entries excluded via --exclude (reported at the end of pack)

func collectPaths(inputs []string, excl []string) (files []string, dirs []string, links []string) {
	for _, p := range inputs {
		fi, err := os.Lstat(p)
		if err != nil {
			fatal("stat %s: %v", p, err)
		}
		if excluded(p, "", excl) {
			nExcluded++
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, p)
			continue
		}
		if fi.IsDir() {
			filepath.Walk(p, func(q string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				rel := ""
				if r, e := filepath.Rel(p, q); e == nil {
					rel = r
				}
				if q != p && excluded(q, rel, excl) {
					nExcluded++
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if info.Mode()&os.ModeSymlink != 0 {
					links = append(links, q)
				} else if info.IsDir() {
					dirs = append(dirs, q)
				} else if !isRegular(info) {
					// socket / fifo / device node: reading blocks or fails outright;
					// one such entry must not sink a whole directory backup
					skipped = append(skipped, q)
				} else {
					files = append(files, q)
				}
				return nil
			})
		} else if !isRegular(fi) {
			skipped = append(skipped, p)
		} else {
			files = append(files, p)
		}
	}
	return
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Regular files only: other types (socket/fifo/device/pipe) cannot be archived
// meaningfully — reading blocks (fifo) or fails (device) and must not sink the pack.
func isRegular(fi os.FileInfo) bool {
	return fi.Mode()&os.ModeType == 0
}

var skipped []string // skipped non-regular files (reported together at the end of pack)

func commonRoot(paths []string) string {
	if len(paths) == 0 {
		return "."
	}
	sep := string(os.PathSeparator)
	rc := strings.Split(filepath.Clean(paths[0]), sep)
	if len(rc) > 1 {
		rc = rc[:len(rc)-1]
	} else {
		rc = []string{"."}
	}
	for _, p := range paths[1:] {
		pc := strings.Split(filepath.Clean(p), sep)
		if len(pc) > 1 {
			pc = pc[:len(pc)-1]
		} else {
			pc = []string{"."}
		}
		i := 0
		for i < len(rc) && i < len(pc) && rc[i] == pc[i] {
			i++
		}
		rc = rc[:i]
	}
	if len(rc) == 0 {
		return "."
	}
	// The first component of an absolute path is empty; filepath.Join would drop it,
	// so the root separator must be restored, otherwise filepath.Rel(root, absFile)
	// fails and falls back to basename (flattening paths).
	abs := rc[0] == ""
	out := filepath.Join(rc...)
	if abs {
		out = sep + out
	}
	return out
}

func storedName(fp, root string) string {
	rel, err := filepath.Rel(root, fp)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return filepath.Base(fp)
	}
	return rel
}

// Take mode bits: permission + setuid/setgid/sticky only.
// The old implementation used os.FileMode.Perm(), which keeps only 0777 — so the
// setuid/setgid/sticky bits were silently dropped in archives (programs lost their
// privilege bits after backing up e.g. /usr/bin). Type bits (dir/link/device) are
// not stored: isDir/isLink flags express them, saving space.
func entryMode(fi os.FileInfo) uint32 {
	const keep = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return uint32(fi.Mode() & keep)
}

// Nanosecond timestamp for writing: some entries (e.g. unit-test constructed) have
// whole-second mtimes only; pad to nanoseconds so v10 archives do not store 0 time.
func entryNano(fe fileEntry) int64 {
	if fe.nano != 0 {
		return fe.nano
	}
	if fe.mtime != 0 {
		return int64(fe.mtime) * 1e9
	}
	return 0
}

// Restore timestamp: v10 uses nanosecond precision, old archives have seconds only.
func entryTime(fe fileEntry) time.Time {
	if fe.nano != 0 {
		return time.Unix(0, fe.nano)
	}
	return time.Unix(int64(fe.mtime), 0)
}

// Streaming 16-byte checksum (first 16B of sha256) over the whole data: after the
// solid/raw regions land in temp files, stream-level verification no longer needs
// the full data in memory (replaces the old hash16 over a full slice).
func hashReader(r io.Reader) [16]byte {
	h := sha256.New()
	io.Copy(h, r)
	var out [16]byte
	copy(out[:], h.Sum(nil)[:16])
	return out
}

// Account by bytes actually read, not the size stat saw at open.
//
// A pack can run for minutes, during which a source file may be rewritten or
// truncated. The old code always recorded st.Size(): a shrunk file produced an
// archive declaring N bytes while holding M — the pack still reported success, and
// the mismatch surfaced only at restore time ("size mismatch"). That is the worst
// kind of failure: it lies during the backup.
//
// tar calls this "file changed as we read it". Here we account by actual read
// (archive stays internally consistent and unpackable) plus a warning so the user
// knows this archive is only a snapshot of some instant.
func sizedRead(fp string, statSize int64, nread uint64) uint64 {
	if uint64(statSize) != nread {
		fmt.Fprintf(os.Stderr,
			"warning: %s changed while being read (stat %d B, read %d B); accounted by actual bytes read\n",
			fp, statSize, nread)
	}
	return nread
}

func pack(inputs []string, outPath, mode string, excl []string, precise bool) {
	spec, ok := modes[mode]
	if !ok {
		fatal("unknown mode %s", mode)
	}
	inputs = dedupPaths(inputs)

	files, dirs, links := collectPaths(inputs, excl)
	files = orderForCompression(files, spec)
	files, dirs, links = excludeSelf(files, dirs, links, outPath)
	files, dirs, links = dropDupNames(files, dirs, links, commonRoot(inputs))
	if len(files) == 0 && len(dirs) == 0 && len(links) == 0 {
		fatal("no files to pack")
	}
	// Archive root = the common parent of the **input arguments** (same as tar/zip):
	// pack out.hcax dir -> stores dir/a/b/c. The old implementation rooted at
	// commonRoot(**files**) — when all files live in subdirectories (e.g. src/main/go/*.go),
	// the common root collapsed to src/main/go and every directory above it degraded to
	// basename: src/main/go/main.go became main.go and the go/ level vanished entirely —
	// the tree was flattened (data loss).
	root := commonRoot(inputs)
	be := newBackend(spec)

	chunkMap := map[[16]byte]*chunkMeta{}
	hardSeen := map[fileIDKey]int{} // (dev,ino) -> index of the first entry in fileEntries
	var chunkMetas []*chunkMeta
	var fileEntries []fileEntry
	var totalUncomp uint64

	// v8e: solid/raw regions go to temp files — chunk data is written out right after
	// transform and dropped, each piece only streams through memory (previously the
	// whole solid stream + raw region lived in RAM, the cause of ~5x input peak usage
	// when packing many large files). Compression reads back from temp files
	// (compressZstdStream / xzCompress / compressMT); memory drops to one chunk plus
	// encoder state.
	csf, err := os.CreateTemp("", "hcax-solid-")
	if err != nil {
		fatal("temp file: %v", err)
	}
	rsf, err := os.CreateTemp("", "hcax-raw-")
	if err != nil {
		fatal("temp file: %v", err)
	}
	// Cleanup goes through addCleanup: defer does not run under fatal(os.Exit) and
	// would leak temp files.
	addCleanup(func() {
		csf.Close()
		rsf.Close()
		os.Remove(csf.Name())
		os.Remove(rsf.Name())
	})
	var trainSamples [][]byte
	var trainBytes int
	var compSolidSize, rawRegionSize int64
	nStored, nComp := 0, 0

	// Sum sizes first for the progress display. Packing hundreds of MB takes tens of
	// seconds; total silence reads as a hang. Progress goes to stderr only, keeping
	// stdout machine-readable.
	var totalBytes uint64
	for _, fp := range files {
		if fi, e := os.Lstat(fp); e == nil {
			totalBytes += uint64(fi.Size())
		}
	}
	showProgress := totalBytes >= 32<<20
	var lastReport uint64

	blk := make([]byte, 1<<20)
	for fi, fp := range files {
		if showProgress && totalUncomp-lastReport >= 16<<20 {
			lastReport = totalUncomp
			fmt.Fprintf(os.Stderr, "\r  chunking %d/%d files, %.0f%%   ",
				fi+1, len(files), 100.0*float64(totalUncomp)/float64(totalBytes))
		}
		f, err := os.Open(fp)
		if err != nil {
			// One unreadable file (permission/deleted) must not sink the whole backup:
			// warn and skip.
			fmt.Fprintf(os.Stderr, "warning: skipped unreadable file %s: %v\n", fp, err)
			skipped = append(skipped, fp)
			continue
		}
		rel := storedName(fp, root)
		// This error must not be ignored: st is nil on failure, and st.Size() /
		// st.ModTime() / fileKeyOf(st) below would nil-deref and crash the whole
		// backup. Treat missing stat as unreadable, same skip strategy as above.
		st, serr := f.Stat()
		if serr != nil || st == nil {
			fmt.Fprintf(os.Stderr, "warning: skipped unreadable file %s: %v\n", fp, serr)
			f.Close()
			skipped = append(skipped, fp)
			continue
		}

		// Hard links (v11): the second and later names of the same inode reuse the
		// first entry's chunk list — CDC dedup would hit the same chunks anyway, so
		// this costs no archive space and saves a full read/chunk pass.
		if first, dup := hardSeen[fileKeyOf(st)]; dup {
			src := fileEntries[first]
			fileEntries = append(fileEntries, fileEntry{
				name: rel, size: src.size, mtime: uint64(st.ModTime().Unix()),
				nano: st.ModTime().UnixNano(), mode: entryMode(st),
				chunks: src.chunks, xform: src.xform, isHard: true, link: src.name,
			})
			totalUncomp += src.size
			f.Close()
			continue
		}
		if k, ok := fileKey(st); ok && k.nlink > 1 {
			hardSeen[k] = len(fileEntries) // index this entry is about to get
		}
		var curChunks []uint32
		var xfProbe byte    // file-level probe transform for zstd; declared before onChunk to capture
		rasterMode := false // raster image: file-level 2-D prediction/color decorrelation already applied, no chunk-level transform
		var fileXform byte  // file-level transform (v7): recorded in the file table, inverted on unpack

		// solid (ultra): whole file into the solid stream (CDC off), cross-file
		// redundancy is left to lzma2; otherwise CDC dedup applies.
		var ch *chunker
		if spec.solid {
			ch = newChunkerMinMax(nil, 4<<20, 256<<20)
		} else {
			ch = newChunker(nil)
		}
		ch.onChunk = func(cb []byte) {
			hh := hash16(cb)
			// v8: dedup before transform — duplicate chunks reference the first
			// occurrence, skipping expensive preprocessing (otherwise memory/GC
			// explodes on large files).
			if !spec.solid {
				if cm, exists := chunkMap[hh]; exists {
					curChunks = append(curChunks, cm.idx)
					return
				}
			}
			var xf byte
			var tb []byte
			switch {
			case rasterMode:
				xf, tb = xfNone, cb // raster already 2-D predicted at file level
			case be.spec.backend == "zstd":
				// best (zstd): probe the transform once per file (cheap, keeps speed)
				xf, tb = xfProbe, applyXform(xfProbe, cb, false)
			default:
				xf, tb = be.chooseTransform(cb)
			}
			// v8d: write straight into the solid/raw regions; chunk data no longer
			// goes through cm.data (avoids double residency "cm.data + compSolid").
			// On xfNone, tb==cb (applyXform/chooseTransform return the original for
			// xfNone, no forced copy); bytes.Buffer.Write copies it out, so pooled
			// buffers can be reused safely. Only zstd training samples are copied on
			// demand (capped at 64MiB).
			if spec.backend == "zstd" && len(tb) >= 200 && len(tb) <= 128<<10 &&
				len(trainSamples) < 2000 && trainBytes < 64<<20 {
				s := append([]byte(nil), tb...)
				trainSamples = append(trainSamples, s)
				trainBytes += len(s)
			}
			cm := &chunkMeta{hash: hh, uncomp: uint32(len(cb)), xform: xf, idx: uint32(len(chunkMetas))}
			if be.shouldStore(tb) {
				cm.stored = true
				cm.offset = uint64(rawRegionSize)
				n, e := rsf.Write(tb)
				if e != nil {
					fatal("write raw region: %v", e)
				}
				rawRegionSize += int64(n)
				nStored++
			} else {
				cm.stored = false
				cm.offset = uint64(compSolidSize)
				n, e := csf.Write(tb)
				if e != nil {
					fatal("write solid stream: %v", e)
				}
				compSolidSize += int64(n)
				nComp++
			}
			if !spec.solid {
				chunkMap[hh] = cm
			}
			chunkMetas = append(chunkMetas, cm)
			curChunks = append(curChunks, cm.idx)
		}

		// Probe the first 64 bytes: raster detection (BMP/TGA/PNM) / zstd file-level
		// transform probe.
		head := make([]byte, 64)
		np, herr := io.ReadFull(f, head)
		// Short files return ErrUnexpectedEOF (with np = bytes actually read), which
		// is normal; anything else is a real read failure and must not be ignored.
		if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
			fatal("read %s: %v", fp, herr)
		}
		if rasterLikely(head[:np]) && st.Size() <= rasterMaxBytes {
			// Old code wrote rest, _ := io.ReadAll(f): a read error silently produced
			// a short buffer while size still came from stat — the pack "succeeded",
			// surfacing as "size mismatch" only at unpack.
			rest, rerr := io.ReadAll(f)
			if rerr != nil {
				fatal("read %s: %v", fp, rerr)
			}
			whole := append(append([]byte{}, head[:np]...), rest...)
			// Raster transforms act across chunks (prediction depends on whole-image
			// geometry), so apply to the whole file before chunking; gate candidate
			// transforms by size and take one that is actually smaller — never worse.
			rxf := be.chooseRasterXform(whole)
			if rxf != xfNone && rasterApply(rxf, whole, true) {
				rasterMode = true // no chunk-level transform on top
				fileXform = rxf
				for off := 0; off < len(whole); off += len(blk) {
					end := off + len(blk)
					if end > len(whole) {
						end = len(whole)
					}
					ch.write(whole[off:end])
				}
			} else {
				if be.spec.backend == "zstd" {
					xfProbe = detectXform(head[:np])
				}
				ch.write(head[:np])
				ch.write(rest)
			}
			ch.flush()
			f.Close()
			sz := sizedRead(fp, st.Size(), uint64(np)+uint64(len(rest)))
			fileEntries = append(fileEntries, fileEntry{
				name: rel, size: sz, mtime: uint64(st.ModTime().Unix()),
				nano: st.ModTime().UnixNano(),
				mode: entryMode(st), chunks: curChunks, xform: fileXform,
			})
			totalUncomp += sz
			continue
		}
		if np > 0 {
			if be.spec.backend == "zstd" {
				xfProbe = detectXform(head[:np])
			}
			ch.write(head[:np])
		}
		nread := uint64(np)
		for {
			n, re := f.Read(blk)
			if n > 0 {
				ch.write(blk[:n])
				nread += uint64(n)
			}
			if re == io.EOF {
				break
			}
			if re != nil {
				fatal("read %s: %v", fp, re)
			}
		}
		ch.flush()
		f.Close()

		sz := sizedRead(fp, st.Size(), nread)
		fileEntries = append(fileEntries, fileEntry{
			name: rel, size: sz, mtime: uint64(st.ModTime().Unix()),
			nano: st.ModTime().UnixNano(),
			mode: entryMode(st), chunks: curChunks,
		})
		totalUncomp += sz
	}
	if showProgress {
		fmt.Fprintf(os.Stderr, "\r  chunking %d/%d files, 100%%   \n", len(files), len(files))
	}
	if os.Getenv("HCAX_T") != "" {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(os.Stderr, "[t] after-chunking GoAlloc=%dMB Sys=%dMB nChunks=%d\n", ms.Alloc/1048576, ms.Sys/1048576, len(chunkMetas))
	}

	// Directory entries (incl. empty dirs): no data chunks, only name/permission/time,
	// so unpack restores the full directory structure.
	for _, d := range dirs {
		rel := storedName(d, root)
		if rel == "." || rel == "" {
			continue
		}
		di, err := os.Stat(d)
		if err != nil {
			continue
		}
		fileEntries = append(fileEntries, fileEntry{
			name: rel, size: 0, mtime: uint64(di.ModTime().Unix()),
			nano: di.ModTime().UnixNano(),
			mode: entryMode(di), isDir: true,
		})
	}

	// Symlinks (v9): store only the link target string, no content, no data chunks.
	// Size records the target length (same as tar; informational only, not archive data).
	for _, lp := range links {
		tgt, err := os.Readlink(lp)
		if err != nil {
			fatal("read symlink %s: %v", lp, err)
		}
		li, err := os.Lstat(lp)
		if err != nil {
			fatal("stat %s: %v", lp, err)
		}
		fileEntries = append(fileEntries, fileEntry{
			name: storedName(lp, root), size: uint64(len(tgt)),
			mtime: uint64(li.ModTime().Unix()), nano: li.ModTime().UnixNano(),
			mode:   entryMode(li),
			isLink: true, link: tgt,
		})
	}

	// v8d: bucketing already happened inside onChunk (chunks written to
	// compSolid/rawRegion and released immediately), so no cm.data staging remains.
	// Compressible/raw data each live in one place only; peak memory for large packs
	// drops from ~5x input to ~1.5x input.

	// lzma2 dict converges on compressible data size; lc/pb adapt during compression
	// (full-stream trials for single stream, sampled trials for parallel).
	be.dictParam = dictFor(int(compSolidSize))
	if os.Getenv("HCAX_T") != "" {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(os.Stderr, "[t] bucket done nComp=%d nStored=%d compSolid=%d raw=%d | GoAlloc=%dMB Sys=%dMB\n",
			nComp, nStored, compSolidSize, rawRegionSize, ms.Alloc/1048576, ms.Sys/1048576)
	}

	// zstd training dictionary (best for similar files): train on the copied samples
	// above, then compress that group with the dictionary.
	var trainDict []byte
	if spec.backend == "zstd" && len(trainSamples) >= 4 {
		// BuildDict: Contents = samples (code table), History = representative
		// samples (dictionary body, >=8B).
		var hist []byte
		for _, s := range trainSamples {
			hist = append(hist, s...)
			if len(hist) >= 100<<10 {
				break
			}
		}
		if len(hist) < 8 {
			hist = trainSamples[0]
		}
		if d, e := zstd.BuildDict(zstd.BuildDictOptions{
			ID:       1,
			Contents: trainSamples,
			History:  hist,
			Offsets:  [3]int{1, 4, 8},
			Level:    zstd.EncoderLevelFromZstd(spec.level),
		}); e == nil && len(d) > 0 {
			trainDict = d
			opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(spec.level)), zstd.WithEncoderDict(trainDict)}
			if spec.window > 0 {
				wl := spec.window
				if wl > zstdMaxWindowLog {
					wl = zstdMaxWindowLog
				}
				opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
			}
			if enc, e2 := zstd.NewWriter(io.Discard, opts...); e2 == nil {
				be.zEncDict = enc
				be.hasDict = true
			}
		}
	}
	be.trainDict = trainDict // v8e: reuse the dictionary to build the output encoder when streaming
	trainSamples = nil       // v8d: dictionary built, release samples

	// Compress the solid stream:
	// - zstd (fast/best): stream from the temp file (compressZstdStream), memory is
	//   one chunk plus encoder;
	// - lzma2 with compressible data >= mmtMinBytes: parallel groups (mmt) for speed,
	//   each group reads its slice from the temp file;
	// - otherwise (small/medium lzma2): single-stream full trial, reading the solid
	//   stream back into memory (<64MiB, safe).
	var frame []byte
	if be.spec.backend == "zstd" {
		if compSolidSize > 0 {
			if _, e := csf.Seek(0, io.SeekStart); e != nil {
				fatal("seek solid stream: %v", e)
			}
			t0 := time.Now()
			frame = be.compressZstdStream(csf)
			if os.Getenv("HCAX_T") != "" {
				fmt.Fprintf(os.Stderr, "[t] compressZstdStream %v\n", time.Since(t0))
			}
			// The training dictionary goes into the archive, and on small corpora it
			// can cost more than it saves: measured 114KB dict on a 200KB corpus,
			// 57% of the archive, overall ratio over 100%. On small data compare
			// compress-twice vs true total (frame + dict) and use the dict only when
			// actually smaller. On large corpora the dict overhead is negligible and
			// a level-19 pass is expensive, so no comparison there.
			if be.hasDict && compSolidSize <= dictTrialMaxBytes {
				if _, e := csf.Seek(0, io.SeekStart); e != nil {
					fatal("seek solid stream: %v", e)
				}
				be.hasDict = false
				alt := be.compressZstdStream(csf)
				if len(alt) < len(frame)+len(be.trainDict) {
					frame = alt
					be.trainDict = nil // dict not worth it: skip the dict region
					if os.Getenv("HCAX_T") != "" {
						fmt.Fprintf(os.Stderr, "[t] dict dropped: %d B dict not worth it\n", len(be.trainDict))
					}
				} else {
					be.hasDict = true
				}
				trainDict = be.trainDict
			}
		}
	} else if be.spec.mmt && nComp > 1 && compSolidSize >= mmtMinBytes {
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] -> mmt path G=%d\n", runtime.NumCPU())
		}
		t0 := time.Now()
		be.tuneLZMA2(csf, compSolidSize) // large corpus: sampled trial (full-stream trial too costly), then parallel groups
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] tuneLZMA2 %v\n", time.Since(t0))
		}
		t0 = time.Now()
		frame = be.compressMT(chunkMetas, csf, compSolidSize)
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] compressMT %v\n", time.Since(t0))
		}
	} else {
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] -> single-stream path\n")
		}
		t0 := time.Now()
		var solidData []byte
		if compSolidSize > 0 {
			csf.Seek(0, io.SeekStart)
			solidData, err = io.ReadAll(csf)
			if err != nil {
				fatal("read solid stream: %v", err)
			}
		}
		frame = be.compressLZMA2Best(solidData) // small/medium: trial pb=2/4 on the full stream, reuse the winner
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] compressLZMA2Best %v\n", time.Since(t0))
		}
	}

	// Atomic write: temp file in the same dir first, rename only after all writes
	// succeeded. Plain os.Create(outPath) truncates an existing archive to 0 at open —
	// on mid-write failure (disk full most likely) the old backup is gone and the new
	// one is half-written: one failure destroys both. See util.go.
	aw := openArchiveForWrite(outPath)
	out := aw.f
	addCleanup(aw.abort) // 失败路径删掉半成品; 成功时 commit 已把 tmp 置空, 空操作
	// Metadata (chunk + file tables) is serialized and compressed together (v6): plain
	// metadata often dominates an archive, compression pays off hugely.
	// Stream-level checksum (replaces per-chunk hashes: per-chunk hashes are random
	// data, incompressible and costly with many small files).
	var solidHash, rawHash [8]byte
	if compSolidSize > 0 {
		csf.Seek(0, io.SeekStart)
		h := hashReader(csf)
		copy(solidHash[:], h[:8])
	}
	if rawRegionSize > 0 {
		rsf.Seek(0, io.SeekStart)
		h := hashReader(rsf)
		copy(rawHash[:], h[:8])
	}
	wv := writeVer(fileEntries, precise)
	metaRaw := serializeMeta(chunkMetas, fileEntries, solidHash, rawHash, wv)
	metaFrame := be.compress(metaRaw)

	// Training dict region (stored raw: the dict itself is nearly incompressible)
	var dictBuf bytes.Buffer
	if len(trainDict) > 0 {
		binary.Write(&dictBuf, binary.LittleEndian, uint32(1))
		binary.Write(&dictBuf, binary.LittleEndian, uint32(len(trainDict)))
		dictBuf.Write(trainDict)
	} else {
		binary.Write(&dictBuf, binary.LittleEndian, uint32(0))
	}

	// header(v6, 48B): magic ver code window flag
	//                | metaCompLen metaRawLen compLen rawLen | nChunks nFiles
	var hdr bytes.Buffer
	hdr.WriteString(magic)
	hdr.Write([]byte{wv, spec.code, byte(spec.window), 0})
	binary.Write(&hdr, binary.LittleEndian, uint64(len(metaFrame)))
	binary.Write(&hdr, binary.LittleEndian, uint64(len(metaRaw)))
	binary.Write(&hdr, binary.LittleEndian, uint64(len(frame)))
	binary.Write(&hdr, binary.LittleEndian, uint64(rawRegionSize))
	binary.Write(&hdr, binary.LittleEndian, uint32(len(chunkMetas)))
	binary.Write(&hdr, binary.LittleEndian, uint32(len(fileEntries)))
	// trailer: "XACH" + totalUncomp(8) + nFiles(4) + nChunks(4)
	var trl bytes.Buffer
	trl.WriteString(trailerMag)
	binary.Write(&trl, binary.LittleEndian, totalUncomp)
	binary.Write(&trl, binary.LittleEndian, uint32(len(fileEntries)))
	binary.Write(&trl, binary.LittleEndian, uint32(len(chunkMetas)))

	// 布局: metaFrame | dataFrame | rawRegion | dictRegion
	mustWrite(out, hdr.Bytes(), "header")
	mustWrite(out, metaFrame, "metadata frame")
	mustWrite(out, frame, "data frame")
	if rawRegionSize > 0 {
		if _, err := rsf.Seek(0, io.SeekStart); err != nil {
			fatal("temp file seek failed: %v", err)
		}
		mustCopy(out, rsf, "raw region")
	}
	mustWrite(out, dictBuf.Bytes(), "training dict region")
	mustWrite(out, trl.Bytes(), "trailer")
	if err := out.Sync(); err != nil {
		fatal("fsync failed (disk may be full): %v", err)
	}
	sz, serr := out.Stat()
	// Stat before Close: the fd is dead after Close, size would be unavailable
	if err := out.Close(); err != nil {
		fatal("close archive failed (disk may be full): %v", err)
	}
	if serr != nil {
		fatal("stat archive failed: %v", serr)
	}
	aw.commit() // 到此才算"打包成功": 旧归档在这一刻才被完整的新归档替换

	raw := totalUncomp
	// All-empty archives have raw==0; dividing would yield +Inf% — "-" is more honest
	ratio := "-"
	if raw > 0 {
		ratio = fmt.Sprintf("%.2f%%", 100.0*float64(sz.Size())/float64(raw))
	}
	runCleanups() // 成功路径也要删临时文件(清理钩子本身幂等, 重复执行无副作用)
	fmt.Printf("packed: %s  raw %d B -> %d B  ratio %s  mode=%s  chunks=%d (comp %d / raw %d)\n",
		outPath, raw, sz.Size(), ratio, mode, len(chunkMetas), nComp, nStored)
	if nExcluded > 0 {
		fmt.Printf("excluded %d entries (--exclude)\n", nExcluded)
	}
	if len(skipped) > 0 {
		fmt.Printf("skipped %d entries (unreadable or non-regular): %s%s\n",
			len(skipped), strings.Join(skipped[:minInt(3, len(skipped))], ", "),
			func() string {
				if len(skipped) > 3 {
					return fmt.Sprintf(" and %d more", len(skipped))
				}
				return ""
			}())
	}
}
