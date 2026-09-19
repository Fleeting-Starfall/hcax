//go:build windows

package main

import "os"

// Windows lacks a stable (dev, ino) file identity, so hard-link detection is disabled:
// every name is stored separately; content dedup still applies, but unpacked files no
// longer share inodes.
func fileKey(fi os.FileInfo) (fileIDKey, bool) {
	return fileIDKey{}, false
}

func fileKeyOf(fi os.FileInfo) fileIDKey {
	k, _ := fileKey(fi)
	return k
}
