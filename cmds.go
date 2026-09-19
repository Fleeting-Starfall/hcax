package main

// Read-only commands: list / info / verify — none of them inflate the data region.

import (
	"fmt"
	"os"
	"sort"
	"time"
)

func modeName(code byte) string {
	for k, m := range modes {
		if m.code == code {
			return k
		}
	}
	return "?"
}

// The archive stores only permission bits (0777) and mtime; the entry type comes from
// flags. Reconstruct an os.FileMode just to reuse its String() for readable output
// like "drwxr-xr-x".
func modeString(fe fileEntry) string {
	m := os.FileMode(fe.mode)
	switch {
	case fe.isDir:
		m |= os.ModeDir
	case fe.isLink:
		m |= os.ModeSymlink
	}
	return m.String()
}

// humanSize formats a byte count so its magnitude is obvious at a glance.
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for i := n / unit; i >= unit && exp < 4; i /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func listArchive(archivePath string, long bool) {
	a := openArchive(archivePath)
	// Sort by path: pack order groups files, dirs, then links, which looks arbitrary.
	// Sort a copy only — unpack relies on a.files' original order (solid-stream order).
	items := make([]fileEntry, len(a.files))
	copy(items, a.files)
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	var tot uint64
	var nFile, nDir, nLink int
	for _, fe := range items {
		switch {
		case fe.isDir:
			nDir++
		case fe.isLink:
			nLink++
		default:
			nFile++
		}
		tot += fe.size
		if long {
			t := "-"
			if fe.mtime != 0 {
				t = time.Unix(int64(fe.mtime), 0).Format("2006-01-02 15:04")
			}
			suffix := ""
			switch {
			case fe.isLink:
				suffix = " -> " + fe.link // symlink target
			case fe.isHard:
				suffix = " => " + fe.link // hard link to the first name sharing this inode
			case fe.isDir:
				suffix = "/"
			}
			fmt.Printf("  %s  %s  %12d  %s%s\n", modeString(fe), t, fe.size, fe.name, suffix)
			continue
		}
		switch {
		case fe.isDir:
			fmt.Printf("  %12s  %s/\n", "<dir>", fe.name)
		case fe.isLink:
			fmt.Printf("  %12s  %s -> %s\n", "<link>", fe.name, fe.link)
		default:
			fmt.Printf("  %12d  %s\n", fe.size, fe.name)
		}
	}
	fmt.Printf("total %d entries (%d files / %d dirs / %d links), raw size %d B (%s), unique chunks %d\n",
		len(items), nFile, nDir, nLink, tot, humanSize(tot), len(a.chunks))
	runCleanups() // success path must clean up too (see verifyArchive)
}

// info prints the container's internal layout for format debugging (see FORMAT.md).
// Archive size alone cannot tell whether data compressed well, metadata dominates, or
// raw storage was used.
func infoArchive(archivePath string) {
	a := openArchive(archivePath)
	fi, fierr := a.src.Stat()
	if fierr != nil || fi == nil {
		fatal("stat archive failed: %v", fierr)
	}
	sz := fi.Size()
	var nStored, nComp, rawSum uint64
	var nLink, nDir int
	for i := range a.chunks {
		if a.chunks[i].stored {
			nStored++
		} else {
			nComp++
		}
		rawSum += uint64(a.chunks[i].uncomp)
	}
	for i := range a.files {
		if a.files[i].isDir {
			nDir++
		} else if a.files[i].isLink {
			nLink++
		}
	}
	dictLen := sz - 20 - (a.rawOff + a.rawLen)
	if dictLen < 0 {
		dictLen = 0
	}
	pct := func(n, d int64) string {
		if d <= 0 {
			return "-"
		}
		return fmt.Sprintf("%.2f%%", 100.0*float64(n)/float64(d))
	}
	fmt.Printf("archive: %s\n", archivePath)
	fmt.Printf("  version   v%d\n", a.ver)
	fmt.Printf("  mode      %s (backend %s)\n", modeName(a.spec.code), a.spec.backend)
	fmt.Printf("  entries   %d (files %d / dirs %d / links %d)\n",
		len(a.files), len(a.files)-nDir-nLink, nDir, nLink)
	fmt.Printf("  chunks    %d (compressed %d / raw %d)\n", len(a.chunks), nComp, nStored)
	fmt.Printf("  chunk data %d B (after dedup)\n", rawSum)
	fmt.Printf("  archive size %d B\n", sz)
	fmt.Printf("---- layout ----\n")
	hdrLen := int64(48)
	if a.ver < 6 {
		hdrLen = a.dataStart // v2=24, v3~v5=32
	}
	fmt.Printf("  header    %d B\n", hdrLen)
	fmt.Printf("  metadata  %d B (compressed) -> %d B (raw)\n", a.metaCompLen, a.metaRawLen)
	fmt.Printf("  data      %d B  %s\n", a.compLen, pct(a.compLen, sz))
	fmt.Printf("  raw       %d B  %s\n", a.rawLen, pct(a.rawLen, sz))
	fmt.Printf("  dict      %d B  %s\n", dictLen, func() string {
		if dictLen > 8 {
			return "contains training dict"
		}
		return "none"
	}())
	fmt.Printf("  trailer   %d B\n", int64(20))
	fmt.Printf("  total     %d B (file %d B)\n", a.dataStart+a.compLen+a.rawLen+dictLen+20, sz)
	runCleanups()
}

func verifyArchive(archivePath string) {
	t0 := time.Now()
	a := openArchive(archivePath)
	var tot uint64
	for i := range a.chunks {
		tot += uint64(a.chunks[i].uncomp)
	}

	a.checkLogical()
	checked := "solid/raw stream hashes match"
	if a.hashLen == 0 {
		a.verifyStream()
	} else {
		checked = "per-chunk hashes match"
		for i := range a.chunks {
			_ = a.readChunk(uint32(i), true)
		}
	}
	el := time.Since(t0)
	rate := ""
	if el > 0 && tot >= 1<<20 { // rate for tiny archives is meaningless (often 0.0 MB/s)
		rate = fmt.Sprintf(", %.1f MB/s", float64(tot)/el.Seconds()/1048576)
	}
	fmt.Printf("verify OK: %s; %d entries / %d chunks, decompressed %d B (%s), took %v%s\n",
		checked, len(a.files), len(a.chunks), tot, humanSize(tot), el.Round(time.Millisecond), rate)
	// Explicit cleanup is required: verify calls readChunk -> ensureSolid, which creates
	// a solid-stream temp file (up to the full decompressed size) in the system temp dir.
	// The fatal path is covered by runCleanups inside fatal(), but the success path was
	// not — each verify left a tens-of-MB hcax-solid-* in /tmp. (infoArchive always had
	// it; list/verify missed it.)
	runCleanups()
}
