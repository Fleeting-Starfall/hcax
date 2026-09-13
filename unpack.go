package main

// 解包侧: 打开归档、惰性展开固实流、还原文件/目录/符号链接/硬链接。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func openArchive(path string) *archive {
	f, err := os.Open(path)
	if err != nil {
		fatal("open %s: %v", path, err)
	}
	// 句柄需在整个解包过程存活(原样区/数据区按需 ReadAt), 故不能用 defer 关闭:
	// defer 在 fatal(os.Exit) 下不执行, 交给统一清理钩子。
	addCleanup(func() { f.Close() })
	hdr := make([]byte, 48)
	if _, err := io.ReadFull(f, hdr[:8]); err != nil {
		fatal("读头失败: %v", err)
	}
	if string(hdr[0:4]) != magic {
		fatal("不是 HCAX 文件")
	}
	ver := hdr[4]
	if ver < 2 || ver > version {
		fatal("版本不兼容: %d", ver)
	}
	spec := modeSpec{code: hdr[5], window: int(hdr[6])}
	for _, m := range modes {
		if m.code == spec.code {
			spec.backend, spec.level, spec.solid, spec.mmt = m.backend, m.level, m.solid, m.mmt
		}
	}
	be := newBackend(spec)

	// ================= v6/v7: 压缩元数据 + 精简块表 =================
	if ver >= 6 {
		if _, err := io.ReadFull(f, hdr[8:48]); err != nil {
			fatal("读头(v6+)失败: %v", err)
		}
		metaCompLen := binary.LittleEndian.Uint64(hdr[8:16])
		metaRawLen := binary.LittleEndian.Uint64(hdr[16:24])
		compLen := binary.LittleEndian.Uint64(hdr[24:32])
		rawLen := binary.LittleEndian.Uint64(hdr[32:40])
		nChunks := binary.LittleEndian.Uint32(hdr[40:44])
		nFiles := binary.LittleEndian.Uint32(hdr[44:48])
		metaOff := int64(48)
		dataOff := metaOff + int64(metaCompLen)
		rawOff := dataOff + int64(compLen)
		dictOff := rawOff + int64(rawLen)

		checkTailMagic(f, rawOff+int64(rawLen))
		checkHeaderBounds(f, metaCompLen, metaRawLen, compLen, rawLen, nChunks, nFiles)

		// 1) 先装载训练字典(元数据帧可能就是用该字典压的)
		if _, err := f.Seek(dictOff, io.SeekStart); err == nil {
			var dc uint32
			if binary.Read(f, binary.LittleEndian, &dc) == nil {
				for i := 0; i < int(dc); i++ {
					var dl uint32
					if binary.Read(f, binary.LittleEndian, &dl) != nil {
						break
					}
					d := make([]byte, dl)
					if _, err := io.ReadFull(f, d); err != nil {
						break
					}
					if spec.backend == "zstd" && i == 0 {
						if dec, e := zstd.NewReader(bytes.NewReader(nil), zstd.WithDecoderDicts(d)); e == nil {
							be.zDecDict = dec
						}
						be.dictBytes = d // 流式解压时需用它重建解码器
					}
				}
			}
		}
		// 2) 解压并解析元数据
		f.Seek(metaOff, io.SeekStart)
		metaFrame := make([]byte, metaCompLen)
		if _, err := io.ReadFull(f, metaFrame); err != nil {
			fatal("读元数据失败: %v", err)
		}
		metaRaw := be.decompress(metaFrame)
		if uint64(len(metaRaw)) != metaRawLen {
			fatal("元数据长度不符: 期望 %d 实际 %d", metaRawLen, len(metaRaw))
		}
		chunks, files, sh, rh := parseMeta(metaRaw, nChunks, nFiles, ver)
		// 3) 数据区: 只记录位置, 不读不解压 —— 按需(惰性)解压见 ensureSolid
		a := &archive{spec: spec, be: be, src: f, dataStart: dataOff,
			compLen: int64(compLen), rawOff: rawOff, rawLen: int64(rawLen),
			metaCompLen: int64(metaCompLen), metaRawLen: int64(metaRawLen),
			chunks: chunks, files: files, hashLen: 0,
			solidHash: sh, rawHash: rh, ver: ver}
		a.checkCounts()
		return a
	}

	// ================= 旧版本 v2..v5 =================
	if _, err := io.ReadFull(f, hdr[8:24]); err != nil {
		fatal("读头失败: %v", err)
	}
	compLen := binary.LittleEndian.Uint64(hdr[8:16])
	var rawLen uint64
	var nChunks, nFiles uint32
	var dataStart int64
	if ver >= 3 {
		rawLen = binary.LittleEndian.Uint64(hdr[16:24])
		if _, err := io.ReadFull(f, hdr[24:32]); err != nil {
			fatal("读头(v3+)失败: %v", err)
		}
		nChunks = binary.LittleEndian.Uint32(hdr[24:28])
		nFiles = binary.LittleEndian.Uint32(hdr[28:32])
		dataStart = 32
	} else {
		nChunks = binary.LittleEndian.Uint32(hdr[16:20])
		nFiles = binary.LittleEndian.Uint32(hdr[20:24])
		dataStart = 24
	}

	// 同样在读数据区之前先确认文件没被截断(v2~v5 的尾部布局与 v6+ 一致)
	checkTailMagic(f, dataStart+int64(compLen)+int64(rawLen))

	// 先解析块表/文件表, 定位 v5 dict 区; 训练字典必须在解压帧之前装载(否则 zstd 报 unknown dictionary)
	ctOff := dataStart + int64(compLen) + int64(rawLen)
	f.Seek(ctOff, io.SeekStart)
	chunks := make([]chunkMeta, nChunks)
	var ctEntry int
	switch {
	case ver >= 5:
		ctEntry = 31
	case ver == 4:
		ctEntry = 30
	case ver == 3:
		ctEntry = 29
	default:
		ctEntry = 28
	}
	cb := make([]byte, ctEntry)
	for i := range chunks {
		io.ReadFull(f, cb)
		copy(chunks[i].hash[:], cb[0:16])
		chunks[i].offset = binary.LittleEndian.Uint64(cb[16:24])
		chunks[i].uncomp = binary.LittleEndian.Uint32(cb[24:28])
		chunks[i].stored = cb[28] == 1
		if ver >= 4 {
			chunks[i].xform = cb[29]
		}
		if ver >= 5 {
			chunks[i].dictID = cb[30]
		}
	}
	// file table (同时累计长度用于定位 dict 区)
	var ftLen int64
	files := make([]fileEntry, nFiles)
	for i := range files {
		var nl uint16
		binary.Read(f, binary.LittleEndian, &nl)
		ftLen += 2 + int64(nl) + 8 + 8 + 4 + 4 // nameLen + name + size(uint64) + mtime(uint64) + mode(uint32) + nchunks(uint32)
		nb := make([]byte, nl)
		io.ReadFull(f, nb)
		var size, mtime uint64
		var mode uint32
		binary.Read(f, binary.LittleEndian, &size)
		binary.Read(f, binary.LittleEndian, &mtime)
		binary.Read(f, binary.LittleEndian, &mode)
		var nc uint32
		binary.Read(f, binary.LittleEndian, &nc)
		ftLen += 4 * int64(nc)
		idxs := make([]uint32, nc)
		for j := range idxs {
			binary.Read(f, binary.LittleEndian, &idxs[j])
		}
		files[i] = fileEntry{name: string(nb), size: size, mtime: mtime, mode: mode, chunks: idxs}
	}
	// dict region (仅 v5): 必须在解压帧之前装载
	if ver == 5 {
		dictOff := ctOff + int64(ctEntry)*int64(nChunks) + ftLen
		f.Seek(dictOff, io.SeekStart)
		var dc uint32
		binary.Read(f, binary.LittleEndian, &dc)
		for i := 0; i < int(dc); i++ {
			var dl uint32
			binary.Read(f, binary.LittleEndian, &dl)
			d := make([]byte, dl)
			io.ReadFull(f, d)
			if spec.backend == "zstd" && i == 0 {
				if dec, e := zstd.NewReader(bytes.NewReader(nil), zstd.WithDecoderDicts(d)); e == nil {
					be.zDecDict = dec
				}
				be.dictBytes = d
			}
		}
	}

	// 数据区同样只记位置(惰性解压): 旧版布局为 [数据区][原样区][块表][文件表][字典区]
	a := &archive{spec: spec, be: be, src: f, dataStart: dataStart,
		compLen: int64(compLen), rawOff: dataStart + int64(compLen), rawLen: int64(rawLen),
		chunks: chunks, files: files, hashLen: 16, ver: ver}
	a.checkCounts()
	return a
}

func checkTailMagic(f *os.File, dataEnd int64) {
	const trailerLen = 20
	fi, err := f.Stat()
	if err != nil {
		return
	}
	sz := fi.Size()
	if sz < 48+trailerLen {
		fatal("归档过短(%d B): 不是完整的 HCAX 文件", sz)
	}
	off := sz - trailerLen
	if dataEnd > off {
		fatal("归档被截断: 数据区需延伸到 %d B, 但文件只有 %d B", dataEnd, sz)
	}
	tb := make([]byte, 4)
	if _, err := f.ReadAt(tb, off); err != nil {
		fatal("读尾部失败: %v", err)
	}
	if string(tb) != trailerMag {
		fatal("尾部魔数不符: 归档被截断或损坏(尾部 %d 字节处不是 %s)", off, trailerMag)
	}
}

func checkHeaderBounds(f *os.File, metaCompLen, metaRawLen, compLen, rawLen uint64, nChunks, nFiles uint32) {
	fi, err := f.Stat()
	if err != nil {
		return
	}
	sz := uint64(fi.Size())
	if metaCompLen > sz || compLen > sz || rawLen > sz {
		fatal("头部长度字段异常(归档损坏): metaComp=%d comp=%d raw=%d, 文件仅 %d B",
			metaCompLen, compLen, rawLen, sz)
	}
	if 48+metaCompLen+compLen+rawLen+20 > sz {
		fatal("头部布局超出文件大小(归档损坏或截断)")
	}
	if metaRawLen > 1<<30 {
		fatal("元数据长度异常(%d B): 归档损坏", metaRawLen)
	}
	// 块表项至少 5B, 文件表项至少 21B —— 据此给条目数设上界
	if uint64(nChunks)*5 > metaRawLen || uint64(nFiles)*16 > metaRawLen {
		fatal("头部条目数与元数据长度矛盾(nChunks=%d nFiles=%d, 元数据 %d B): 归档损坏",
			nChunks, nFiles, metaRawLen)
	}
}

// 逻辑一致性: 每个文件声明的 size 必须等于它引用的块长度之和。
// 光校验"数据区字节流没变"是不够的 —— "哪个文件由哪些块拼成"存在元数据里,
// 它若被改坏(比如 size 字段翻掉一位), 流哈希照样对得上, verify 却报"通过",
// 于是拿着一个自相矛盾的归档当完好。这条检查只是做加法, 代价极小。
func (a *archive) checkLogical() {
	for _, fe := range a.files {
		if fe.isDir || fe.isLink {
			continue // 目录不产生块; 链接的 size 记的是目标字符串长度
		}
		var sum uint64
		for _, ix := range fe.chunks {
			sum += uint64(a.chunks[ix].uncomp)
		}
		if sum != fe.size {
			fatal("元数据与数据自相矛盾: 文件 %s 声明 %d B, 但它引用的块合计 %d B",
				fe.name, fe.size, sum)
		}
	}
}

// 条目数一致性校验: 尾部记录的 nFiles/nChunks 必须与头部一致(解析元数据后调用)
func (a *archive) checkCounts() {
	const trailerLen = 20
	fi, err := a.src.Stat()
	if err != nil {
		return
	}
	tb := make([]byte, trailerLen)
	if _, err := a.src.ReadAt(tb, fi.Size()-trailerLen); err != nil {
		fatal("读尾部失败: %v", err)
	}
	nf := binary.LittleEndian.Uint32(tb[12:16])
	nc := binary.LittleEndian.Uint32(tb[16:20])
	if nf != uint32(len(a.files)) || nc != uint32(len(a.chunks)) {
		fatal("尾部与头部不一致(文件数 %d≠%d 或 块数 %d≠%d): 归档已损坏",
			nf, len(a.files), nc, len(a.chunks))
	}
}

// addProgress 累加已解出字节并按阈值回报进度。
// 解包几百 MB 的归档要跑几十秒, 全程无反馈跟卡死没区别(打包侧早就有了, 解包侧一直缺)。
func (a *archive) addProgress(n uint64) {
	if !a.progShow || a.progTotal == 0 || a.progDone >= a.progTotal {
		return // 已报完 100% 就别再打了(后续 0 字节的目录条目还会进来)
	}
	a.progDone += n
	if a.progDone-a.progLast >= 16<<20 || a.progDone >= a.progTotal {
		a.progLast = a.progDone
		fmt.Fprintf(os.Stderr, "\r  解包中 %.0f%%   ", 100.0*float64(a.progDone)/float64(a.progTotal))
		if a.progDone >= a.progTotal {
			fmt.Fprintln(os.Stderr)
		}
	}
}

func (a *archive) ensureSolid(need uint64) {
	if a.solidEOF || int64(need) <= a.solidSize {
		return
	}
	if a.solidFile == nil {
		tf, err := os.CreateTemp("", "hcax-solid-")
		if err != nil {
			fatal("临时文件: %v", err)
		}
		a.solidFile = tf
		addCleanup(func() {
			tf.Close()
			os.Remove(tf.Name())
		})
		var r io.Reader
		var closeFn func()
		if a.spec.backend == "lzma2" {
			fr := io.NewSectionReader(a.src, a.dataStart, a.compLen)
			xr, err := xz.NewReader(fr)
			if err != nil {
				fatal("xz reader: %v", err)
			}
			r, closeFn = xr, nil
		} else {
			fr := io.NewSectionReader(a.src, a.dataStart, a.compLen)
			r, closeFn, err = a.be.decompressReader(fr)
			if err != nil {
				fatal("解压器: %v", err)
			}
		}
		a.solidR = r
		if closeFn != nil {
			addCleanup(closeFn)
		}
	}
	want := int64(need) - a.solidSize
	n, err := io.CopyN(a.solidFile, a.solidR, want)
	a.solidSize += n
	if err != nil {
		if err == io.EOF {
			a.solidEOF = true
		} else {
			fatal("解压固实流: %v", err)
		}
	}
}

// 把固实流完整解压到底(校验等需要全量数据的场景)
func (a *archive) drainSolid() {
	for !a.solidEOF {
		a.ensureSolid(uint64(a.solidSize) + 1<<20)
	}
}

// 预先解压到"这批文件所需的最大偏移", 使后续 readChunk 不再触发解压。
// 抽取归档前部的少量文件时, 可省掉解压整条流的开销。
func (a *archive) prepareFor(files []fileEntry) {
	var maxEnd uint64
	for _, fe := range files {
		for _, ix := range fe.chunks {
			if ix >= uint32(len(a.chunks)) {
				continue
			}
			cm := a.chunks[ix]
			if cm.stored {
				continue
			}
			if e := cm.offset + uint64(cm.uncomp); e > maxEnd {
				maxEnd = e
			}
		}
	}
	if maxEnd > 0 {
		a.ensureSolid(maxEnd)
	}
}

func (a *archive) readChunk(idx uint32, verify bool) []byte {
	cm := a.chunks[idx]
	out := make([]byte, cm.uncomp)
	if cm.stored {
		// 原样区不解压, 直接从归档文件按偏移读 —— 零额外内存
		if _, err := a.src.ReadAt(out, a.rawOff+int64(cm.offset)); err != nil {
			fatal("读原样区: %v", err)
		}
	} else {
		a.ensureSolid(cm.offset + uint64(cm.uncomp))
		if _, err := a.solidFile.ReadAt(out, int64(cm.offset)); err != nil {
			fatal("读固实流: %v", err)
		}
	}
	out = applyXform(cm.xform, out, true)
	if verify && a.hashLen > 0 { // 旧版: 逐块校验; v6 走流级校验(verifyStream)
		h := hash16(out)
		n := a.hashLen
		if n > 16 {
			n = 16
		}
		if !bytes.Equal(h[:n], cm.hash[:n]) {
			fatal("块 %d 校验失败(哈希不匹配)", idx)
		}
	}
	return out
}

// 流级校验(v6): 校验解压后的固实流与原样区
func (a *archive) verifyStream() {
	if a.hashLen != 0 {
		return
	}
	if a.compLen > 0 {
		a.drainSolid() // 校验需要全量数据
		a.solidFile.Seek(0, io.SeekStart)
		h := hashReader(a.solidFile)
		if !bytes.Equal(h[:8], a.solidHash[:]) {
			fatal("固实流校验失败(数据损坏)")
		}
	}
	if a.rawLen > 0 {
		h := hashReader(io.NewSectionReader(a.src, a.rawOff, a.rawLen))
		if !bytes.Equal(h[:8], a.rawHash[:]) {
			fatal("原样区校验失败(数据损坏)")
		}
	}
}

func (a *archive) extractFile(fe fileEntry, outDir string, verify bool) {
	if !safeName(fe.name) {
		fatal("非法路径: %s", fe.name)
	}
	defer a.addProgress(fe.size) // 所有 return 分支都要计入进度
	target := joinOut(outDir, fe.name)
	// 覆盖计数: 解到非空目录时会静默改写同名条目。tar 也这样, 但至少该让人知道
	// "这次解包动了多少已有的东西" —— 否则误以为解进了空目录, 事后发现被改了都不知道。
	// 目录条目不计: MkdirAll 对已存在的目录是空操作(不是改写), 而且目录往往会
	// 因为"先给文件建父目录"而提前存在, 算进去会把首次解包也报成覆盖。
	if !fe.isDir {
		if _, err := os.Lstat(target); err == nil {
			a.overwrote++
		}
	}
	// 目录条目(含空目录): 先按可写权限建出来, 权限与 mtime 记下来,
	// 等所有条目(含它下面的文件)都写完再统一补 —— 否则往里写文件既写不进
	// (只读目录), 也会把 mtime 冲成"现在"。
	if fe.isDir {
		if err := mkdirUnderOut(outDir, target); err != nil {
			fatal("mkdir: %v", err)
		}
		a.dirTodos = append(a.dirTodos, dirTodo{path: target, mode: fe.mode, nano: entryNano(fe)})
		return
	}
	// 符号链接(v9): 重建链接本身, 不写内容。
	// 注: Go 标准库没有 lutimes, 链接自身的 mtime 无法还原(只还原普通文件/目录的)。
	if fe.isLink {
		if err := mkdirUnderOut(outDir, filepath.Dir(target)); err != nil {
			fatal("mkdir: %v", err)
		}
		os.Remove(target) // 覆盖同名已存在的文件/链接
		if err := os.Symlink(fe.link, target); err != nil {
			fatal("symlink %s: %v", fe.name, err)
		}
		return
	}
	if err := mkdirUnderOut(outDir, filepath.Dir(target)); err != nil {
		fatal("mkdir: %v", err)
	}
	// 硬链接(v11): 尽量还原成共享 inode 的硬链接。
	// 源不在这次解包范围内时(extract 只抽了一部分)退化成普通文件 —— 块索引还在,
	// 内容照样完整, 不会解出一个空壳。
	if fe.isHard {
		src := joinOut(outDir, fe.link)
		if _, e := os.Lstat(src); e == nil {
			os.Remove(target)
			if e := os.Link(src, target); e == nil {
				return
			}
		}
	}
	// 防"先建链接再写穿": 归档里若先有条目在 target 处建了符号链接(比如 -> /etc),
	// 后续同名文件条目经 os.Create 会跟随该链接写到链接指向的目录里去。
	// 写文件前先把已存在的符号链接摘掉, 保证只会写解包目录内部。
	if li, e := os.Lstat(target); e == nil && li.Mode()&os.ModeSymlink != 0 {
		os.Remove(target)
	}
	out, err := os.Create(target)
	if err != nil {
		fatal("create %s: %v", target, err)
	}
	// Close 也要检查: 数据在内核页缓存里, Close 之前的 Write 成功不代表真的落盘
	defer func() {
		if err := out.Close(); err != nil {
			fatal("关闭 %s 失败(磁盘可能已满): %v", fe.name, err)
		}
	}()
	// 文件级光栅变换(v7): 变换跨块生效, 必须先拼出整文件再逆变换
	if len(fe.chunks) > 0 && fe.xform >= xfRasterMed && fe.xform <= xfRasterRowRctMed {
		// 这条路径要**整文件进内存**。打包侧有 rasterMaxBytes 门控, 写出来的归档
		// 不可能超; 所以超限只可能是损坏或恶意构造(声明一个巨大的"光栅图",
		// 内容全是零 —— 几 KB 的归档就能让人吃掉几十 GB 内存)。在读块之前挡掉。
		if fe.size > rasterMaxBytes {
			fatal("文件 %s 声明 %d B, 超过光栅变换上限(%d B): 归档损坏",
				fe.name, fe.size, rasterMaxBytes)
		}
		buf := []byte{}
		for _, ix := range fe.chunks {
			buf = append(buf, a.readChunk(ix, verify)...)
		}
		if !rasterApply(fe.xform, buf, false) {
			fatal("文件 %s: 光栅逆变换失败", fe.name)
		}
		if uint64(len(buf)) != fe.size {
			fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, len(buf))
		}
		mustWrite(out, buf, fe.name)
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, entryTime(fe), entryTime(fe))
		return
	}
	if len(fe.chunks) > 0 {
		first := a.readChunk(fe.chunks[0], verify)
		// 兼容旧版(v2~v6): 旧归档无文件级变换记录, 按"BMP 一定做过二维预测"的规则逆运算。
		// v7 起变换显式记录在 fe.xform, 不再走启发式(否则"故意不做变换"的文件会被误逆变换)。
		if a.ver < 7 && len(first) >= 2 && first[0] == 'B' && first[1] == 'M' {
			buf := append([]byte{}, first...)
			for _, ix := range fe.chunks[1:] {
				buf = append(buf, a.readChunk(ix, verify)...)
			}
			if _, ok := bmpInfoLegacy(buf); ok {
				pred2DLegacy(buf, false) // 旧版用的是二维梯度预测
			}
			if uint64(len(buf)) != fe.size {
				fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, len(buf))
			}
			mustWrite(out, buf, fe.name)
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, entryTime(fe), entryTime(fe))
			return
		}
		mustWrite(out, first, fe.name)
		if uint64(len(first)) == fe.size {
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, entryTime(fe), entryTime(fe))
			return
		}
		var written2 uint64 = uint64(len(first))
		for _, ix := range fe.chunks[1:] {
			cb := a.readChunk(ix, verify)
			mustWrite(out, cb, fe.name)
			written2 += uint64(len(cb))
		}
		if written2 != fe.size {
			fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, written2)
		}
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, entryTime(fe), entryTime(fe))
		return
	}
	var written uint64
	if written != fe.size {
		fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, written)
	}
	os.Chmod(target, os.FileMode(fe.mode))
	os.Chtimes(target, entryTime(fe), entryTime(fe))
}

// 归一用户给出的抽取目标: 反斜杠按分隔符处理, 去掉结尾多余的分隔符,
// 这样 "d/sub" 与 "d/sub/" 是同一个意思。
func normAsk(s string) string {
	s = filepath.ToSlash(s)
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}

// 判断归档内的一个条目是否被用户的某个抽取目标命中。三种命中方式(由精确到宽松):
//  1. 完整路径相等:   "d/sub/r.bin"
//  2. 基名相等:       "r.bin"   —— 记不住全路径时的兜底
//  3. 目录前缀:       "d/sub"   —— 抽出该子树(含子树下的目录条目)
//
// 老实现只支持 1)2), 于是 extract 归档 d/sub 只会建出一个空的 d/sub 目录,
// 里面的文件一个都没抽出来; 用户会以为命令成功、数据到手了。
func matchEntry(name, ask string) bool {
	n := filepath.ToSlash(name)
	if n == ask {
		return true
	}
	if path.Base(n) == ask {
		return true
	}
	return strings.HasPrefix(n, ask+"/")
}

// 用户指定的目标一个都没命中, 十有八九是拼错了 —— 静默"抽取完成: 0 文件"
// 会让人误以为成功。全部落空直接报错; 部分落空只警告(批量抽取时常见)。
func reportUnmatched(asks []string, hit map[string]bool) {
	var miss []string
	for _, p := range asks {
		if !hit[p] {
			miss = append(miss, p)
		}
	}
	if len(miss) == 0 {
		return
	}
	msg := "归档中没有匹配 " + strings.Join(miss, ", ") + " 的条目"
	if len(miss) == len(asks) {
		fatal("%s(用 list 查看归档内的真实路径)", msg)
	}
	fmt.Fprintf(os.Stderr, "警告: %s\n", msg)
}

// 只有"够大"才显示进度: 小归档一闪而过, 进度反而成了噪音
func (a *archive) setProgress(items []fileEntry) {
	var tot uint64
	for _, fe := range items {
		tot += fe.size
	}
	if tot < 32<<20 {
		return
	}
	a.progShow = true
	a.progTotal = tot
}

// 目录的权限与 mtime 最后统一补(见 archive.dirTodos)。
// 顺序: 深的在前 —— 万一某个父目录的权限挡住了子目录, 子目录已经先设完了。
func (a *archive) applyDirMeta() {
	todos := a.dirTodos
	a.dirTodos = nil
	sort.SliceStable(todos, func(i, j int) bool {
		return len(todos[i].path) > len(todos[j].path)
	})
	for _, d := range todos {
		if d.mode != 0 {
			if err := os.Chmod(d.path, os.FileMode(d.mode)); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 无法设置目录权限 %s: %v\n", d.path, err)
			}
		}
		if d.nano != 0 {
			t := time.Unix(0, d.nano)
			if err := os.Chtimes(d.path, t, t); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 无法设置目录时间 %s: %v\n", d.path, err)
			}
		}
	}
}

// 在解包目录内建目录, 途中任何一段若是符号链接就先摘掉。
//
// os.MkdirAll 会**跟随**符号链接: 恶意归档可以先放一个条目 link -> /etc(或 -> ../..),
// 再放一个 "link/xxx" 的文件/目录条目, MkdirAll 就会顺着链接把目录建到解包目录
// 外面去, 随后的 os.Create 也就写穿了。原先只在"写文件前检查 target 本身是不是
// 链接", 挡不住"链接出现在路径中间"这一种。
//
// 归档里出现"符号链接之下还有条目"本身就不合法(打包侧 Walk 不跟随链接, 写不出
// 这种结构), 所以直接把挡路的链接摘掉, 与上面写文件时的处理保持一致。
func mkdirUnderOut(outDir, target string) error {
	root := filepath.Clean(outDir)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return fmt.Errorf("路径逃逸出解包目录: %s", target)
	}
	cur := root
	rest := strings.TrimPrefix(target, root)
	for _, seg := range strings.Split(rest, string(os.PathSeparator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		if li, e := os.Lstat(cur); e == nil && li.Mode()&os.ModeSymlink != 0 {
			if e := os.Remove(cur); e != nil {
				return e
			}
		}
	}
	return os.MkdirAll(target, 0o755)
}

func overwriteNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("  (覆盖 %d 个已存在条目)", n)
}

func unpack(archivePath, outDir string, verify bool, only []string) {
	a := openArchive(archivePath)
	if verify {
		a.verifyStream() // v6 流级校验(旧版为空操作, 由 readChunk 逐块校验)
	}
	os.MkdirAll(outDir, 0o755)
	a.setProgress(a.files)
	if len(only) == 0 {
		// 全量解包: 顺序读取, 固实流按需推进即可
		for _, fe := range a.files {
			a.extractFile(fe, outDir, verify)
		}
		a.applyDirMeta()
		runCleanups()
		fmt.Printf("解包完成: %d 条目 -> %s  模式=%s 校验=%v%s\n",
			len(a.files), outDir, modeName(a.spec.code), verify, overwriteNote(a.overwrote))
		return
	}
	asks := make([]string, 0, len(only))
	for _, o := range only {
		asks = append(asks, normAsk(o))
	}
	var want []fileEntry
	hit := make(map[string]bool, len(asks))
	for _, fe := range a.files {
		for _, p := range asks {
			if matchEntry(fe.name, p) {
				want = append(want, fe)
				hit[p] = true
				break // 一个条目只解一次(多个条件同时命中时不重复)
			}
		}
	}
	reportUnmatched(asks, hit)
	a.setProgress(want)
	// 只解到目标文件的块末尾为止: 抽归档前部的小文件不必解压整条固实流
	a.prepareFor(want)
	for _, fe := range want {
		a.extractFile(fe, outDir, verify)
	}
	a.applyDirMeta()
	runCleanups()
	fmt.Printf("抽取完成: %d 文件 -> %s  模式=%s 校验=%v%s\n",
		len(want), outDir, modeName(a.spec.code), verify, overwriteNote(a.overwrote))
}
