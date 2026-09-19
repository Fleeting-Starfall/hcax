package main

// security cases for hostile archives.
//
// names in an archive are **someone else's**, never trusted: pack never writes "../"
// or outward symlinks, but unpack must assume a hand-crafted hostile archive.
// each case first proves the attack works, then guards it; regressing the fix turns red.

import (
	"os"
	"path/filepath"
	"testing"
)

// build a minimal archive object: only for extractFile, no solid stream involved
func emptyArchive() *archive { return &archive{} }

// attack 1: an outward symlink entry, then a file entry "under" that link.
// MkdirAll(parent) **follows** the link during unpack, building dirs and writing files outside.
func TestNoWriteThroughDirSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	a := emptyArchive()
	// entry 1: symlink out/link -> <outside>
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link", isLink: true, link: outside, mode: 0o777}, out, false)
	}) {
		t.Fatal("failed to build symlink entry")
	}
	// entry 2: file link/sub/f.txt -- follows the link straight to outside/sub/f.txt
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link/sub/f.txt", size: 0, mode: 0o644}, out, false)
	}) {
		t.Fatal("failed to extract file entry")
	}
	if _, err := os.Lstat(filepath.Join(outside, "sub")); err == nil {
		t.Fatalf("dir symlink followed: created %s outside the extract dir", filepath.Join(outside, "sub"))
	}
	if _, err := os.Lstat(filepath.Join(outside, "sub", "f.txt")); err == nil {
		t.Fatalf("wrote through symlink: file landed at %s", filepath.Join(outside, "sub", "f.txt"))
	}
}

// attack 2: a dir entry itself sits under a symlink (MkdirAll builds outside)
func TestNoMkdirThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)
	a := emptyArchive()
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link", isLink: true, link: outside, mode: 0o777}, out, false)
	}) {
		t.Fatal("failed to build symlink entry")
	}
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link/deep", isDir: true, mode: 0o755}, out, false)
	}) {
		t.Fatal("failed to build dir entry")
	}
	if _, err := os.Lstat(filepath.Join(outside, "deep")); err == nil {
		t.Fatalf("dir entry built outside the extract dir via symlink: %s", filepath.Join(outside, "deep"))
	}
}

// attack 3: a symlink pointing at a parent ("..") lets later entries escape the extract dir
func TestNoEscapeViaDotDotSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)
	a := emptyArchive()
	if runCatchingFatal(func() {
		// out/up -> parent of out (dir), so out/up/outside is outside the extract dir
		a.extractFile(fileEntry{name: "up", isLink: true, link: filepath.Join("..", "outside"), mode: 0o777}, out, false)
	}) {
		t.Fatal("failed to build symlink entry")
	}
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "up/pwned.txt", size: 0, mode: 0o644}, out, false)
	}) {
		t.Fatal("failed to extract file entry")
	}
	if _, err := os.Lstat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatalf("wrote a file through a ../ symlink: %s", filepath.Join(outside, "pwned.txt"))
	}
}
