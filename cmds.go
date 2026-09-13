package main

// 只读命令: list / info / verify —— 它们都不需要展开数据区。

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

// 归档里只存了权限位(0777)和 mtime, 类型由标志位决定 —— 拼回一个 os.FileMode
// 只为借用它的 String() 打印 "drwxr-xr-x" 这种人眼一眼能读的形式。
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

// 人类可读的大小: 字节数在列表里一眼看不出量级(100000 还是 100000000?)
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
	// 按路径排序: 打包顺序是 文件→目录→链接 三组分着来的, 直接打印看着是乱的。
	// 只排副本, 不动 a.files —— 解包依赖其原顺序(固实流顺序读)。
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
				suffix = " -> " + fe.link // 符号链接
			case fe.isHard:
				suffix = " => " + fe.link // 硬链接: 指向归档内首个名字(共享同一 inode)
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
	fmt.Printf("共 %d 条目 (%d 文件 / %d 目录 / %d 链接), 原大小 %d B (%s), 唯一块 %d\n",
		len(items), nFile, nDir, nLink, tot, humanSize(tot), len(a.chunks))
}

// info: 打印容器内部布局。配合 FORMAT.md 用来核对/调试格式 —— 光看归档大小
// 无法判断"到底是数据压得好, 还是元数据占了大头"、"有没有走原样存储"这些问题。
func infoArchive(archivePath string) {
	a := openArchive(archivePath)
	fi, _ := a.src.Stat()
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
	fmt.Printf("归档: %s\n", archivePath)
	fmt.Printf("  版本      v%d\n", a.ver)
	fmt.Printf("  模式      %s (后端 %s)\n", modeName(a.spec.code), a.spec.backend)
	fmt.Printf("  条目      %d 个 (文件 %d / 目录 %d / 链接 %d)\n",
		len(a.files), len(a.files)-nDir-nLink, nDir, nLink)
	fmt.Printf("  块        %d (可压 %d / 原样 %d)\n", len(a.chunks), nComp, nStored)
	fmt.Printf("  块数据    %d B (去重后)\n", rawSum)
	fmt.Printf("  归档大小  %d B\n", sz)
	fmt.Printf("---- 布局 ----\n")
	hdrLen := int64(48)
	if a.ver < 6 {
		hdrLen = a.dataStart // v2=24, v3~v5=32
	}
	fmt.Printf("  头部      %d B\n", hdrLen)
	fmt.Printf("  元数据    %d B (压缩) -> %d B (原始)\n", a.metaCompLen, a.metaRawLen)
	fmt.Printf("  数据区    %d B  %s\n", a.compLen, pct(a.compLen, sz))
	fmt.Printf("  原样区    %d B  %s\n", a.rawLen, pct(a.rawLen, sz))
	fmt.Printf("  字典区    %d B  %s\n", dictLen, func() string {
		if dictLen > 8 {
			return "含训练字典"
		}
		return "无"
	}())
	fmt.Printf("  尾部      %d B\n", int64(20))
	fmt.Printf("  合计      %d B (文件 %d B)\n", a.dataStart+a.compLen+a.rawLen+dictLen+20, sz)
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
	checked := "固实流/原样区哈希一致"
	if a.hashLen == 0 {
		a.verifyStream()
	} else {
		checked = "逐块哈希一致"
		for i := range a.chunks {
			_ = a.readChunk(uint32(i), true)
		}
	}
	el := time.Since(t0)
	rate := ""
	if el > 0 && tot >= 1<<20 { // 小归档算出来的速率没意义(还容易显示成 0.0 MB/s)
		rate = fmt.Sprintf(", %.1f MB/s", float64(tot)/el.Seconds()/1048576)
	}
	fmt.Printf("校验通过: %s; %d 个条目 / %d 块, 解压数据 %d B (%s), 耗时 %v%s\n",
		checked, len(a.files), len(a.chunks), tot, humanSize(tot), el.Round(time.Millisecond), rate)
}
