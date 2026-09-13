package main

// 恶意归档的安全用例。
//
// 归档里的名字是**别人给的**, 不能信: 打包侧永远不会往归档里写 "../" 或指向
// 外面的符号链接, 但解包侧必须假设对方递过来的就是一个精心构造的归档。
// 这些用例全部是"先确认能攻进去, 再修"的路子 —— 修完如果又被人改回去, 测试会红。

import (
	"os"
	"path/filepath"
	"testing"
)

// 造一个最小的归档对象: 只用来调 extractFile, 不碰固实流
func emptyArchive() *archive { return &archive{} }

// 攻击 1: 先放一个指向解包目录外的符号链接, 再放一个"在该链接之下"的文件条目。
// 解包时 MkdirAll(父目录) 会**跟随**这个链接, 把目录建到外面去, 写文件也就写到外面了。
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
	// 条目 1: 符号链接 out/link -> <outside>
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link", isLink: true, link: outside, mode: 0o777}, out, false)
	}) {
		t.Fatal("建符号链接条目失败")
	}
	// 条目 2: 文件 link/sub/f.txt —— 顺着链接就写到 outside/sub/f.txt 了
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link/sub/f.txt", size: 0, mode: 0o644}, out, false)
	}) {
		t.Fatal("解出文件条目失败")
	}
	if _, err := os.Lstat(filepath.Join(outside, "sub")); err == nil {
		t.Fatalf("目录符号链接被跟随了: 在解包目录外建出了 %s", filepath.Join(outside, "sub"))
	}
	if _, err := os.Lstat(filepath.Join(outside, "sub", "f.txt")); err == nil {
		t.Fatalf("写穿符号链接: 文件被写到了 %s", filepath.Join(outside, "sub", "f.txt"))
	}
}

// 攻击 2: 目录条目本身落在符号链接之下(MkdirAll 直接建到外面)
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
		t.Fatal("建符号链接条目失败")
	}
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "link/deep", isDir: true, mode: 0o755}, out, false)
	}) {
		t.Fatal("建目录条目失败")
	}
	if _, err := os.Lstat(filepath.Join(outside, "deep")); err == nil {
		t.Fatalf("目录条目顺着符号链接建到了解包目录外: %s", filepath.Join(outside, "deep"))
	}
}

// 攻击 3: 符号链接指向解包目录内的上级(".."), 让后续条目绕出解包目录
func TestNoEscapeViaDotDotSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)
	a := emptyArchive()
	if runCatchingFatal(func() {
		// out/up -> out 的上一层(dir), 于是 out/up/outside 就是解包目录之外
		a.extractFile(fileEntry{name: "up", isLink: true, link: filepath.Join("..", "outside"), mode: 0o777}, out, false)
	}) {
		t.Fatal("建符号链接条目失败")
	}
	if runCatchingFatal(func() {
		a.extractFile(fileEntry{name: "up/pwned.txt", size: 0, mode: 0o644}, out, false)
	}) {
		t.Fatal("解出文件条目失败")
	}
	if _, err := os.Lstat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatalf("通过 ../ 符号链接写出了文件: %s", filepath.Join(outside, "pwned.txt"))
	}
}
