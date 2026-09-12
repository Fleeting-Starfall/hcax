//go:build !windows

package main

import (
	"os"
	"syscall"
)

// 硬链接识别需要 (dev, ino, nlink): 同一 inode 的多个名字就是硬链接。
// os.SameFile 也能判断, 但它是两两比较 —— 几万个文件时会退化成 O(n²);
// 而 (dev, ino) 可以直接当 map 键, 一次遍历搞定。
func fileKey(fi os.FileInfo) (fileIDKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIDKey{}, false
	}
	return fileIDKey{
		dev:   uint64(st.Dev),
		ino:   uint64(st.Ino),
		nlink: uint64(st.Nlink),
	}, true
}

// 取不到文件身份时的兜底: 零值在 hardSeen 里永远查不到, 等价于"不识别为硬链接"
func fileKeyOf(fi os.FileInfo) fileIDKey {
	k, _ := fileKey(fi)
	return k
}
