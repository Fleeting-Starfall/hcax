package main

// 全局小工具: 清理钩子、报错退出、归档内路径安全。

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

// 退出动作单独抽出来: 测试里会把它换成 panic, 好把"归档损坏"这类分支当成
// 普通失败来断言(否则 os.Exit 会直接干掉整个测试进程, 后面的用例全跑不了)。
var fatalExit = os.Exit

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	runCleanups()
	fatalExit(1)
}

// 写盘必须检查返回值。Write/Copy 在磁盘满、超配额、IO 出错时只是返回 error,
// 不会 panic —— 老代码一路 out.Write(...) 不看返回值, 于是磁盘满时会静默产出
// 一个**截断的归档/文件**: 头部、元数据、校验码都是按"写成功"生成的, 看上去
// 完好, 校验也过得去, 等到某天真要恢复才发现内容不对。这是最坏的一类失败 ——
// 它不报错, 它骗人。
func mustWrite(f *os.File, b []byte, what string) {
	if len(b) == 0 {
		return
	}
	if _, err := f.Write(b); err != nil {
		fatal("写 %s(%s) 失败: %v", what, f.Name(), err)
	}
}

func mustCopy(dst io.Writer, src io.Reader, what string) {
	if _, err := io.Copy(dst, src); err != nil {
		fatal("写 %s 失败: %v", what, err)
	}
}

// ---------------- 归档落盘: 必须先写临时文件再 rename ----------------
//
// 老代码直接 os.Create(outPath)。os.Create 是 O_TRUNC, 一打开就把已有文件
// 截成 0 字节。打包动辄跑几分钟, 而失败最容易发生在最后写盘那一步(磁盘满、
// 超配额、IO 出错)—— 那一刻旧的备份已经被清空, 新的又只写了一半:
// **一次失败同时毁掉两份**。mustWrite 只能保证"不静默", 救不回被截断的旧归档。
//
// 所以改成: 在同一目录下写临时文件, 全部写成功后再 rename 覆盖目标。
// rename 在同一文件系统内是原子的 —— 要么拿到完整的新归档, 要么旧的原封不动。

type archiveWriter struct {
	f       *os.File
	outPath string
	tmp     string // 非空 = 当前写的是同目录临时文件, 收尾需 rename
}

func openArchiveForWrite(outPath string) *archiveWriter {
	dir := filepath.Dir(outPath)
	if dir == "" {
		dir = "."
	}
	// 临时文件必须在**同一目录**: rename 跨文件系统会失败(EXDEV)
	if f, err := os.CreateTemp(dir, ".hcax-new-"); err == nil {
		// CreateTemp 固定 0600, 直接改名会让归档变得只有自己能读。
		// 新建按 0644(与 os.Create 在默认 umask 下一致); 覆盖已有归档时
		// 沿用它原本的权限, 免得"重新打包一次"顺手把权限改了。
		mode := os.FileMode(0o644)
		if fi, e := os.Stat(outPath); e == nil {
			mode = fi.Mode().Perm()
		}
		f.Chmod(mode)
		return &archiveWriter{f: f, outPath: outPath, tmp: f.Name()}
	}
	// 同目录不可写(但目标文件本身可能可写) -> 退回老做法, 总比打不了包强
	f, err := os.Create(outPath)
	if err != nil {
		fatal("create %s: %v", outPath, err)
	}
	return &archiveWriter{f: f, outPath: outPath}
}

// abort: 关掉并删掉半成品。commit 之后 tmp 已置空, 再调一次是空操作 ——
// 清理钩子会无条件跑, 这里必须幂等。
func (w *archiveWriter) abort() {
	if w.tmp == "" {
		return
	}
	w.f.Close()
	os.Remove(w.tmp)
	w.tmp = ""
}

// commit: 写盘全部成功后调用, 把临时文件改名成目标归档。
func (w *archiveWriter) commit() {
	if w.tmp == "" {
		return
	}
	if err := os.Rename(w.tmp, w.outPath); err != nil {
		fatal("归档落盘失败(%s -> %s): %v", w.tmp, w.outPath, err)
	}
	w.tmp = "" // 已经搬过去了, 后面的清理钩子不要再删
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

// 拼出解包目标路径, 并二次确认结果仍在 outDir 之内(纵深防御)
func joinOut(outDir, name string) string {
	target := filepath.Join(outDir, filepath.FromSlash(name))
	root := filepath.Clean(outDir) + string(os.PathSeparator)
	if !strings.HasPrefix(target, root) {
		fatal("非法路径(逃逸出解包目录): %s", name)
	}
	return target
}
