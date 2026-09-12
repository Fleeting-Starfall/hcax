//go:build windows

package main

import "os"

// Windows 没有 (dev, ino) 这种稳定可用的文件身份, 直接放弃硬链接识别:
// 退化为"每个名字各存一份", 内容去重仍然生效, 只是解包后不再共享。
func fileKey(fi os.FileInfo) (fileIDKey, bool) {
	return fileIDKey{}, false
}

func fileKeyOf(fi os.FileInfo) fileIDKey {
	k, _ := fileKey(fi)
	return k
}
