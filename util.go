package main

// 全局小工具: 清理钩子、报错退出、归档内路径安全。

import (
	"fmt"
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
