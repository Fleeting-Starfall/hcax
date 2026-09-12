package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

type chunker struct {
	rh      uint64
	start   int
	scanned int
	pending []byte
	minS    int
	maxS    int
	mask    uint64
	gear    [256]uint64
	onChunk func([]byte)
}

type chunkMeta struct {
	hash   [16]byte
	offset uint64 // 可压块: 在 compSolid 中的偏移; 原样块: 在 rawRegion 中的偏移
	uncomp uint32
	stored bool // true=原样存储(跳过压缩器)
	xform  byte // 预处理类型(DELTA/BCJ), 解码时逆变换还原
	dictID byte // 训练字典索引(best 相似文件); 0=无
	data   []byte
	idx    uint32
}

type fileEntry struct {
	name   string
	size   uint64
	mtime  uint64
	mode   uint32
	chunks []uint32
	isDir  bool // 目录条目(空目录也保留)
	isLink bool // v9: 符号链接条目(存链接目标, 不存文件内容)
	link   string
	// xform: 文件级预处理标记(v7)。光栅图(BMP/TGA/PNM)在分块之前对整文件做过
	// 二维预测/色彩去相关, 该变换跨块生效, 故必须记在文件级而非块级。
	// 非光栅文件恒为 xfNone。
	xform byte
}

// 元数据序列化(v6): [流哈希16B] + 块表(5B/项: len4 + flags1) + 文件表
//   - 块偏移不再存储: 可压块在固实流中、原样块在原样区中都是按顺序紧凑排列, 解包时累加即得
//   - 不再逐块存哈希(每块 8B 随机数完全不可压, 大量小文件时开销巨大);
//     改为流级校验: 固实流 + 原样区 各存 8B 哈希。verify 时校验整条流。
func serializeMeta(chunks []*chunkMeta, files []fileEntry, solidHash, rawHash [8]byte) []byte {
	var b bytes.Buffer
	b.Write(solidHash[:])
	b.Write(rawHash[:])
	for _, cm := range chunks {
		binary.Write(&b, binary.LittleEndian, cm.uncomp)
		var f byte
		if cm.stored {
			f |= 1
		}
		f |= (cm.xform & 0x0f) << 1
		if cm.dictID != 0 {
			f |= 0x20
		}
		b.WriteByte(f)
	}
	for _, fe := range files {
		nb := []byte(fe.name)
		binary.Write(&b, binary.LittleEndian, uint16(len(nb)))
		b.Write(nb)
		var f byte
		if fe.isDir {
			f |= 1
		}
		f |= (fe.xform & 0x0f) << 1 // v7: 文件级光栅变换
		if fe.isLink {
			f |= 0x20 // v9: 符号链接
		}
		b.WriteByte(f)
		binary.Write(&b, binary.LittleEndian, fe.size)
		binary.Write(&b, binary.LittleEndian, uint32(fe.mtime))
		binary.Write(&b, binary.LittleEndian, uint16(fe.mode))
		binary.Write(&b, binary.LittleEndian, uint32(len(fe.chunks)))
		for _, ix := range fe.chunks {
			binary.Write(&b, binary.LittleEndian, ix)
		}
		if fe.isLink { // v9: 链接目标跟在该条目之后
			lb := []byte(fe.link)
			binary.Write(&b, binary.LittleEndian, uint16(len(lb)))
			b.Write(lb)
		}
	}
	return b.Bytes()
}

// 元数据是从"压缩帧"解压出来的字节流, 一旦归档损坏, 解压出的内容就是任意字节。
// 老实现逐字段裸解析, 越界直接 panic 崩掉(而不是报"归档损坏")。
// 这里在每处读取前做边界检查, 把 panic 转成明确的错误。

func need(m []byte, pos, n int, what string) {
	if pos < 0 || n < 0 || pos+n > len(m) {
		fatal("元数据损坏: 解析%s时越界(偏移 %d 需要 %d 字节, 元数据共 %d 字节)", what, pos, n, len(m))
	}
}

func parseMeta(m []byte, nChunks, nFiles uint32, ver byte) ([]chunkMeta, []fileEntry, [8]byte, [8]byte) {
	pos := 0
	var sh, rh [8]byte
	copy(sh[:], m[pos:pos+8])
	pos += 8
	copy(rh[:], m[pos:pos+8])
	pos += 8
	chunks := make([]chunkMeta, nChunks)
	var compOff, rawOff uint64
	for i := range chunks {
		need(m, pos, 5, "块表项")
		chunks[i].uncomp = binary.LittleEndian.Uint32(m[pos : pos+4])
		pos += 4
		f := m[pos]
		pos++
		chunks[i].stored = f&1 != 0
		chunks[i].xform = (f >> 1) & 0x0f
		if f&0x20 != 0 {
			chunks[i].dictID = 1
		}
		if chunks[i].stored {
			chunks[i].offset = rawOff
			rawOff += uint64(chunks[i].uncomp)
		} else {
			chunks[i].offset = compOff
			compOff += uint64(chunks[i].uncomp)
		}
	}
	files := make([]fileEntry, nFiles)
	for i := range files {
		need(m, pos, 2, "文件名长度")
		nl := int(binary.LittleEndian.Uint16(m[pos : pos+2]))
		pos += 2
		need(m, pos, nl+1+8+4+2+4, "文件表项")
		files[i].name = string(m[pos : pos+nl])
		pos += nl
		f := m[pos]
		pos++
		files[i].isDir = f&1 != 0
		if ver >= 7 {
			files[i].xform = (f >> 1) & 0x0f
		}
		files[i].isLink = ver >= 9 && f&0x20 != 0
		files[i].size = binary.LittleEndian.Uint64(m[pos : pos+8])
		pos += 8
		files[i].mtime = uint64(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		files[i].mode = uint32(binary.LittleEndian.Uint16(m[pos : pos+2]))
		pos += 2
		nc := int(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		need(m, pos, 4*nc, "文件块索引")
		if nc > 0 && uint32(nc) > nChunks {
			fatal("元数据损坏: 文件 %s 引用了 %d 个块, 但块表只有 %d 项", files[i].name, nc, nChunks)
		}
		idxs := make([]uint32, nc)
		for j := range idxs {
			idxs[j] = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
			if idxs[j] >= nChunks {
				fatal("元数据损坏: 文件 %s 引用了不存在的块 %d(共 %d 块)", files[i].name, idxs[j], nChunks)
			}
		}
		files[i].chunks = idxs
		if files[i].isLink {
			need(m, pos, 2, "链接目标长度")
			tl := int(binary.LittleEndian.Uint16(m[pos : pos+2]))
			pos += 2
			need(m, pos, tl, "链接目标")
			files[i].link = string(m[pos : pos+tl])
			pos += tl
		}
	}
	return chunks, files, sh, rh
}

// ---------- pack ----------

// 收集输入的文件/目录/符号链接(目录也收集 -> 空目录不会丢失)。
// 符号链接单独归类: 用 Lstat 而非 Stat, 保证"链接本身"被记录, 而不是跟随它去读目标内容。
// 旧实现把链接当普通文件读: 指向目录的链接会让 Read 返回 EISDIR 直接打包失败;
// 指向文件的链接则把目标内容复制一份存进归档, 解包后变成普通文件 —— 语义错误且浪费空间。

func collectPaths(inputs []string) (files []string, dirs []string, links []string) {
	for _, p := range inputs {
		fi, err := os.Lstat(p)
		if err != nil {
			fatal("stat %s: %v", p, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, p)
			continue
		}
		if fi.IsDir() {
			filepath.Walk(p, func(q string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if info.Mode()&os.ModeSymlink != 0 {
					links = append(links, q)
				} else if info.IsDir() {
					dirs = append(dirs, q)
				} else {
					files = append(files, q)
				}
				return nil
			})
		} else {
			files = append(files, p)
		}
	}
	return
}

func commonRoot(paths []string) string {
	if len(paths) == 0 {
		return "."
	}
	sep := string(os.PathSeparator)
	rc := strings.Split(filepath.Clean(paths[0]), sep)
	if len(rc) > 1 {
		rc = rc[:len(rc)-1]
	} else {
		rc = []string{"."}
	}
	for _, p := range paths[1:] {
		pc := strings.Split(filepath.Clean(p), sep)
		if len(pc) > 1 {
			pc = pc[:len(pc)-1]
		} else {
			pc = []string{"."}
		}
		i := 0
		for i < len(rc) && i < len(pc) && rc[i] == pc[i] {
			i++
		}
		rc = rc[:i]
	}
	if len(rc) == 0 {
		return "."
	}
	// 绝对路径的首个分量是空串; filepath.Join 会丢掉它 -> 必须补回根分隔符,
	// 否则后续 filepath.Rel(相对根, 绝对文件) 会失败并退化成 basename(路径被拍平)。
	abs := rc[0] == ""
	out := filepath.Join(rc...)
	if abs {
		out = sep + out
	}
	return out
}

func storedName(fp, root string) string {
	rel, err := filepath.Rel(root, fp)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return filepath.Base(fp)
	}
	return rel
}

// 流式计算整条数据的 16 字节校验哈希(sha256 前 16B): 固实流/原样区落临时文件后,
// 不再把全量数据读进内存即可完成流级校验(替代原先的 hash16(全量切片))。
func hashReader(r io.Reader) [16]byte {
	h := sha256.New()
	io.Copy(h, r)
	var out [16]byte
	copy(out[:], h.Sum(nil)[:16])
	return out
}

func pack(inputs []string, outPath, mode string) {
	spec, ok := modes[mode]
	if !ok {
		fatal("未知模式 %s", mode)
	}
	files, dirs, links := collectPaths(inputs)
	if len(files) == 0 && len(dirs) == 0 && len(links) == 0 {
		fatal("没有可打包的文件")
	}
	// 归档根 = **输入参数**的公共父目录(与 tar/zip 一致): pack out.hcax dir -> 存 dir/a/b/c。
	// 旧实现用 commonRoot(**文件**) —— 当所有文件都在子目录里时(如 src/main/go/*.go),
	// 公共根会被推到 src/main/go, 该层之上的目录全部退化成 basename:
	// src/main/go/main.go 存成 main.go, 且 go/ 这一层彻底消失 —— 目录结构被拍平(数据损坏)。
	root := commonRoot(inputs)
	be := newBackend(spec)

	chunkMap := map[[16]byte]*chunkMeta{}
	var chunkMetas []*chunkMeta
	var fileEntries []fileEntry
	var totalUncomp uint64

	// v8e: 固实流/原样区落临时文件 —— 块变换后立即写入文件并丢弃, 内存里同一份数据只"流式经过",
	// 不再整条常驻(此前整条固实流 + 原样区 = 全量输入常驻内存, 是多样大文件打包内存 ~5×输入 的根因)。
	// 压缩时从临时文件流式读取(见 compressZstdStream / xzCompress / compressMT), 内存降到"单块 + 编码器状态"。
	csf, err := os.CreateTemp("", "hcax-solid-")
	if err != nil {
		fatal("临时文件: %v", err)
	}
	rsf, err := os.CreateTemp("", "hcax-raw-")
	if err != nil {
		fatal("临时文件: %v", err)
	}
	// 清理交给 addCleanup: defer 在 fatal(os.Exit) 下不会执行, 会泄漏临时文件
	addCleanup(func() {
		csf.Close()
		rsf.Close()
		os.Remove(csf.Name())
		os.Remove(rsf.Name())
	})
	var trainSamples [][]byte
	var trainBytes int
	var compSolidSize, rawRegionSize int64
	nStored, nComp := 0, 0

	// 先统计总字节数, 用于显示进度。打包数百 MB 的语料要跑几十秒,
	// 全程没有任何反馈会让人误以为卡死 —— 进度只写 stderr, 不污染 stdout 的机器可读输出。
	var totalBytes uint64
	for _, fp := range files {
		if fi, e := os.Lstat(fp); e == nil {
			totalBytes += uint64(fi.Size())
		}
	}
	showProgress := totalBytes >= 32<<20
	var lastReport uint64

	blk := make([]byte, 1<<20)
	for fi, fp := range files {
		if showProgress && totalUncomp-lastReport >= 16<<20 {
			lastReport = totalUncomp
			fmt.Fprintf(os.Stderr, "\r  分块中 %d/%d 文件, %.0f%%   ",
				fi+1, len(files), 100.0*float64(totalUncomp)/float64(totalBytes))
		}
		f, err := os.Open(fp)
		if err != nil {
			fatal("open %s: %v", fp, err)
		}
		rel := storedName(fp, root)
		st, _ := f.Stat()
		var curChunks []uint32
		var xfProbe byte    // zstd 档文件级探测的变换类型; 在闭包 onChunk 之前声明以便捕获
		rasterMode := false // 光栅图: 已做文件级二维预测/色彩去相关, 块级不再叠加变换
		var fileXform byte  // 文件级变换(v7): 记入文件表, 解包时据此逆变换

		// solid(ultra): 整文件固实(关 CDC 去重) -> 跨文件冗余交给 lzma2; 否则 CDC 去重。
		var ch *chunker
		if spec.solid {
			ch = newChunkerMinMax(nil, 4<<20, 256<<20)
		} else {
			ch = newChunker(nil)
		}
		ch.onChunk = func(cb []byte) {
			hh := hash16(cb)
			// v8: 去重先于变换 —— 重复块直接引用首次出现的块, 跳过昂贵的预处理(否则大文件内存/GC 爆炸)
			if !spec.solid {
				if cm, exists := chunkMap[hh]; exists {
					curChunks = append(curChunks, cm.idx)
					return
				}
			}
			var xf byte
			var tb []byte
			switch {
			case rasterMode:
				xf, tb = xfNone, cb // 光栅已在文件级做过二维预测
			case be.spec.backend == "zstd":
				// best(zstd): 文件级探测一次变换(便宜保速度)
				xf, tb = xfProbe, applyXform(xfProbe, cb, false)
			default:
				xf, tb = be.chooseTransform(cb)
			}
			// v8d: 直接写入固实流/原样区, 块数据不再经 cm.data 中转(避免"cm.data + compSolid"双份常驻)。
			// xfNone 时 tb==cb(applyXform/chooseTransform 对 xfNone 返回原块, 不再强制拷贝),
			// bytes.Buffer.Write 会同步拷走, 池化缓冲被复用也无碍; 仅 zstd 训练样本按需复制(封顶 64MiB)。
			if spec.backend == "zstd" && len(tb) >= 200 && len(tb) <= 128<<10 &&
				len(trainSamples) < 2000 && trainBytes < 64<<20 {
				s := append([]byte(nil), tb...)
				trainSamples = append(trainSamples, s)
				trainBytes += len(s)
			}
			cm := &chunkMeta{hash: hh, uncomp: uint32(len(cb)), xform: xf, idx: uint32(len(chunkMetas))}
			if be.shouldStore(tb) {
				cm.stored = true
				cm.offset = uint64(rawRegionSize)
				n, e := rsf.Write(tb)
				if e != nil {
					fatal("写原样区: %v", e)
				}
				rawRegionSize += int64(n)
				nStored++
			} else {
				cm.stored = false
				cm.offset = uint64(compSolidSize)
				n, e := csf.Write(tb)
				if e != nil {
					fatal("写固实流: %v", e)
				}
				compSolidSize += int64(n)
				nComp++
			}
			if !spec.solid {
				chunkMap[hh] = cm
			}
			chunkMetas = append(chunkMetas, cm)
			curChunks = append(curChunks, cm.idx)
		}

		// 先探前 64 字节: 判断是否光栅图(BMP/TGA/PNM) / zstd 档文件级变换探测
		head := make([]byte, 64)
		np, _ := io.ReadFull(f, head)
		if rasterLikely(head[:np]) && st.Size() <= rasterMaxBytes {
			rest, _ := io.ReadAll(f)
			whole := append(append([]byte{}, head[:np]...), rest...)
			// 光栅变换跨块生效(预测依赖整幅几何), 故在分块之前对"整文件"施加;
			// 用门控比较候选变换, 只采用确实更小的 -> 绝不退步
			rxf := be.chooseRasterXform(whole)
			if rxf != xfNone && rasterApply(rxf, whole, true) {
				rasterMode = true // 块级不再叠加变换
				fileXform = rxf
				for off := 0; off < len(whole); off += len(blk) {
					end := off + len(blk)
					if end > len(whole) {
						end = len(whole)
					}
					ch.write(whole[off:end])
				}
			} else {
				if be.spec.backend == "zstd" {
					xfProbe = detectXform(head[:np])
				}
				ch.write(head[:np])
				ch.write(rest)
			}
			ch.flush()
			f.Close()
			fileEntries = append(fileEntries, fileEntry{
				name: rel, size: uint64(st.Size()), mtime: uint64(st.ModTime().Unix()),
				mode: uint32(st.Mode().Perm()), chunks: curChunks, xform: fileXform,
			})
			totalUncomp += uint64(st.Size())
			continue
		}
		if np > 0 {
			if be.spec.backend == "zstd" {
				xfProbe = detectXform(head[:np])
			}
			ch.write(head[:np])
		}
		for {
			n, re := f.Read(blk)
			if n > 0 {
				ch.write(blk[:n])
			}
			if re == io.EOF {
				break
			}
			if re != nil {
				fatal("read %s: %v", fp, re)
			}
		}
		ch.flush()
		f.Close()

		fileEntries = append(fileEntries, fileEntry{
			name: rel, size: uint64(st.Size()), mtime: uint64(st.ModTime().Unix()),
			mode: uint32(st.Mode().Perm()), chunks: curChunks,
		})
		totalUncomp += uint64(st.Size())
	}
	if showProgress {
		fmt.Fprintf(os.Stderr, "\r  分块中 %d/%d 文件, 100%%   \n", len(files), len(files))
	}
	if os.Getenv("HCAX_T") != "" {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(os.Stderr, "[t] after-chunking GoAlloc=%dMB Sys=%dMB nChunks=%d\n", ms.Alloc/1048576, ms.Sys/1048576, len(chunkMetas))
	}

	// 目录条目(含空目录): 不产生数据块, 只记录名字/权限/时间, 保证解包后目录结构完整
	for _, d := range dirs {
		rel := storedName(d, root)
		if rel == "." || rel == "" {
			continue
		}
		di, err := os.Stat(d)
		if err != nil {
			continue
		}
		fileEntries = append(fileEntries, fileEntry{
			name: rel, size: 0, mtime: uint64(di.ModTime().Unix()),
			mode: uint32(di.Mode().Perm()), isDir: true,
		})
	}

	// 符号链接(v9): 只存链接目标字符串, 不读内容、不占数据块。
	// size 记为链接目标的长度(与 tar 一致, 仅作信息展示, 不代表归档里的数据量)。
	for _, lp := range links {
		tgt, err := os.Readlink(lp)
		if err != nil {
			fatal("读取符号链接 %s: %v", lp, err)
		}
		li, err := os.Lstat(lp)
		if err != nil {
			fatal("stat %s: %v", lp, err)
		}
		fileEntries = append(fileEntries, fileEntry{
			name: storedName(lp, root), size: uint64(len(tgt)),
			mtime: uint64(li.ModTime().Unix()), mode: uint32(li.Mode().Perm()),
			isLink: true, link: tgt,
		})
	}

	// v8d: 分桶已在 onChunk 内直接完成(块写入 compSolid/rawRegion 并即时释放), 此处不再有 cm.data 中转。
	// 故内存里可压/原样数据各只存一份(compSolid / rawRegion), 大文件打包峰值内存从 ~5×输入 降到 ~1.5×输入。

	// lzma2 dict 按可压数据量收敛; lc/pb 自适应在压缩阶段进行(单流用全流试选, 并行用采样试选)
	be.dictParam = dictFor(int(compSolidSize))
	if os.Getenv("HCAX_T") != "" {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(os.Stderr, "[t] bucket done nComp=%d nStored=%d compSolid=%d raw=%d | GoAlloc=%dMB Sys=%dMB\n",
			nComp, nStored, compSolidSize, rawRegionSize, ms.Alloc/1048576, ms.Sys/1048576)
	}

	// zstd 训练字典(best 相似文件): 用上面复制的独立样本训练字典, 用字典压缩该组
	var trainDict []byte
	if spec.backend == "zstd" && len(trainSamples) >= 4 {
		// BuildDict: Contents=样本(建码表), History=代表样本(作字典正文, >=8B)
		var hist []byte
		for _, s := range trainSamples {
			hist = append(hist, s...)
			if len(hist) >= 100<<10 {
				break
			}
		}
		if len(hist) < 8 {
			hist = trainSamples[0]
		}
		if d, e := zstd.BuildDict(zstd.BuildDictOptions{
			ID:       1,
			Contents: trainSamples,
			History:  hist,
			Offsets:  [3]int{1, 4, 8},
			Level:    zstd.EncoderLevelFromZstd(spec.level),
		}); e == nil && len(d) > 0 {
			trainDict = d
			opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(spec.level)), zstd.WithEncoderDict(trainDict)}
			if spec.window > 0 {
				wl := spec.window
				if wl > zstdMaxWindowLog {
					wl = zstdMaxWindowLog
				}
				opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
			}
			if enc, e2 := zstd.NewWriter(io.Discard, opts...); e2 == nil {
				be.zEncDict = enc
				be.hasDict = true
			}
		}
	}
	be.trainDict = trainDict // v8e: 流式压缩时复用该字典建输出编码器
	trainSamples = nil       // v8d: 字典已建, 释放样本

	// 压缩可压固实流:
	// - zstd 档(fast/best): 从临时文件流式压缩(compressZstdStream), 内存只留单块+编码器;
	// - lzma2 且可压数据 >= mmtMinBytes: 并行分组(mmt)提速, 各组从临时文件读各自切片;
	// - 其余(中小 lzma2): 单流全流选参, 把固实流读回内存(<64MiB, 安全)。
	var frame []byte
	if be.spec.backend == "zstd" {
		if compSolidSize > 0 {
			if _, e := csf.Seek(0, io.SeekStart); e != nil {
				fatal("seek 固实流: %v", e)
			}
			t0 := time.Now()
			frame = be.compressZstdStream(csf)
			if os.Getenv("HCAX_T") != "" {
				fmt.Fprintf(os.Stderr, "[t] compressZstdStream %v\n", time.Since(t0))
			}
		}
	} else if be.spec.mmt && nComp > 1 && compSolidSize >= mmtMinBytes {
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] -> mmt path G=%d\n", runtime.NumCPU())
		}
		t0 := time.Now()
		be.tuneLZMA2(csf, compSolidSize) // 大语料: 采样试选(全流试选太贵), 再分组并行压
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] tuneLZMA2 %v\n", time.Since(t0))
		}
		t0 = time.Now()
		frame = be.compressMT(chunkMetas, csf, compSolidSize)
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] compressMT %v\n", time.Since(t0))
		}
	} else {
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] -> single-stream path\n")
		}
		t0 := time.Now()
		var solidData []byte
		if compSolidSize > 0 {
			csf.Seek(0, io.SeekStart)
			solidData, err = io.ReadAll(csf)
			if err != nil {
				fatal("读固实流: %v", err)
			}
		}
		frame = be.compressLZMA2Best(solidData) // 中小: 全流试 pb=2/4, 选优并复用结果
		if os.Getenv("HCAX_T") != "" {
			fmt.Fprintf(os.Stderr, "[t] compressLZMA2Best %v\n", time.Since(t0))
		}
	}

	out, err := os.Create(outPath)
	if err != nil {
		fatal("create %s: %v", outPath, err)
	}
	defer out.Close()

	// 元数据(块表+文件表) 序列化后一并压缩(v6): 明文元数据往往占归档一半以上, 压缩收益极大
	// 流级校验哈希(替代逐块哈希: 逐块哈希是随机数, 不可压缩, 大量小文件时开销极大)
	var solidHash, rawHash [8]byte
	if compSolidSize > 0 {
		csf.Seek(0, io.SeekStart)
		h := hashReader(csf)
		copy(solidHash[:], h[:8])
	}
	if rawRegionSize > 0 {
		rsf.Seek(0, io.SeekStart)
		h := hashReader(rsf)
		copy(rawHash[:], h[:8])
	}
	metaRaw := serializeMeta(chunkMetas, fileEntries, solidHash, rawHash)
	metaFrame := be.compress(metaRaw)

	// 训练字典区(原始存放: 字典本身接近不可压)
	var dictBuf bytes.Buffer
	if len(trainDict) > 0 {
		binary.Write(&dictBuf, binary.LittleEndian, uint32(1))
		binary.Write(&dictBuf, binary.LittleEndian, uint32(len(trainDict)))
		dictBuf.Write(trainDict)
	} else {
		binary.Write(&dictBuf, binary.LittleEndian, uint32(0))
	}

	// 只在含符号链接时才升到 v9: 普通归档仍写 v8, 旧版二进制照样能解开
	wv := byte(verCompat)
	for i := range fileEntries {
		if fileEntries[i].isLink {
			wv = version
			break
		}
	}

	// header(v6, 48B): magic ver code window flag
	//                | metaCompLen metaRawLen compLen rawLen | nChunks nFiles
	out.WriteString(magic)
	out.Write([]byte{wv, spec.code, byte(spec.window), 0})
	binary.Write(out, binary.LittleEndian, uint64(len(metaFrame)))
	binary.Write(out, binary.LittleEndian, uint64(len(metaRaw)))
	binary.Write(out, binary.LittleEndian, uint64(len(frame)))
	binary.Write(out, binary.LittleEndian, uint64(rawRegionSize))
	binary.Write(out, binary.LittleEndian, uint32(len(chunkMetas)))
	binary.Write(out, binary.LittleEndian, uint32(len(fileEntries)))
	// 布局: metaFrame | dataFrame | rawRegion | dictRegion
	out.Write(metaFrame)
	out.Write(frame)
	if rawRegionSize > 0 {
		rsf.Seek(0, io.SeekStart)
		io.Copy(out, rsf)
	}
	out.Write(dictBuf.Bytes())
	// trailer
	out.WriteString(trailerMag)
	binary.Write(out, binary.LittleEndian, totalUncomp)
	binary.Write(out, binary.LittleEndian, uint32(len(fileEntries)))
	binary.Write(out, binary.LittleEndian, uint32(len(chunkMetas)))

	sz, _ := out.Stat()
	raw := totalUncomp
	runCleanups() // 成功路径也要删临时文件(清理钩子本身幂等, 重复执行无副作用)
	fmt.Printf("打包完成: %s  原始 %d B -> %d B  压率 %.2f%%  模式=%s  唯一块=%d(可压%d/原样%d)\n",
		outPath, raw, sz.Size(), 100.0*float64(sz.Size())/float64(raw), mode, len(chunkMetas), nComp, nStored)
}

// ---------- read archive ----------

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

// 尾部魔数校验: trailer 布局(所有版本一致) = "XACH" + totalUncomp(8) + nFiles(4) + nChunks(4) = 20B。
// 老实现只写不读 —— 归档被截断(传输中断/磁盘满/拷一半)时, 会一路走到解压阶段才报出
// "unknown dictionary" 之类莫名其妙的错误, 甚至可能静默解出一部分数据让人误以为成功。
// 这里在**读数据区之前**就核对文件长度与尾部魔数, 截断当场给出明确提示。

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

// 头部字段合理性: 损坏/恶意归档的长度字段可以是任意 64 位值, 直接拿去 make([]byte, N)
// 会瞬间 OOM 或分配出荒谬的大小; 条目数过大也会让解析失控。在读数据之前先挡掉。

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

// 惰性解压固实流到至少 need 字节为止。
// 首次调用时才建临时文件与解压器; 后续若已解压够长则直接返回(零成本)。
// 关键收益: (1) list/verify 这类不需要数据的命令不再解压; (2) extract 只抽归档前部的
// 文件时, 解压到其块末尾即可停止, 不必解完整条流; (3) 解包峰值内存有界(数据在临时文件)。

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
	target := joinOut(outDir, fe.name)
	// 目录条目(含空目录): 直接建目录
	if fe.isDir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			fatal("mkdir: %v", err)
		}
		os.Chmod(target, os.FileMode(fe.mode))
		return
	}
	// 符号链接(v9): 重建链接本身, 不写内容。
	// 注: Go 标准库没有 lutimes, 链接自身的 mtime 无法还原(只还原普通文件/目录的)。
	if fe.isLink {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			fatal("mkdir: %v", err)
		}
		os.Remove(target) // 覆盖同名已存在的文件/链接
		if err := os.Symlink(fe.link, target); err != nil {
			fatal("symlink %s: %v", fe.name, err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fatal("mkdir: %v", err)
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
	defer out.Close()
	// 文件级光栅变换(v7): 变换跨块生效, 必须先拼出整文件再逆变换
	if len(fe.chunks) > 0 && (fe.xform == xfRasterMed || fe.xform == xfRasterRctMed) {
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
		out.Write(buf)
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, time.Unix(int64(fe.mtime), 0), time.Unix(int64(fe.mtime), 0))
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
			out.Write(buf)
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, time.Unix(int64(fe.mtime), 0), time.Unix(int64(fe.mtime), 0))
			return
		}
		out.Write(first)
		if uint64(len(first)) == fe.size {
			os.Chmod(target, os.FileMode(fe.mode))
			os.Chtimes(target, time.Unix(int64(fe.mtime), 0), time.Unix(int64(fe.mtime), 0))
			return
		}
		var written2 uint64 = uint64(len(first))
		for _, ix := range fe.chunks[1:] {
			cb := a.readChunk(ix, verify)
			out.Write(cb)
			written2 += uint64(len(cb))
		}
		if written2 != fe.size {
			fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, written2)
		}
		os.Chmod(target, os.FileMode(fe.mode))
		os.Chtimes(target, time.Unix(int64(fe.mtime), 0), time.Unix(int64(fe.mtime), 0))
		return
	}
	var written uint64
	if written != fe.size {
		fatal("文件 %s 大小不符: 期望 %d 实际 %d", fe.name, fe.size, written)
	}
	os.Chmod(target, os.FileMode(fe.mode))
	os.Chtimes(target, time.Unix(int64(fe.mtime), 0), time.Unix(int64(fe.mtime), 0))
}

func unpack(archivePath, outDir string, verify bool, only []string) {
	a := openArchive(archivePath)
	if verify {
		a.verifyStream() // v6 流级校验(旧版为空操作, 由 readChunk 逐块校验)
	}
	os.MkdirAll(outDir, 0o755)
	if len(only) == 0 {
		// 全量解包: 顺序读取, 固实流按需推进即可
		for _, fe := range a.files {
			a.extractFile(fe, outDir, verify)
		}
		runCleanups()
		fmt.Printf("解包完成: %d 文件 -> %s  模式=%s 校验=%v\n", len(a.files), outDir, modeName(a.spec.code), verify)
		return
	}
	set := map[string]bool{}
	for _, o := range only {
		set[o] = true
	}
	var want []fileEntry
	for _, fe := range a.files {
		if set[fe.name] || set[filepath.Base(fe.name)] {
			want = append(want, fe)
		}
	}
	// 只解到目标文件的块末尾为止: 抽归档前部的小文件不必解压整条固实流
	a.prepareFor(want)
	for _, fe := range want {
		a.extractFile(fe, outDir, verify)
	}
	runCleanups()
	fmt.Printf("抽取完成: %d 文件 -> %s  模式=%s 校验=%v\n", len(want), outDir, modeName(a.spec.code), verify)
}

func modeName(code byte) string {
	for k, m := range modes {
		if m.code == code {
			return k
		}
	}
	return "?"
}

func listArchive(archivePath string) {
	a := openArchive(archivePath)
	var tot uint64
	for _, fe := range a.files {
		switch {
		case fe.isDir:
			fmt.Printf("  %12s  %s/\n", "<dir>", fe.name)
		case fe.isLink:
			fmt.Printf("  %12s  %s -> %s\n", "<link>", fe.name, fe.link)
		default:
			fmt.Printf("  %12d  %s\n", fe.size, fe.name)
		}
		tot += fe.size
	}
	fmt.Printf("共 %d 文件, 原大小 %d B, 唯一块 %d\n", len(a.files), tot, len(a.chunks))
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
	a := openArchive(archivePath)
	var tot uint64
	for i := range a.chunks {
		tot += uint64(a.chunks[i].uncomp)
	}
	if a.hashLen == 0 {
		a.verifyStream()
		fmt.Printf("校验通过: 固实流/原样区哈希一致, %d 块, 解压数据 %d B\n", len(a.chunks), tot)
		return
	}
	for i := range a.chunks {
		_ = a.readChunk(uint32(i), true)
	}
	fmt.Printf("校验通过: %d 块全部哈希一致, 解压数据 %d B\n", len(a.chunks), tot)
}

// ---------- 清理钩子 ----------
// fatal() 用 os.Exit(1) 终止进程, 而 Go 的 defer 在 os.Exit 时**不会执行**。
// 自 v8e 起打包会把固实流/原样区落到临时文件(可达数百 MB), 一旦走到任何 fatal 分支
// (读文件失败、磁盘满、格式损坏...) 这些临时文件就会永久留在 TMPDIR。
// 这里改用显式注册的清理钩子: 正常结束与 fatal 两条路径都会执行。

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

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	runCleanups()
	os.Exit(1)
}

// ---------- 归档内文件名校验 ----------
// 老实现是 strings.Contains(name, "..") 一刀切: 会把 "a..b.txt"、"..hidden" 这类
// 完全合法的文件名误判为路径穿越攻击。后果是"能打包、解不开"——数据被自己锁死。
// 正确做法是逐段判断: 任何一段恰好等于 ".." 才算逃逸; 同时拒绝绝对路径。
// 反斜杠一并检查, 防止 Windows 风格路径名在 Unix 上被当成普通字符放过。

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
