package main

// Global utilities: cleanup hooks, fatal exit, in-archive path safety.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var cleanupFns []func()

func addCleanup(f func()) {
	cleanupFns = append(cleanupFns, f)
}

func runCleanups() {
	for i := len(cleanupFns) - 1; i >= 0; i-- {
		cleanupFns[i]()
	}
	cleanupFns = nil
}

// The exit action is isolated so tests can swap it for panic and assert on
// "corrupt archive" branches as ordinary failures (os.Exit would kill the whole
// test process and every later case).
var fatalExit = os.Exit

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	runCleanups()
	fatalExit(1)
}

// Write results must be checked. Write/Copy only return an error on disk full,
// quota or IO failure — they never panic. Old code called out.Write(...) without
// looking at the return value, so a full disk silently produced a **truncated
// archive/file**: header, metadata and checksums were generated as if the write
// succeeded, verification passed, and the damage surfaced only at restore time.
// That is the worst kind of failure — it does not error, it lies.
func mustWrite(f *os.File, b []byte, what string) {
	if len(b) == 0 {
		return
	}
	if _, err := f.Write(b); err != nil {
		fatal("write %s(%s) failed: %v", what, f.Name(), err)
	}
}

func mustCopy(dst io.Writer, src io.Reader, what string) {
	if _, err := io.Copy(dst, src); err != nil {
		fatal("write %s failed: %v", what, err)
	}
}

// Reads need the same treatment as mustWrite: a short ReaderAt read leaves zeros
// in the buffer without erroring or panicking, silently corrupting data until
// unpack fails. compressMT / concatChunksFile used to be written this way.
func mustReadAt(r io.ReaderAt, buf []byte, off int64, what string) {
	if len(buf) == 0 {
		return
	}
	n, err := r.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		fatal("read %s failed (offset %d): %v", what, off, err)
	}
	if n != len(buf) {
		fatal("read %s short read (offset %d, want %d got %d)", what, off, len(buf), n)
	}
}

// ---------------- archive write: temp file first, then rename ----------------
//
// Old code used os.Create(outPath). os.Create is O_TRUNC — opening truncates an
// existing file to 0 bytes. Packs run for minutes and failures are most likely at
// the final write step (disk full, quota, IO error): at that moment the old backup
// is already gone and the new one is half-written — **one failure destroys both**.
// mustWrite only prevents silence; it cannot rescue the truncated old archive.
//
// So: write a temp file in the same directory, then rename over the target after
// all writes succeed. rename within one filesystem is atomic — either the complete
// new archive is there, or the old one is untouched.

type archiveWriter struct {
	f       *os.File
	outPath string
	tmp     string // non-empty = writing a same-dir temp file; commit must rename
}

func openArchiveForWrite(outPath string) *archiveWriter {
	dir := filepath.Dir(outPath)
	if dir == "" {
		dir = "."
	}
	// The temp file must live in the same directory: rename across filesystems fails (EXDEV).
	if f, err := os.CreateTemp(dir, ".hcax-new-"); err == nil {
		// CreateTemp always uses 0600; renaming directly would make the archive owner-only.
		// New archives use 0644 (matching os.Create under the default umask); overwriting an
		// existing archive keeps its original mode so repacking does not silently change it.
		mode := os.FileMode(0o644)
		if fi, e := os.Stat(outPath); e == nil {
			mode = fi.Mode().Perm()
		}
		f.Chmod(mode)
		return &archiveWriter{f: f, outPath: outPath, tmp: f.Name()}
	}
	// Same dir not writable (but the target itself may be) -> fall back to the old
	// way; better than refusing to pack.
	f, err := os.Create(outPath)
	if err != nil {
		fatal("create %s: %v", outPath, err)
	}
	return &archiveWriter{f: f, outPath: outPath}
}

// abort closes and deletes the half-written file. After commit, tmp is empty, so
// calling it again is a no-op — cleanup hooks run unconditionally, must be idempotent.
func (w *archiveWriter) abort() {
	if w.tmp == "" {
		return
	}
	w.f.Close()
	os.Remove(w.tmp)
	w.tmp = ""
}

// commit renames the temp file to the target archive after all writes succeeded.
func (w *archiveWriter) commit() {
	if w.tmp == "" {
		return
	}
	if err := os.Rename(w.tmp, w.outPath); err != nil {
		fatal("archive rename failed (%s -> %s): %v", w.tmp, w.outPath, err)
	}
	w.tmp = "" // already moved; later cleanup hooks must not delete it
}

func safeName(name string) bool {
	if name == "" {
		return false
	}
	if filepath.IsAbs(name) {
		return false
	}
	for _, sep := range []string{"/", "\\"} {
		for _, p := range strings.Split(name, sep) {
			if p == ".." {
				return false
			}
		}
	}
	return true
}

// joinOut builds the unpack target path and double-checks it stays inside outDir.
func joinOut(outDir, name string) string {
	target := filepath.Join(outDir, filepath.FromSlash(name))
	root := filepath.Clean(outDir) + string(os.PathSeparator)
	if !strings.HasPrefix(target, root) {
		fatal("illegal path (escapes unpack dir): %s", name)
	}
	return target
}
