package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
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

// zstd 模式按块编码(块上限 chunkMax=256KiB), 单个块被独立 EncodeAll, 匹配距离不可能跨块,
// 故编码器窗口封顶到 1MiB(已远超 256KiB 块上限); 更大的窗口纯浪费内存。
// best 档原 window=27(1<<27=128MiB), 两个 zstd 编码器常驻 ~256MiB —— 是 best 内存(~264MB)远高于
// max(~88MB, zstd 模式才建编码器)的根因。封顶后 best 内存回到 ~10MB 级, 压率不变(块内匹配用不到大窗口)。
const zstdMaxWindowLog = 20

// dictTrialMaxBytes: 训练字典是否划算的"试压"上限。
// 字典本身要写进归档, 小语料上可能比它省下的还多 —— 只有可压数据不超过这个量时,
// 才值得多花一次压缩去比较"带字典帧+字典" vs "无字典帧"。(见 archive.go 中的试压逻辑)
const dictTrialMaxBytes = 32 << 20

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
	"text":  {4, "cm", 0, 0, false, false},     // 上下文混合: 对文本/代码/源数据极高压缩率(纯算法, 慢)
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
	trainDict []byte        // zstd(best)训练字典正文(流式压缩时复用以建输出编码器)
	hasDict   bool
	dictBytes []byte // 归档里的训练字典正文(流式**解压**时需重建带字典的解码器)
}

func newBackend(spec modeSpec) *backend {
	b := &backend{spec: spec, lcParam: "4", pbParam: "2"} // lzma2 默认; tuneLZMA2 会按数据自适应覆盖
	if spec.backend == "zstd" {
		var err error
		// v8c: zEnc(无字典编码器)改为惰性创建 —— best 档总会训练字典并用 zEncDict, 原 zEnc 从未被使用,
		// 却白白常驻一个 level-19 编码器(~35~69MB, 其内存由 level 决定、与窗口无关)。惰性创建后,
		// 只有"未训练字典"的 zstd 档(fast / 训练失败回退)才建它。
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
	if len(cb) == 0 {
		return true
	}
	// CM(text) 靠统计预测, 对高熵数据(随机/已压缩)既压不动又极慢(逐 bit 建模
	// ~1.3MB/s, 实测值见 README §8; 早期注释写的 0.5MB/s 是估算, 已更正)。
	// 让这类块走原样存储, 免得 CM 在压不出结果的数据上白耗时间。
	// (此前只有 lzma2 档做原样判定, text 档一律硬送进 CM。)
	if b.spec.backend == "cm" {
		return byteEntropy(cb) > 7.95
	}
	if b.spec.backend != "lzma2" {
		return false
	}
	out := b.gateEnc.EncodeAll(cb, nil)
	if len(out) < len(cb)*storeThresholdPct/100 {
		return false // 门控能压 -> 可压, 进固实流
	}
	// 门控"压不动": 还要确认它是否真的随机。图像残差这类数据常常"块内不可压、但整体(固实/lzma2大字典)可压",
	// 若仅凭门控就原样存储会误判丢压率。用字节熵二次确认: 只有高熵(真随机)才原样存储。
	return byteEntropy(cb) > 7.95
}

// lzma2 dict 按可压数据量收敛。lzma2 的匹配距离受限于 dict, hcax 块均 64KiB,
// 4MiB 已覆盖 ~64 个块的历史 —— 对"多样语料"(文本/JSON/源码混合)再往上加字典
// 实测毫无收益(12.6MB 语料: 4/8/16/32MiB 压缩结果**完全一样**), 只涨内存。
//
// 但可执行文件是另一回事: 20MB 的 Mach-O 上, dict 8MiB→16MiB 还能再省 4.0%
// (6,659,708 -> 6,395,092 B), 32MiB 则一分不省。这类数据的重复是"长距离"的
// (相同函数体/字符串表隔得很远), 只有字典够大才连得上。
//
// 所以 >=16MiB 的可压数据一律给 16MiB: 耗时不变(实测 3.30s vs 3.36s),
// 代价是 xz 子进程峰值内存从 ~121MB 涨到 ~208MB(只影响 >=16MiB 的输入;
// 更小的输入仍走 4MiB / 2MiB, 峰值 ~72MB)。

func dictFor(n int) string {
	switch {
	case n < 4<<20:
		return "2MiB"
	case n < 16<<20:
		return "4MiB"
	default:
		return "16MiB"
	}
}

// lzma2 lc/pb 自适应: 用固实流样本试若干候选参数, 选"投影最小"的组合。
// 只采用确实更小者 -> 绝不退步; 依赖系统 xz(缺失则保持默认, 回退 ulikunitz)。
// 实测: 对代码/结构化数据 pb=4 常优于默认 pb=2 (约 1~2%); 但最优值随数据而异, 故自适应。

func (b *backend) tuneLZMA2(r io.ReaderAt, size int64) {
	if b.spec.backend != "lzma2" || size == 0 {
		return
	}
	if _, err := exec.LookPath("xz"); err != nil {
		return
	}
	// 采样: 等距抽 8 段拼成 ~1MiB —— 既代表全流, 又够快(避免只取头部造成误判)
	if size < 256<<10 {
		return
	}
	sample := stridedSampleFrom(r, size, 1<<20)
	type cand struct{ lc, pb string }
	cands := []cand{{"4", "4"}, {"3", "4"}, {"4", "2"}, {"4", "3"}}
	bestLc, bestPb := b.lcParam, b.pbParam
	bestSz := int64(-1)
	for _, c := range cands {
		if out := b.xzCompress(bytes.NewReader(sample), c.lc, c.pb, b.dictParam); out != nil {
			if sz := int64(len(out)); bestSz < 0 || sz < bestSz {
				bestSz, bestLc, bestPb = sz, c.lc, c.pb
			}
		}
	}
	if bestSz >= 0 {
		b.lcParam, b.pbParam = bestLc, bestPb
	}
}

// 从 io.ReaderAt 等距抽 8 段拼成不超过 max 的样本(同 stridedSample, 但数据源是文件而非内存切片)
func stridedSampleFrom(r io.ReaderAt, size int64, max int) []byte {
	if size <= int64(max) {
		buf := make([]byte, size)
		r.ReadAt(buf, 0)
		return buf
	}
	seg := max / 8
	if seg < 4096 {
		seg = 4096
	}
	var out []byte
	stride := (size - int64(seg)) / 7
	for i := 0; i < 8; i++ {
		off := i * int(stride)
		if off+seg > int(size) {
			off = int(size) - seg
		}
		buf := make([]byte, seg)
		r.ReadAt(buf, int64(off))
		out = append(out, buf...)
	}
	return out
}

// xzFallbackWarn 记录"本进程是否已经警告过 xz 回退"; 只报一次, 免得 max 档
// 每条固实流都刷一行。测试用 xzWarnCount 断言警告确实发出过。
var (
	xzWarnOnce  sync.Once
	xzWarnCount int
)

// warnXZFallback: 系统 xz 不可用时必须让用户知道。
// 这不是"锦上添花"—— 实测同一语料: 有系统 xz 压率 26.89%, 回退到内置纯 Go lzma2
// 后 33.40%, 归档凭空大 24%。此前是**完全静默**的, 用户以为自己在用最强档。
func warnXZFallback(reason string) {
	xzWarnOnce.Do(func() {
		xzWarnCount++
		fmt.Fprintf(os.Stderr,
			"警告: %s; max/ultra 已回退到内置的纯 Go lzma2 实现\n"+
				"       压率会明显变差(实测同一语料 26.89%% -> 33.40%%, 归档大 24%%)。\n"+
				"       装上系统 xz 可恢复: brew install xz / apt-get install xz-utils\n", reason)
	})
}

// 用系统 xz 按指定 lc/pb 压缩; 不可用或出错时返回 nil(以便走回退路径)

func (b *backend) xzCompress(r io.Reader, lc, pb, dict string) []byte {
	xzbin, err := exec.LookPath("xz")
	if err != nil {
		warnXZFallback("未能在 PATH 中找到 xz")
		return nil
	}
	// v8d: 弃用极端模式 -e(nice=273, 深度 256MiB) —— 实测对 30MB 多样数据压率零收益(与 -9 完全一致),
	// 却显著拖慢匹配器。改 preset=9 + nice=64(标准 -9 档), 配合 dictFor 收敛的字典, 速度/内存大幅下降。
	// dict 上限 16MiB(见 dictFor): 超出后压率增益 < 0.03%, 但内存/耗时陡增。
	// v8e: 入参改为 io.Reader —— 固实流落临时文件后, xz 直接从文件流读取, 不再把全量固实流读进内存。
	cmd := exec.Command(xzbin, "-c", "-", "--lzma2=preset=9,dict="+dict+",nice=64,lc="+lc+",lp=0,pb="+pb)
	cmd.Stdin = r
	// stdout 与 stderr 必须是两个 buffer。此前两者共用一个, 于是 xz 往 stderr
	// 写的任何东西(哪怕只是一条无害警告)都会被拼进压缩流 —— 打包照样"成功",
	// 归档凭空大几十字节, 直到解包时才报 invalid header magic bytes。
	// 数据损坏是静默发生的, 所以这里分开接: stdout 是数据, stderr 只用于诊断。
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		reason := "系统 xz 执行失败"
		if msg := bytes.TrimSpace(errb.Bytes()); len(msg) > 0 {
			reason += "(xz 说: " + string(msg) + ")"
		}
		warnXZFallback(reason)
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
			outs[i] = b.xzCompress(bytes.NewReader(cb), b.lcParam, pb, b.dictParam)
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

// 等距采样(从内存切片): 旧版 tuneLZMA2 用; v8e 改用 stridedSampleFrom(文件源)

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
		if b.zEnc == nil {
			opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level))}
			if b.spec.window > 0 {
				wl := b.spec.window
				if wl > zstdMaxWindowLog {
					wl = zstdMaxWindowLog
				}
				opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
			}
			e, err := zstd.NewWriter(io.Discard, opts...)
			if err != nil {
				fatal("zstd encoder: %v", err)
			}
			b.zEnc = e
		}
		return b.zEnc.EncodeAll(cb, nil)
	}
	// lzma2 档: 系统 xz (lzma2, xz 容器) 取得接近 7z 的极高压缩率; 缺失则回退 ulikunitz xz。
	// 精调 lzma2 参数(dict/nice/lc/lp/pb) + dict 按量收敛, 比 -9e 预设再小约 0.34%, 同速。
	if b.spec.backend == "lzma2" {
		if out := b.xzCompress(bytes.NewReader(cb), b.lcParam, b.pbParam, b.dictParam); out != nil {
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

// 流式压缩固实流(zstd 档: fast/best): 从 io.Reader 逐段读入喂给 zstd 编码器,
// 内存只留"单块 + 编码器状态", 不再把整条固实流(可达数百 MiB)读进内存。
// 输出单条 zstd 帧(frame 经解码还原整条固实流), 解包逻辑不变。
func (b *backend) compressZstdStream(r io.Reader) []byte {
	var buf bytes.Buffer
	var enc *zstd.Encoder
	var err error
	if b.hasDict && b.trainDict != nil {
		opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level)), zstd.WithEncoderDict(b.trainDict)}
		enc, err = zstd.NewWriter(&buf, opts...)
	} else {
		opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(b.spec.level))}
		if b.spec.window > 0 {
			wl := b.spec.window
			if wl > zstdMaxWindowLog {
				wl = zstdMaxWindowLog
			}
			opts = append(opts, zstd.WithWindowSize(1<<uint(wl)))
		}
		enc, err = zstd.NewWriter(&buf, opts...)
	}
	if err != nil {
		fatal("zstd 流式编码器: %v", err)
	}
	if _, err := io.Copy(enc, r); err != nil {
		fatal("zstd 流式压缩: %v", err)
	}
	if err := enc.Close(); err != nil {
		fatal("zstd 关闭: %v", err)
	}
	return buf.Bytes()
}

// 并行固实分组压缩(max 档): 把可压块按序分 G 组, 每组并行调 xz 压成独立帧, 顺序拼接回单流。
// 段间冗余丢失 -> 压率略损, 但打包速度数倍提升。解压仍为整流(多 xz 帧串联), 格式不变。
// 注意: 数据来自 solid(打包阶段写好的固实缓冲), 不再读 cm.data —— v8 的 cm.data=nil 释放后 cm.data 已为空,
// 若此处读 c.data 会得到空组 -> 空帧 -> 解包越界panic。这是 mmt 路径在此前(v8)被引入的回归, 仅大语料(>=64MiB)触发。

func (b *backend) compressMT(chunks []*chunkMeta, solid io.ReaderAt, solidSize int64) []byte {
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
		return b.compress(concatChunksFile(comp, solid))
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
		// v8e: 固实流已落临时文件, 同组块按偏移从文件读出(一次 copy, 仅组大小常驻),
		// 不再持有"全量固实流 + 各组副本"双份 —— 此前大语料并行期内存翻倍的根因。
		start := comp[i].offset
		last := comp[e-1]
		end := last.offset + uint64(last.uncomp)
		buf := make([]byte, end-start)
		solid.ReadAt(buf, int64(start))
		local := map[*chunkMeta]uint64{}
		base := start
		for _, c := range comp[i:e] {
			local[c] = c.offset - base
		}
		groups = append(groups, grp{buf, local})
	}
	results := make([][]byte, len(groups))
	var wg sync.WaitGroup
	for gi := range groups {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			results[gi] = b.compressLZMA2Dict(groups[gi].sub, dictFor(len(groups[gi].sub)))
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

func concatChunksFile(cs []*chunkMeta, solid io.ReaderAt) []byte {
	var buf bytes.Buffer
	for _, c := range cs {
		seg := make([]byte, c.uncomp)
		solid.ReadAt(seg, int64(c.offset))
		buf.Write(seg)
	}
	return buf.Bytes()
}

// 按指定 lzma2 字典压缩(并行分组用): 组越小字典越小 -> xz 子进程内存随组大小线性下降。
// 系统 xz 缺失时回退 ulikunitz。xz 帧自描述(字典嵌帧头), 解压无需记参, 格式不变。

func (b *backend) compressLZMA2Dict(cb []byte, dict string) []byte {
	if b.spec.backend != "lzma2" {
		return b.compress(cb)
	}
	if len(cb) < 256<<10 {
		return b.compress(cb)
	}
	if out := b.xzCompress(bytes.NewReader(cb), b.lcParam, b.pbParam, dict); out != nil {
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

// maxOut: 期望的输出上限, 只 CM 需要(zstd/lzma2 有流结束标记, 自己会停)。
// CM 帧头的长度字段是归档里给的, 改坏了会让解码器跑到地老天荒 —— 见 cmDecompressTo。
func (b *backend) decompress(frame []byte, maxOut int64) []byte {
	if b.spec.backend == "cm" {
		return cmDecompress(frame, maxOut)
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

// 流式解压: 返回一个按后端封装好的 reader, 供"边解压边落盘"使用。
// 与 decompress(整份在内存) 的区别在于输出不再要求常驻 —— 这是解包侧内存有界的前提。
// 返回的 closeFn 负责释放解码器资源(zstd 会起后台 goroutine), 可为 nil。

func (b *backend) decompressReader(r io.Reader, maxOut int64) (io.Reader, func(), error) {
	switch b.spec.backend {
	case "cm":
		// CM 必须先拿到完整压缩帧(自描述长度头在帧首), 但输出是流式的:
		// 用 pipe 把 cmDecompressTo 的输出接到 reader 上, 内存只留一个 64KiB 缓冲。
		frame, err := io.ReadAll(r)
		if err != nil {
			return nil, nil, err
		}
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(cmDecompressTo(pw, frame, maxOut))
		}()
		return pr, func() { pr.Close() }, nil
	case "zstd":
		opts := []zstd.DOption{}
		if len(b.dictBytes) > 0 {
			opts = append(opts, zstd.WithDecoderDicts(b.dictBytes))
		}
		d, err := zstd.NewReader(r, opts...)
		if err != nil {
			return nil, nil, err
		}
		return d, func() { d.Close() }, nil
	default: // lzma2: xz 容器, 本身即流式
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return xr, nil, nil
	}
}

// ---------- container structs ----------

type archive struct {
	spec      modeSpec
	be        *backend
	dataStart int64

	// 归档文件句柄: 原样区/数据区按需 ReadAt, 不再整段读进内存
	src         *os.File
	compLen     int64 // 压缩帧字节数
	metaCompLen int64 // 元数据帧压缩后长度
	metaRawLen  int64 // 元数据解压后长度
	rawOff      int64 // 原样区在归档文件中的偏移
	rawLen      int64

	// 固实流解压结果落临时文件(与打包侧 v8e 对称), 且**惰性解压**:
	// 只有真正要读某个块时才解压到该块末尾为止。
	// 此前 openArchive 无条件把整条固实流 + 原样区解进内存 —— list 一个归档
	// 要吃掉全量数据的内存, extract 单个小文件也要解压整条流。
	solidFile *os.File
	solidR    io.Reader // 解压器(保留以便续解压)
	solidSize int64     // 已解压字节数
	solidEOF  bool      // 流已到底

	// 固实流**总的**未压缩大小(各非原样块 uncomp 之和)。CM 解码需要它当上限:
	// CM 帧头声明的长度不可信, 改坏了能让解码器跑到挂死。老格式算不出时为 0,
	// cmDecompressTo 会退回一个宽松的绝对上限。
	solidUncomp int64

	// 解包进度(与打包侧的进度对称): 只写 stderr, 不污染 stdout 的机器可读输出
	progShow  bool
	progDone  uint64
	progLast  uint64
	progTotal uint64
	overwrote int // 本次解包改写掉的、输出目录里已存在的条目数

	// 目录条目的权限/时间戳不能"边解边设": 往目录里写文件会把它的 mtime 改成
	// "现在", 提前 chmod 成只读(0555)更会让后面的子文件写不进去。都推到最后
	// 统一落地(见 applyDirMeta)。
	dirTodos []dirTodo

	chunks    []chunkMeta
	files     []fileEntry
	ver       byte // 归档格式版本(v7 起文件级变换显式记录; 更早版本靠启发式判断)
	hashLen   int  // 逐块哈希长度: 旧版=16, v6=0(改用流级校验)
	solidHash [8]byte
	rawHash   [8]byte
}

// 待补的目录元数据(权限 + mtime)
type dirTodo struct {
	path string
	mode uint32
	nano int64
}
