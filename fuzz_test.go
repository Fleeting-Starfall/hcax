package main

// randomized tests: hand-written cases cover only "thinkable" inputs, yet an R18-style
// bug -- "opens but never unpacks" on high-redundancy archives -- is exactly unthinkable.
// so let the machine hunt: random dir-tree round trips + random corruption behavior.
//
//   go test -run TestRandom -v ./...

import (
	"bytes"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- turn fatal into a caught panic to assert "clean failure" ----------

type fatalPanic struct{}

// runCatchingFatal runs fn; true means "failed cleanly" (fatal), false means it ran fine.
// a real runtime panic (index OOB / nil deref) propagates up
// and go test records it as a failure -- exactly what we hunt.
func runCatchingFatal(fn func()) (fataled bool) {
	old := fatalExit
	fatalExit = func(int) { panic(fatalPanic{}) }
	defer func() {
		fatalExit = old
		if r := recover(); r != nil {
			if _, ok := r.(fatalPanic); ok {
				fataled = true
				return
			}
			panic(r) // not fatal -- real crash, hand it to go test
		}
	}()
	fn()
	return false
}

// ---------- random directory tree ----------

var namePool = []string{
	"a.txt", "b.bin", "a..b.txt", "..weird", "sp ace.txt", "chinese-name.txt",
	".hidden", "x", "y.log", "ZZZ", "n1", "n2", "deep", "empty",
}

func randName(rng *rand.Rand, used map[string]bool) string {
	for i := 0; i < 50; i++ {
		n := namePool[rng.Intn(len(namePool))]
		if !used[n] {
			used[n] = true
			return n
		}
	}
	return "fallback"
}

// build a random dir tree. Content is reproducible (seeded rng) for re-running failures.
func buildRandomTree(t *testing.T, rng *rand.Rand, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var files []string // regular files built so far, for symlink/hardlink targets
	var dirs []string  // dirs: mode and mtime must be set after the whole tree exists --
	// adding children bumps a dir's mtime, and read-only too early blocks the writes
	var build func(dir string, depth int)
	build = func(dir string, depth int) {
		used := map[string]bool{}
		n := 3 + rng.Intn(6)
		for i := 0; i < n; i++ {
			name := randName(rng, used)
			p := filepath.Join(dir, name)
			switch {
			case depth < 3 && rng.Intn(4) == 0: // subdir
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				dirs = append(dirs, p)
				build(p, depth+1)
			case len(files) > 0 && rng.Intn(8) == 0: // symlink
				target, _ := filepath.Rel(dir, files[rng.Intn(len(files))])
				if rng.Intn(4) == 0 {
					target = "nowhere-" + name // dangling
				}
				if err := os.Symlink(target, p); err == nil {
					continue
				}
				fallthrough
			case len(files) > 0 && rng.Intn(10) == 0: // hardlink
				if err := os.Link(files[rng.Intn(len(files))], p); err == nil {
					files = append(files, p)
					continue
				}
				fallthrough
			default: // regular file
				size := rng.Intn(4) * rng.Intn(60000) // many 0-byte files, a few large
				buf := make([]byte, size)
				switch rng.Intn(3) {
				case 0: // random (incompressible)
					rng.Read(buf)
				case 1: // highly repetitive (internal dedup path)
					for j := range buf {
						buf[j] = byte(j % 7)
					}
				default: // compressible pseudo-text
					for j := range buf {
						buf[j] = byte('a' + (j+rng.Intn(3))%20)
					}
				}
				mode := os.FileMode(0o644)
				switch rng.Intn(4) {
				case 0:
					mode = 0o600
				case 1:
					mode = 0o755
				case 2:
					mode = 0o400 // read-only: wrong order would make extraction fail to write
				}
				if err := os.WriteFile(p, buf, mode); err != nil {
					t.Fatal(err)
				}
				randMeta(t, rng, p)
				files = append(files, p)
			}
		}
	}
	build(root, 0)
	// set dir metadata last (mirroring applyDirMeta on extract): deepest first,
	// else a read-only parent blocks setting its children
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		mode := os.FileMode(0o755)
		switch rng.Intn(3) {
		case 0:
			mode = 0o700
		case 1:
			mode = 0o500 // read-only dir: if mode took effect early, children could not be written
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		randMeta(t, rng, d)
	}
}

// a read-only dir (0500) blocks t.TempDir() cleanup of child files. The extracted tree
// also has 0500 dirs, so loosen the whole temp dir (not just the source tree).
// Cleanup runs LIFO, so this runs before TempDir removes itself.
func makeWritableOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

// random mtime (whole seconds: default stores seconds; whole seconds diff byte-clean).
// mode/mtime restore is the only guard for deferred dir-metadata changes -- content alone
func randMeta(t *testing.T, rng *rand.Rand, p string) {
	t.Helper()
	sec := 978307200 + rng.Int63n(20*365*24*3600) // within 20 years of 2001-01-01
	ts := time.Unix(sec, 0)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// ---------- directory tree comparison ----------

type node struct {
	kind  byte // 'f' file, 'd' dir, 'l' link
	size  int64
	link  string
	data  []byte
	ino   uint64
	mode  os.FileMode
	mtime int64 // Unix seconds
}

func scanTree(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			// link mtimes are unrestorable (Go has no lutimes); skip them
			out[rel] = node{kind: 'l', link: tgt}
		case fi.IsDir():
			out[rel] = node{kind: 'd', mode: fi.Mode().Perm(), mtime: fi.ModTime().Unix()}
		default:
			data, _ := os.ReadFile(p)
			out[rel] = node{kind: 'f', size: fi.Size(), data: data, ino: inoOf(fi),
				mode: fi.Mode().Perm(), mtime: fi.ModTime().Unix()}
		}
		return nil
	})
	return out
}

func inoOf(fi os.FileInfo) uint64 {
	if k, ok := fileKey(fi); ok {
		return k.ino
	}
	return 0
}

// checkMeta=false compares only presence and content (for partial extraction:
// parents are created incidentally, their archive entries were not selected, so no meta)
func diffTrees(t *testing.T, want, got map[string]node, ctx string) {
	diffTreesOpt(t, want, got, ctx, true)
}

func diffTreesOpt(t *testing.T, want, got map[string]node, ctx string, checkMeta bool) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := want[k]
		g, ok := got[k] // note: old code wrote w, ok := got[k], comparing got to itself,
		if !ok {        //       so content/mode/mtime were never really compared -- vacuous
			t.Errorf("%s: missing entry %s", ctx, k)
			continue
		}
		if w.kind != g.kind {
			t.Errorf("%s: %s kind mismatch (%c vs %c)", ctx, k, w.kind, g.kind)
			continue
		}
		switch w.kind {
		case 'l':
			if w.link != g.link {
				t.Errorf("%s: %s link target mismatch (%q vs %q)", ctx, k, w.link, g.link)
			}
		case 'f':
			if w.size != g.size || !bytes.Equal(w.data, g.data) {
				t.Errorf("%s: %s content mismatch (%d vs %d bytes)", ctx, k, w.size, g.size)
			}
		}
		if checkMeta && (w.kind == 'f' || w.kind == 'd') {
			if w.mode != g.mode {
				t.Errorf("%s: %s mode mismatch (%o vs %o)", ctx, k, w.mode, g.mode)
			}
			if w.mtime != g.mtime {
				t.Errorf("%s: %s mtime mismatch (%d vs %d)", ctx, k, w.mtime, g.mtime)
			}
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: extra entry %s", ctx, k)
		}
	}
}

// ---------- case 1: random dir tree x every mode round trip ----------

func TestRandomTreeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20260913))
	for iter := 0; iter < 4; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		for _, mode := range []string{"fast", "best", "max", "ultra"} {
			arc := filepath.Join(dir, "a_"+mode+".hcax")
			out := filepath.Join(dir, "o_"+mode)
			if runCatchingFatal(func() { pack([]string{src}, arc, mode, nil, false) }) {
				t.Fatalf("round %d [%s] pack failed", iter, mode)
			}
			if runCatchingFatal(func() { unpack(arc, out, false, nil) }) {
				t.Fatalf("round %d [%s] unpack failed", iter, mode)
			}
			diffTrees(t, want, scanTree(t, filepath.Join(out, "src")), "iter"+string(rune('0'+iter))+"/"+mode)
			// hardlinks must restore as shared inodes (content alone is not enough)
			checkHardlinks(t, want, filepath.Join(out, "src"), filepath.Join(out, "src"))
		}
	}
}

// ---------- case 1b: partial extraction -- exactly the asked entries, nothing more ----------
// extract matching (full path / basename / dir prefix) arrived after R13; before that
// "extract one subdir" made an empty dir and claimed success. Let the random tree hit it.

func TestRandomPartialExtract(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	for iter := 0; iter < 6; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		arc := filepath.Join(dir, "a.hcax")
		if runCatchingFatal(func() { pack([]string{src}, arc, "fast", nil, false) }) {
			t.Fatalf("round %d pack failed", iter)
		}
		// in-archive paths carry the src/ prefix
		var all []string
		for k := range want {
			all = append(all, filepath.ToSlash(filepath.Join("src", k)))
		}
		sort.Strings(all)
		if len(all) == 0 {
			continue
		}
		// pick 1-3 targets, alternating "full path" and "basename" phrasing
		asks := map[string]bool{}
		for i := 0; i < 1+rng.Intn(3); i++ {
			a := all[rng.Intn(len(all))]
			if rng.Intn(2) == 0 {
				a = path.Base(a)
			}
			asks[a] = true
		}
		var only []string
		for a := range asks {
			only = append(only, a)
		}
		// expected set: run every entry through matchEntry (same rules as extract side)
		exp := map[string]node{}
		for k, v := range want {
			full := filepath.ToSlash(filepath.Join("src", k))
			for _, a := range only {
				if matchEntry(full, a) {
					exp[full] = v
					break
				}
			}
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, false, only) }) {
			t.Fatalf("round %d extract failed only=%v", iter, only)
		}
		got := scanTree(t, out)
		// empty dirs appear in the result incidentally, not selected -- drop them from the scan
		for k, v := range got {
			if v.kind == 'd' {
				if _, ok := exp[k]; !ok {
					delete(got, k)
				}
			}
		}
		diffTreesOpt(t, exp, got, "iter"+string(rune('0'+iter))+" only="+joinStr(only), false)
	}
}

func joinStr(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// ---------- case 1c: --exclude random patterns ----------
// exclusion matches on "full path / path relative to pack root / any single segment";
// hand cases covered one. Randomize and assert excluded are absent, others present & correct.

func TestRandomExcludePack(t *testing.T) {
	rng := rand.New(rand.NewSource(31337))
	for iter := 0; iter < 6; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		// use a real extension so it excludes something without emptying everything
		exts := map[string]bool{}
		for k := range want {
			if e := filepath.Ext(k); e != "" {
				exts[e] = true
			}
		}
		var extList []string
		for e := range exts {
			extList = append(extList, e)
		}
		sort.Strings(extList)
		if len(extList) == 0 {
			continue
		}
		pat := "*" + extList[rng.Intn(len(extList))]
		arc := filepath.Join(dir, "a.hcax")
		if runCatchingFatal(func() { pack([]string{src}, arc, "fast", []string{pat}, false) }) {
			t.Fatalf("round %d pack failed", iter)
		}
		exp := map[string]node{}
		for k, v := range want {
			// a pattern matches **any segment**, so a hit on a dir name excludes
			// its whole subtree -- the expected set must compute it that way or false-miss
			drop := false
			for _, seg := range strings.Split(filepath.ToSlash(k), "/") {
				if ok, _ := filepath.Match(pat, seg); ok {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
			exp[k] = v
		}
		if len(exp) == 0 {
			continue // this round would exclude the whole tree; pick another pattern
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, false, nil) }) {
			t.Fatalf("round %d unpack failed", iter)
		}
		diffTrees(t, exp, scanTree(t, filepath.Join(out, "src")),
			"iter"+string(rune('0'+iter))+" exclude="+pat)
	}
}

// files sharing an inode on the source tree must share one after unpack
func checkHardlinks(t *testing.T, want map[string]node, srcRoot, outRoot string) {
	t.Helper()
	byIno := map[uint64][]string{}
	for k, v := range want {
		if v.kind == 'f' && v.ino != 0 {
			byIno[v.ino] = append(byIno[v.ino], k)
		}
	}
	for ino, names := range byIno {
		if len(names) < 2 {
			continue
		}
		var outIno uint64
		for _, n := range names {
			fi, err := os.Lstat(filepath.Join(outRoot, n))
			if err != nil {
				t.Errorf("hardlink %s missing after unpack: %v", n, err)
				continue
			}
			cur := inoOf(fi)
			if outIno == 0 {
				outIno = cur
			} else if cur != outIno {
				t.Errorf("hardlink group (inode %d) no longer shares inode: %v", ino, names)
			}
		}
	}
}

// ---------- case 2: random corruption -- must fail cleanly, never crash ----------

func TestRandomCorruptionDoesNotCrash(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	dir := t.TempDir()
	makeWritableOnCleanup(t, dir)
	src := filepath.Join(dir, "src")
	buildRandomTree(t, rng, src)
	arc := filepath.Join(dir, "a.hcax")
	pack([]string{src}, arc, "best", nil, false)
	orig, err := os.ReadFile(arc)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) < 200 {
		t.Skip("archive too small; corruption testing is meaningless")
	}
	var nFatal, nClean int
	for i := 0; i < 40; i++ {
		bad := append([]byte{}, orig...)
		switch rng.Intn(3) {
		case 0: // flip one byte
			bad[rng.Intn(len(bad))] ^= byte(1 << rng.Intn(8))
		case 1: // truncate
			bad = bad[:1+rng.Intn(len(bad)-1)]
		default: // zero out a span
			off := rng.Intn(len(bad))
			n := 1 + rng.Intn(64)
			for j := off; j < off+n && j < len(bad); j++ {
				bad[j] = 0
			}
		}
		p := filepath.Join(dir, "bad.hcax")
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "badout")
		os.RemoveAll(out)
		// anything but a panic passes: clean fatal and "still unpacked right" both count.
		// list never checks the data region (by design); data-region damage needs
		// unpack --verify to surface, so "rejected" means either one.
		rejected := runCatchingFatal(func() { listArchive(p, false) })
		os.RemoveAll(out)
		if runCatchingFatal(func() { unpack(p, out, true, nil) }) {
			rejected = true
		}
		if rejected {
			nFatal++
		} else {
			nClean++
		}
	}
	t.Logf("40 corrupted archives: %d cleanly rejected, %d hit harmless bytes (still readable)", nFatal, nClean)
	// self-check: zero rejections means the test does nothing (do not let it rot into decor)
	if nFatal == 0 {
		t.Error("none of 40 random corruptions was caught -- corruption detection may be a no-op")
	}
}

// ---------- write failures must surface, never silently truncate output ----------

func TestMustWriteReportsError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r.Close() // close the read end -> write returns EPIPE (Go ignores SIGPIPE on non-std fds)

	if !runCatchingFatal(func() { mustWrite(w, []byte("hello"), "test") }) {
		t.Error("write failure went unreported -- disk full would silently truncate output")
	}
	if !runCatchingFatal(func() { mustCopy(w, bytes.NewReader([]byte("hello")), "test") }) {
		t.Error("Copy failure went unreported")
	}
	// an empty slice must not write (else every empty file costs a pointless Write call)
	if runCatchingFatal(func() { mustWrite(w, nil, "test") }) {
		t.Error("writing empty content must not error")
	}
}
