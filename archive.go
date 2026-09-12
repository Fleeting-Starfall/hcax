package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
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
		b.WriteByte(f)
		binary.Write(&b, binary.LittleEndian, fe.size)
		binary.Write(&b, binary.LittleEndian, uint32(fe.mtime))
		binary.Write(&b, binary.LittleEndian, uint16(fe.mode))
		binary.Write(&b, binary.LittleEndian, uint32(len(fe.chunks)))
		for _, ix := range fe.chunks {
			binary.Write(&b, binary.LittleEndian, ix)
		}
	}
	return b.Bytes()
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
		nl := int(binary.LittleEndian.Uint16(m[pos : pos+2]))
		pos += 2
		files[i].name = string(m[pos : pos+nl])
		pos += nl
		f := m[pos]
		pos++
		files[i].isDir = f&1 != 0
		if ver >= 7 {
			files[i].xform = (f >> 1) & 0x0f
		}
		files[i].size = binary.LittleEndian.Uint64(m[pos : pos+8])
		pos += 8
		files[i].mtime = uint64(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		files[i].mode = uint32(binary.LittleEndian.Uint16(m[pos : pos+2]))
		pos += 2
		nc := int(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		idxs := make([]uint32, nc)
		for j := range idxs {
			idxs[j] = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
		}
		files[i].chunks = idxs
	}
	return chunks, files, sh, rh
}

// ---------- pack ----------

// 收集输入的文件与目录(目录也收集 -> 空目录不会丢失)
func collectPaths(inputs []string) (files []string, dirs []string) {
	for _, p := range inputs {
		fi, err := os.Stat(p)
		if err != nil {
			fatal("stat %s: %v", p, err)
		}
		if fi.IsDir() {
			filepath.Walk(p, func(q string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if info.IsDir() {
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

func pack(inputs []string, outPath, mode string) {
	spec, ok := modes[mode]
	if !ok {
		fatal("未知模式 %s", mode)
	}
	files, dirs := collectPaths(inputs)
	if len(files) == 0 && len(dirs) == 0 {
		fatal("没有可打包的文件")
	}
	root := commonRoot(files)
	be := newBackend(spec)

	chunkMap := map[[16]byte]*chunkMeta{}
	var chunkMetas []*chunkMeta
	var fileEntries []fileEntry
	var totalUncomp uint64

	blk := make([]byte, 1<<20)
	for _, fp := range files {
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
			// v8: xfNone 时 applyXform 返回原块(cb); 配合块缓冲池需复制一份, 避免池化缓冲被 cm.data 持有后被复用而损坏数据
			if xf == xfNone {
				tb = append([]byte(nil), cb...)
			}
			cm := &chunkMeta{hash: hh, uncomp: uint32(len(cb)), xform: xf, data: tb, idx: uint32(len(chunkMetas))}
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

	// 分桶: 可压块进 compSolid(固实压缩保压率), 不可压块进 rawRegion(原样存储, 省编码开销)
	var compSolid, rawRegion bytes.Buffer
	nStored, nComp := 0, 0
	for _, cm := range chunkMetas {
		if be.shouldStore(cm.data) {
			cm.stored = true
			cm.offset = uint64(rawRegion.Len())
			rawRegion.Write(cm.data)
			nStored++
		} else {
			cm.stored = false
			cm.offset = uint64(compSolid.Len())
			compSolid.Write(cm.data)
			nComp++
		}
	}

	// lzma2 dict 按可压数据量收敛; lc/pb 自适应在压缩阶段进行(单流用全流试选, 并行用采样试选)
	be.dictParam = dictFor(compSolid.Len())

	// zstd 训练字典(best 相似文件): 收集可压块样本训练一个字典, 用字典压缩该组
	var trainDict []byte
	if spec.backend == "zstd" {
		var samples [][]byte
		var samplesBytes int
		for _, c := range chunkMetas {
			if len(c.data) >= 200 && len(c.data) <= 128<<10 {
				samples = append(samples, c.data)
				samplesBytes += len(c.data)
				// 封顶样本总量(64MiB): zstd 字典训练对样本量不敏感, 多于此只是徒增内存峰值;
				// 对"非重复多文件"场景, 原上限 2000×128KiB=256MiB 会在训练期间与 cm.data 双份常驻
				if len(samples) >= 2000 || samplesBytes >= 64<<20 {
					break
				}
			}
		}
		if len(samples) >= 4 {
			// BuildDict: Contents=样本(建码表), History=代表样本(作字典正文, >=8B)
			var hist []byte
			for _, s := range samples {
				hist = append(hist, s...)
				if len(hist) >= 100<<10 {
					break
				}
			}
			if len(hist) < 8 {
				hist = samples[0]
			}
			if d, e := zstd.BuildDict(zstd.BuildDictOptions{
				ID:       1,
				Contents: samples,
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
	}

	// v8: 打包阶段 cm.data 仅用于分桶/训练字典; 此后压缩与解包都读 compSolid/rawRegion,
	// 不再需要 cm.data. 释放它, 避免"块数据被存两份"(尤其 ultra/随机不可压大数据)吃内存.
	for _, cm := range chunkMetas {
		cm.data = nil
	}

	// 压缩可压固实流: 仅当可压数据 >= mmtMinBytes 才并行分组(mmt)提速;
	// 中小语料走单线程整流保压率(并行切段会丢段间冗余)
	var frame []byte
	if be.spec.mmt && nComp > 1 && compSolid.Len() >= mmtMinBytes {
		be.tuneLZMA2(compSolid.Bytes()) // 大语料: 采样试选(全流试选太贵), 再分组并行压
		frame = be.compressMT(chunkMetas, compSolid.Bytes())
	} else {
		frame = be.compressLZMA2Best(compSolid.Bytes()) // 中小: 全流试 pb=2/4, 选优并复用结果
	}

	out, err := os.Create(outPath)
	if err != nil {
		fatal("create %s: %v", outPath, err)
	}
	defer out.Close()

	// 元数据(块表+文件表) 序列化后一并压缩(v6): 明文元数据往往占归档一半以上, 压缩收益极大
	// 流级校验哈希(替代逐块哈希: 逐块哈希是随机数, 不可压缩, 大量小文件时开销极大)
	var solidHash, rawHash [8]byte
	if compSolid.Len() > 0 {
		h := hash16(compSolid.Bytes())
		copy(solidHash[:], h[:8])
	}
	if rawRegion.Len() > 0 {
		h := hash16(rawRegion.Bytes())
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

	// header(v6, 48B): magic ver code window flag
	//                | metaCompLen metaRawLen compLen rawLen | nChunks nFiles
	out.WriteString(magic)
	out.Write([]byte{version, spec.code, byte(spec.window), 0})
	binary.Write(out, binary.LittleEndian, uint64(len(metaFrame)))
	binary.Write(out, binary.LittleEndian, uint64(len(metaRaw)))
	binary.Write(out, binary.LittleEndian, uint64(len(frame)))
	binary.Write(out, binary.LittleEndian, uint64(rawRegion.Len()))
	binary.Write(out, binary.LittleEndian, uint32(len(chunkMetas)))
	binary.Write(out, binary.LittleEndian, uint32(len(fileEntries)))
	// 布局: metaFrame | dataFrame | rawRegion | dictRegion
	out.Write(metaFrame)
	out.Write(frame)
	out.Write(rawRegion.Bytes())
	out.Write(dictBuf.Bytes())
	// trailer
	out.WriteString(trailerMag)
	binary.Write(out, binary.LittleEndian, totalUncomp)
	binary.Write(out, binary.LittleEndian, uint32(len(fileEntries)))
	binary.Write(out, binary.LittleEndian, uint32(len(chunkMetas)))

	sz, _ := out.Stat()
	raw := totalUncomp
	fmt.Printf("打包完成: %s  原始 %d B -> %d B  压率 %.2f%%  模式=%s  唯一块=%d(可压%d/原样%d)\n",
		outPath, raw, sz.Size(), 100.0*float64(sz.Size())/float64(raw), mode, len(chunkMetas), nComp, nStored)
}

// ---------- read archive ----------

func openArchive(path string) *archive {
	f, err := os.Open(path)
	if err != nil {
		fatal("open %s: %v", path, err)
	}
	defer f.Close()
	hdr := make([]byte, 48)
	if _, err := io.ReadFull(f, hdr[:8]); err != nil {
		fatal("读头失败: %v", err)
	}
	if string(hdr[0:4]) != magic {
		fatal("不是 HCAX 文件")
	}
	ver := hdr[4]
	if ver < 2 || ver > 8 {
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
		// 3) 数据区
		f.Seek(dataOff, io.SeekStart)
		var compSolid []byte
		if compLen > 0 {
			fr := make([]byte, compLen)
			if _, err := io.ReadFull(f, fr); err != nil {
				fatal("读数据区失败: %v", err)
			}
			compSolid = be.decompress(fr)
		}
		rawRegion := make([]byte, rawLen)
		if rawLen > 0 {
			if _, err := io.ReadFull(f, rawRegion); err != nil {
				fatal("读原样区失败: %v", err)
			}
		}
		return &archive{spec: spec, be: be, dataStart: dataOff, compSolid: compSolid,
			rawRegion: rawRegion, chunks: chunks, files: files, hashLen: 0,
			solidHash: sh, rawHash: rh, ver: ver}
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
			}
		}
	}

	// 现在回读数据区并解压(字典已就绪)
	var compSolid []byte
	f.Seek(dataStart, io.SeekStart)
	if compLen > 0 {
		frame := make([]byte, compLen)
		if _, err := io.ReadFull(f, frame); err != nil {
			fatal("读数据区失败: %v", err)
		}
		compSolid = be.decompress(frame)
	}
	rawRegion := make([]byte, rawLen)
	if rawLen > 0 {
		if _, err := io.ReadFull(f, rawRegion); err != nil {
			fatal("读原样区失败: %v", err)
		}
	}
	return &archive{spec: spec, be: be, dataStart: dataStart, compSolid: compSolid, rawRegion: rawRegion, chunks: chunks, files: files, hashLen: 16, ver: ver}
}

func (a *archive) readChunk(idx uint32, verify bool) []byte {
	cm := a.chunks[idx]
	var out []byte
	if cm.stored {
		out = a.rawRegion[cm.offset : cm.offset+uint64(cm.uncomp)]
	} else {
		out = a.compSolid[cm.offset : cm.offset+uint64(cm.uncomp)]
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
	if len(a.compSolid) > 0 {
		h := hash16(a.compSolid)
		if !bytes.Equal(h[:8], a.solidHash[:]) {
			fatal("固实流校验失败(数据损坏)")
		}
	}
	if len(a.rawRegion) > 0 {
		h := hash16(a.rawRegion)
		if !bytes.Equal(h[:8], a.rawHash[:]) {
			fatal("原样区校验失败(数据损坏)")
		}
	}
}

func (a *archive) extractFile(fe fileEntry, outDir string, verify bool) {
	target := filepath.Join(outDir, fe.name)
	if strings.HasPrefix(fe.name, "/") || strings.Contains(fe.name, "..") {
		fatal("非法路径: %s", fe.name)
	}
	// 目录条目(含空目录): 直接建目录
	if fe.isDir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			fatal("mkdir: %v", err)
		}
		os.Chmod(target, os.FileMode(fe.mode))
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fatal("mkdir: %v", err)
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
		for _, fe := range a.files {
			a.extractFile(fe, outDir, verify)
		}
		fmt.Printf("解包完成: %d 文件 -> %s  模式=%s 校验=%v\n", len(a.files), outDir, modeName(a.spec.code), verify)
		return
	}
	set := map[string]bool{}
	for _, o := range only {
		set[o] = true
	}
	n := 0
	for _, fe := range a.files {
		if set[fe.name] || set[filepath.Base(fe.name)] {
			a.extractFile(fe, outDir, verify)
			n++
		}
	}
	fmt.Printf("抽取完成: %d 文件 -> %s  模式=%s 校验=%v\n", n, outDir, modeName(a.spec.code), verify)
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
		fmt.Printf("  %12d  %s\n", fe.size, fe.name)
		tot += fe.size
	}
	fmt.Printf("共 %d 文件, 原大小 %d B, 唯一块 %d\n", len(a.files), tot, len(a.chunks))
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

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
