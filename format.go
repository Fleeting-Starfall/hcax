package main

// 容器格式的读写: 条目类型、元数据序列化/解析、版本决策。字节级布局见 FORMAT.md。

import (
	"bytes"
	"encoding/binary"
	"os"
)

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

// 文件身份: (设备号, inode) 唯一确定一个文件; nlink>1 才说明它有多个名字(硬链接)。
// 单名字文件不记 —— 否则每个文件都要白存一份键。
type fileIDKey struct {
	dev, ino, nlink uint64
}

type fileEntry struct {
	name   string
	size   uint64
	mtime  uint64 // Unix 秒(仅用于显示; v10 的精确时间在 nano 里)
	nano   int64  // v10: Unix 纳秒。0 = 该条目没有可用时间戳
	mode   uint32 // 权限位 + setuid/setgid/sticky(Go os.FileMode 的位布局)
	chunks []uint32
	isDir  bool // 目录条目(空目录也保留)
	isLink bool // v9: 符号链接条目(存链接目标, 不存文件内容)
	isHard bool // v11: 硬链接条目(复用首个条目的块索引, 不再读一遍内容)
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
func serializeMeta(chunks []*chunkMeta, files []fileEntry, solidHash, rawHash [8]byte, ver byte) []byte {
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
		if fe.isHard {
			f |= 0x40 // v11: 硬链接
		}
		b.WriteByte(f)
		binary.Write(&b, binary.LittleEndian, fe.size)
		if ver >= 10 {
			// v10: 纳秒时间戳 + 完整模式位(含 setuid/setgid/sticky)
			binary.Write(&b, binary.LittleEndian, entryNano(fe))
			binary.Write(&b, binary.LittleEndian, fe.mode)
		} else {
			binary.Write(&b, binary.LittleEndian, uint32(fe.mtime))
			binary.Write(&b, binary.LittleEndian, uint16(fe.mode&0o777))
		}
		binary.Write(&b, binary.LittleEndian, uint32(len(fe.chunks)))
		for _, ix := range fe.chunks {
			binary.Write(&b, binary.LittleEndian, ix)
		}
		if fe.isLink || fe.isHard { // v9 链接目标 / v11 硬链接指向的归档内路径
			lb := []byte(fe.link)
			binary.Write(&b, binary.LittleEndian, uint16(len(lb)))
			b.Write(lb)
		}
	}
	return b.Bytes()
}

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
		// 单个块的声明长度必须落在**我们自己分块器**的上界之内(CDC 最大 256KB,
		// ultra 的整文件固实最大 256MB)。损坏/恶意归档可以随便填 0xFFFFFFFF,
		// 而 readChunk 是"先 make([]byte, uncomp) 再发现数据不够" —— 一个 1KB 的
		// 归档就能让人先吃掉 4GB 内存。这句把巨额分配挡在读数据之前。
		if chunks[i].uncomp > maxChunkUncomp {
			fatal("块 %d 声明长度 %d B 超出合理上界(%d B): 归档损坏",
				i, chunks[i].uncomp, maxChunkUncomp)
		}
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
		files[i].isHard = ver >= 11 && f&0x40 != 0
		files[i].size = binary.LittleEndian.Uint64(m[pos : pos+8])
		pos += 8
		if ver >= 10 {
			need(m, pos, 12, "文件表项(v10 时间/权限)")
			files[i].nano = int64(binary.LittleEndian.Uint64(m[pos : pos+8]))
			pos += 8
			files[i].mode = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
			if files[i].nano != 0 {
				files[i].mtime = uint64(files[i].nano / 1e9)
			}
		} else {
			files[i].mtime = uint64(binary.LittleEndian.Uint32(m[pos : pos+4]))
			pos += 4
			files[i].mode = uint32(binary.LittleEndian.Uint16(m[pos : pos+2]))
			pos += 2
		}
		nc := int(binary.LittleEndian.Uint32(m[pos : pos+4]))
		pos += 4
		need(m, pos, 4*nc, "文件块索引")
		// 不能拿"文件引用的块数"去和"块表条目数"比大小: 去重之后同一个块会被
		// 引用多次, 引用数**必然**可以大于唯一块数(一个 35MB 全零文件只对应
		// 2 个唯一块, 却要引用 134 次)。老检查在这里直接判"元数据损坏",
		// 于是内部重复度高的归档打得开、解不开 —— 数据被自己锁死。
		// 只对逐个索引做上界检查(下面那个循环)。
		idxs := make([]uint32, nc)
		for j := range idxs {
			idxs[j] = binary.LittleEndian.Uint32(m[pos : pos+4])
			pos += 4
			if idxs[j] >= nChunks {
				fatal("元数据损坏: 文件 %s 引用了不存在的块 %d(共 %d 块)", files[i].name, idxs[j], nChunks)
			}
		}
		files[i].chunks = idxs
		if files[i].isLink || files[i].isHard {
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

const (
	magic      = "HCAX"
	trailerMag = "XACH"

	// 兼容读 v2~v11。写的时候能不升就不升 —— 每升一版就等于跟旧版二进制绝缘:
	//   普通归档写 v8; 含符号链接写 v9; 含 setuid/setgid/sticky 或 2038 后的
	//   时间戳写 v10; 含硬链接写 v11。
	version   = 11
	verCompat = 8
	verLinks  = 9  // 需要符号链接条目
	verExt    = 10 // 需要扩展时间/权限
	verHard   = 11 // 需要硬链接条目

	headerSize = 24 // v2 头长; v3+ 头长 32(后续再读 8 字节 rawLen)

	// 单个块声明长度的上界。取自 ultra 模式 newChunkerMinMax(nil, 4MB, 256MB) 的
	// 上限 —— 我们自己写出来的块不可能比它更大。
	maxChunkUncomp = 256 << 20
)

// 该写哪个版本? 逐条检查: 只要有一个条目用了高版本才装得下的特性, 就整体升上去
// (容器只有一个版本号, 不能每个条目各写各的)。
// precise = 用户显式要求纳秒时间戳(-T/--precise-times)
func writeVer(files []fileEntry, precise bool) byte {
	if precise {
		return verExt
	}
	v := byte(verCompat)
	for i := range files {
		fe := &files[i]
		if fe.isLink && v < verLinks {
			v = verLinks
		}
		if fe.isHard && v < verHard {
			v = verHard
		}
		if v >= verExt {
			continue
		}
		// v9 及以前: 权限只有 0777, 时间是 uint32 秒 —— 这两个存不下才升 v10
		if fe.mode&^uint32(os.ModePerm) != 0 || !fitsOldTime(fe.nano) {
			v = verExt
		}
	}
	return v
}

// 亚秒部分**不算**必须升版本的理由: 现实里几乎所有文件都带亚秒时间戳(APFS/ext4
// 都是纳秒级), 若因此一律写 v10, 等于让每一个新归档都与旧版二进制绝缘 —— 为了
// 一点点精度牺牲兼容性不值得。所以默认按秒存(与 tar 一致), 只有用户显式
// 要求 -T、或归档本来就要升 v10 时才把纳秒一起存下来。
func fitsOldTime(nano int64) bool {
	if nano == 0 {
		return true // 没有可用时间戳, 存 0 即可, 不必为此升版本
	}
	sec := nano / 1e9
	return sec >= 0 && sec <= 0xFFFFFFFF
}
