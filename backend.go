package main

import (
	"bytes"
	"io"
	"os/exec"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const storeThresholdPct = 100

// mmtMinBytes: 并行固实分组(mmt)的数据量门槛。
// 并行分组把固实流切成多段独立压缩, 会丢失段间冗余 -> 压率略损。
// 中小语料上"压率越低越好"优先, 故仅在可压数据足够大(>=64MiB, 速度才是瓶颈)时才用并行。

const mmtMinBytes = 64 << 20

type modeSpec struct {
	code    byte
	backend string // "zstd" or "lzma2"
	level   int
	window  int  // zstd window log / lzma2 dict 提示
	solid   bool // true=整文件固实(关 CDC 去重, 像 7z); 用于 ultra
	mmt     bool // true=并行固实分组压缩; 用于 max
}

var modes = map[string]modeSpec{
	"fast":  {0, "zstd", 3, 0, false, false},
	"best":  {1, "zstd", 19, 27, false, false},
	"max":   {2, "lzma2", 9, 27, false, true},
	"ultra": {3, "lzma2", 9, 27, false, false}, // v8b: 关固实(去重), 内存从 ~1.8GB 降到 ~max 级(~86MB); 仍单流全流选参保压率(区别于 max 的并行)
	"text":  {4, "cm", 0, 0, false, false}, // 上下文混合: 对文本/代码/源数据极高压缩率(纯算法, 慢)
}

type backend struct {
	spec      modeSpec
	zEnc      *zstd.Encoder // 无字典编码(zstd)
	zDec      *zstd.Decoder
	zEncDict  *zstd.Encoder // 带训练字典编码(zstd best 相似文件)
	zDecDict  *zstd.Decoder // 带训练字典解码
	gateEnc   *zstd.Encoder // 廉价 l1 编码器, 用于不可压判定/逐块预处理选型
	dictParam string        // lzma2 dict 大小(按可压数据量收敛)
	lcParam   string        // lzma2 lc(literal context bits), 自适应选取
	pbParam   string        // lzma2 pb(position bits), 自适应选取
	hasDict   bool
}

func newBackend(spec modeSpec) *backend {
	b := &backend{spec: spec, lcParam: "4", pbParam: "2"} // lzma2 默认; tuneLZMA2 会按数据自适应覆盖
	if spec.backend == "zstd" {
		var err error
		opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(spec.level))}
		if spec.window > 0 {
			opts = append(opts, zstd.WithWindowSize(1<<uint(spec.window)))
		}
		b.zEnc, err = zstd.NewWriter(io.Discard, opts...)
		if err != nil {
			fatal("zstd encoder: %v", err)
		}
		b.zDec, err = zstd.NewReader(nil)
		if err != nil {
			fatal("zstd decoder: %v", err)
		}
	}
	// v8: 门控编码器只用于"估计块是否可压"(选原样/选预处理), 不需要大窗口/历史。
	// 用固定 1MiB 窗口, 避免 zstd 流式编码器历史缓冲随数据膨胀(大文件可飙到 GB 级, 见 ensureHist)。
	// 注意: 仅 zstd 模式才需要 zEnc/zDec; lzma2 模式不创建, 省下 128MiB 字典窗口。
	gateOpts := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithWindowSize(1 << 20)}
	g, err := zstd.NewWriter(io.Discard, gateOpts...)
	if err != nil {
		fatal("gate encoder: %v", err)
	}
	b.gateEnc = g
	return b
}

// 逐块自适应预处理选型: 试候选变换, 用门控(zstd-l3)投影选最小者。
// 只采用"确实变小时"的变换 -> 绝不退步; 且能抓取无文件头的结构化数据(裸光栅/内嵌x86代码)。
// light=true(用于 best 快档): 只试主要候选, 降门控开销保速度; max/ultra 档用全 6 候选求更优。

func (b *backend) chooseTransform(cb []byte) (byte, []byte) {
	if len(cb) < 4 {
		return xfNone, append([]byte(nil), cb...)
	}
	var cands []byte
	if b.spec.backend == "lzma2" {
		cands = []byte{xfNone, xfDelta1, xfDelta2, xfDelta3, xfDelta4, xfBCJX86}
	} else {
		cands = []byte{xfNone, xfDelta3, xfBCJX86} // best: 轻量候选, 保速度
	}
	best, bestSz := xfNone, len(cb)
	// v8: 复用单个 scratch 试所有候选, 不再为每块分配 6 个全尺寸缓冲
	scratch := make([]byte, len(cb))
	for _, t := range cands {
		copy(scratch, cb)
		applyXformInPlace(t, scratch)
		if sz := len(b.gateEnc.EncodeAll(scratch, nil)); sz < bestSz {
			best, bestSz = t, sz
		}
	}
	return best, applyXform(best, cb, false)
}

// 廉价判定: 该块是否不可压(应原样存储而非进压缩器)。zstd 模式恒压, 不原样存储。

func (b *backend) shouldStore(cb []byte) bool {
	if b.spec.backend != "lzma2" {
		return false
	}
	if len(cb) == 0 {
		return true
	}
	out := b.gateEnc.EncodeAll(cb, nil)
	if len(out) < len(cb)*storeThresholdPct/100 {
		return false // 门控能压 -> 可压, 进固实流
	}
	// 门控"压不动": 还要确认它是否真的随机。图像残差这类数据常常"块内不可压、但整体(固实/lzma2大字典)可压",
	// 若仅凭门控就原样存储会误判丢压率。用字节熵二次确认: 只有高熵(真随机)才原样存储。
	return byteEntropy(cb) > 7.95
}

// lzma2 dict 按可压数据量收敛: 小数据用更小 dict 更快且压率不丢; 大数据用 256MiB 上限

func dictFor(n int) string {
	switch {
	case n < 32<<20:
		return "32MiB"
	case n < 96<<20:
		return "64MiB"
	case n < 256<<20:
		return "128MiB"
	default:
		return "256MiB"
	}
}

// lzma2 lc/pb 自适应: 用固实流样本试若干候选参数, 选"投影最小"的组合。
// 只采用确实更小者 -> 绝不退步; 依赖系统 xz(缺失则保持默认, 回退 ulikunitz)。
// 实测: 对代码/结构化数据 pb=4 常优于默认 pb=2 (约 1~2%); 但最优值随数据而异, 故自适应。

func (b *backend) tuneLZMA2(solid []byte) {
	if b.spec.backend != "lzma2" || len(solid) == 0 {
		return
	}
	if _, err := exec.LookPath("xz"); err != nil {
		return
	}
	// 采样: 等距抽 8 段拼成 ~1MiB —— 既代表全流, 又够快(避免只取头部造成误判)
	if len(solid) < 256<<10 {
		return
	}
	sample := stridedSample(solid, 1<<20)
	type cand struct{ lc, pb string }
	cands := []cand{{"4", "4"}, {"3", "4"}, {"4", "2"}, {"4", "3"}}
	bestLc, bestPb := b.lcParam, b.pbParam
	bestSz := int64(-1)
	for _, c := range cands {
		if out := b.xzCompress(sample, c.lc, c.pb); out != nil {
			if sz := int64(len(out)); bestSz < 0 || sz < bestSz {
				bestSz, bestLc, bestPb = sz, c.lc, c.pb
			}
		}
	}
	if bestSz >= 0 {
		b.lcParam, b.pbParam = bestLc, bestPb
	}
}

// 用系统 xz 按指定 lc/pb 压缩; 不可用或出错时返回 nil(以便走回退路径)

func (b *backend) xzCompress(cb []byte, lc, pb string) []byte {
	xzbin, err := exec.LookPath("xz")
	if err != nil {
		return nil
	}
	cmd := exec.Command(xzbin, "-c", "-", "--lzma2=preset=9e,dict="+b.dictParam+",nice=273,lc="+lc+",lp=0,pb="+pb)
	cmd.Stdin = bytes.NewReader(cb)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

// 中小数据(单流)选参: 对全流试 pb=2/4, 选最小者并**直接复用其压缩结果**(不重复压),
// 同时记录胜出参数供后续(如 mmt 分组)使用。基于全流真实结果, 比采样更可靠; 代价 2 次全流压缩。

func (b *backend) compressLZMA2Best(cb []byte) []byte {
	if b.spec.backend != "lzma2" {
		return b.compress(cb) // zstd 等直接走原路径
	}
	if len(cb) < 256<<10 {
		return b.compress(cb) // 太小, 不值得选参
	}
	pbs := []string{"2", "4"}
	outs := make([][]byte, len(pbs))
	var wg sync.WaitGroup
	for i, pb := range pbs {
		wg.Add(1)
		go func(i int, pb string) {
			defer wg.Done()
			outs[i] = b.xzCompress(cb, b.lcParam, pb)
		}(i, pb)
	}
	wg.Wait()
	var best []byte
	for i, out := range outs {
		if out != nil && (best == nil || len(out) < len(best)) {
			best = out
			b.pbParam = pbs[i]
		}
	}
	if best == nil {
		return b.compress(cb) // xz 不可用 -> 回退
	}
	return best
}

// 等距采样: 从头到尾均匀抽 8 段拼成不超过 max 的样本, 兼顾代表性与速度

func stridedSample(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	seg := max / 8
	if seg < 4096 {
		seg = 4096
	}
	var out []byte
	stride := (len(b) - seg) / 7
	for i := 0; i < 8; i++ {
		off := i * stride
		if off+seg > len(b) {
			off = len(b) - seg
		}
		out = append(out, b[off:off+seg]...)
	}
	return out
}

func (b *backend) compress(cb []byte) []byte {
	if b.spec.backend == "cm" {
		return cmCompress(cb)
	}
	if b.spec.backend == "zstd" {
		if b.hasDict && b.zEncDict != nil {
			return b.zEncDict.EncodeAll(cb, nil)
		}
		return b.zEnc.EncodeAll(cb, nil)
	}
	// lzma2 档: 系统 xz (lzma2, xz 容器) 取得接近 7z 的极高压缩率; 缺失则回退 ulikunitz xz。
	// 精调 lzma2 参数(dict/nice/lc/lp/pb) + dict 按量收敛, 比 -9e 预设再小约 0.34%, 同速。
	if b.spec.backend == "lzma2" {
		if out := b.xzCompress(cb, b.lcParam, b.pbParam); out != nil {
			return out
		}
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			fatal("xz writer: %v", err)
		}
		if _, err := w.Write(cb); err != nil {
			fatal("xz write: %v", err)
		}
		if err := w.Close(); err != nil {
			fatal("xz close: %v", err)
		}
		return buf.Bytes()
	}
	fatal("未知后端 %s", b.spec.backend)
	return nil
}

// 并行固实分组压缩(max 档): 把可压块按序分 G 组, 每组并行调 xz 压成独立帧, 顺序拼接回单流。
// 段间冗余丢失 -> 压率略损, 但打包速度数倍提升。解压仍为整流(多 xz 帧串联), 格式不变。

func (b *backend) compressMT(chunks []*chunkMeta) []byte {
	var comp []*chunkMeta
	for _, c := range chunks {
		if !c.stored {
			comp = append(comp, c)
		}
	}
	if len(comp) == 0 {
		return nil
	}
	G := runtime.NumCPU()
	if G > 8 {
		G = 8
	}
	if G < 1 {
		G = 1
	}
	if len(comp) <= G {
		return b.compress(concatChunks(comp))
	}
	per := (len(comp) + G - 1) / G
	type grp struct {
		sub   []byte
		local map[*chunkMeta]uint64
	}
	var groups []grp
	for i := 0; i < len(comp); i += per {
		e := i + per
		if e > len(comp) {
			e = len(comp)
		}
		var sub bytes.Buffer
		local := map[*chunkMeta]uint64{}
		off := uint64(0)
		for _, c := range comp[i:e] {
			sub.Write(c.data)
			local[c] = off
			off += uint64(len(c.data))
		}
		groups = append(groups, grp{sub.Bytes(), local})
	}
	results := make([][]byte, len(groups))
	var wg sync.WaitGroup
	for gi := range groups {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			results[gi] = b.compress(groups[gi].sub)
		}(gi)
	}
	wg.Wait()
	var out bytes.Buffer
	ugs := uint64(0) // 未压缩累计(偏移必须在解压后流中定位, 不能用压缩字节)
	for gi, g := range groups {
		out.Write(results[gi])
		for c, lo := range g.local {
			c.offset = ugs + lo
		}
		ugs += uint64(len(g.sub))
	}
	return out.Bytes()
}

func concatChunks(cs []*chunkMeta) []byte {
	var buf bytes.Buffer
	for _, c := range cs {
		buf.Write(c.data)
	}
	return buf.Bytes()
}

func (b *backend) decompress(frame []byte) []byte {
	if b.spec.backend == "cm" {
		return cmDecompress(frame)
	}
	if b.spec.backend == "zstd" {
		if b.zDecDict != nil {
			out, err := b.zDecDict.DecodeAll(frame, nil)
			if err != nil {
				fatal("zstd dict decode: %v", err)
			}
			return out
		}
		out, err := b.zDec.DecodeAll(frame, nil)
		if err != nil {
			fatal("zstd decode: %v", err)
		}
		return out
	}
	r, err := xz.NewReader(bytes.NewReader(frame))
	if err != nil {
		fatal("xz reader: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		fatal("xz read: %v", err)
	}
	return out
}

// ---------- container structs ----------

type archive struct {
	spec      modeSpec
	be        *backend
	dataStart int64
	compSolid []byte
	rawRegion []byte
	chunks    []chunkMeta
	files     []fileEntry
	ver       byte // 归档格式版本(v7 起文件级变换显式记录; 更早版本靠启发式判断)
	hashLen   int // 逐块哈希长度: 旧版=16, v6=0(改用流级校验)
	solidHash [8]byte
	rawHash   [8]byte
}
