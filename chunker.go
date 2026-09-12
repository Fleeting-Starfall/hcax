package main

import (
	"crypto/sha256"
	"math/rand"
	"os"
	"sync"
)

const (
	magic      = "HCAX"
	trailerMag = "XACH"
	version    = 11 // v10: 文件表条目的时间改 int64 纳秒、权限改 uint32(带 setuid/setgid/sticky)
	// 兼容读 v2~v10; 写的时候能不升就不升:
	//   普通归档仍写 v8, 有符号链接才升 v9, 只有 v9 装不下的元数据才升 v10。
	// 这样格式演进不破坏既有生态 —— 旧版二进制照样能解开新工具打的绝大多数包。
	verCompat = 8
	verLinks  = 9  // 需要符号链接条目
	verExt    = 10 // 需要扩展时间/权限
	verHard   = 11 // 需要硬链接条目

	chunkBits = 16
	chunkMin  = 8 * 1024
	chunkMax  = 256 * 1024
	mask64    = (1 << chunkBits) - 1

	headerSize = 24 // v2 头长; v3+ 头长 32(后续再读 8 字节 rawLen)
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

// 不可压判定阈值: 廉价 zstd-l1 压缩后若输出 >= 输入(完全压不动)才原样存储;
// 只要 l1 能缩小一点点, 就交真压缩器(可能压得更好), 避免误丢压率

var gear = func() [256]uint64 {
	var g [256]uint64
	r := rand.New(rand.NewSource(0x9E3779B1))
	for i := range g {
		g[i] = r.Uint64()
	}
	return g
}()

func hash16(b []byte) [16]byte {
	h := sha256.Sum256(b)
	var out [16]byte
	copy(out[:], h[:16])
	return out
}

// ---------- 内容自适应预处理 (DELTA / BCJ-x86) ----------
// 进压缩器前对可压块做变换, 把"结构化冗余"暴露给 LZ/熵编码, 解码时逆变换还原原始字节.
// 纯 Go 实现, 自包含可逆, 不依赖 xz 滤波器解码.

func newChunker(on func([]byte)) *chunker {
	return &chunker{minS: chunkMin, maxS: chunkMax, mask: mask64, gear: gear, onChunk: on}
}

func newChunkerMinMax(on func([]byte), mn, mx int) *chunker {
	return &chunker{minS: mn, maxS: mx, mask: mask64, gear: gear, onChunk: on}
}

// v8: 复用块缓冲, 避免大文件分块时每块都 make([]byte,~64K) 造成瞬时内存/GC 峰值(尤其 GOMAXPROCS 高时).
// onChunk 内部会把所需数据复制到 cm.data(含 xfNone 也复制), 故归还缓冲安全.
var chunkPool = sync.Pool{New: func() interface{} { b := make([]byte, 0, chunkMax); return &b }}

func (c *chunker) emit(n int) {
	size := n - c.start
	var chunk []byte
	pp := chunkPool.Get().(*[]byte)
	if cap(*pp) >= size {
		chunk = (*pp)[:size]
	} else {
		chunk = make([]byte, size)
		pp = nil
	}
	copy(chunk, c.pending[c.start:n])
	c.onChunk(chunk)
	if pp != nil {
		*pp = (*pp)[:0]
		chunkPool.Put(pp)
	}
	rest := make([]byte, len(c.pending)-n)
	copy(rest, c.pending[n:])
	c.pending = rest
	c.start = 0
	c.scanned = 0
	c.rh = 0
}

func (c *chunker) write(p []byte) {
	c.pending = append(c.pending, p...)
	for {
		avail := len(c.pending) - c.start
		if avail < c.minS {
			return
		}
		end := c.start + c.maxS
		if end > len(c.pending) {
			end = len(c.pending)
		}
		cut := 0
		i := c.scanned
		for ; i < end; i++ {
			c.rh = (c.rh << 1) + c.gear[c.pending[i]]
			if i >= c.start+c.minS && (c.rh&c.mask) == 0 {
				cut = i + 1
				break
			}
		}
		c.scanned = i
		if cut > 0 {
			c.emit(cut)
			continue
		}
		if avail >= c.maxS {
			c.emit(end)
			continue
		}
		return
	}
}

func (c *chunker) flush() {
	if len(c.pending) > c.start {
		c.emit(len(c.pending))
	}
}

// ---------- compression backends ----------
