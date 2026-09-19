//go:build !windows

package main

import (
	"os"
	"syscall"
)

// Hard-link detection needs (dev, ino, nlink): multiple names for the same inode are
// hard links. os.SameFile also works but compares pairwise — O(n²) on tens of
// thousands of files; (dev, ino) works directly as a map key in one pass.
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

// fileKeyOf returns the zero key when identity is unavailable; it never matches hardSeen.
func fileKeyOf(fi os.FileInfo) fileIDKey {
	k, _ := fileKey(fi)
	return k
}
