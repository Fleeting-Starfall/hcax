package main

// 打包侧: 收集输入、排除规则、分块去重、压缩、写容器。

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// 排除规则(glob): 命中的条目不进归档; 命中的目录整棵子树跳过(连遍历都不做,
// 备份时排除 .git / node_modules 这类大目录时能省下大量 stat 与读写)。
// 三种比对, 任一命中即排除:
//
//	"*.tmp"       —— 任意层级下叫这个名字的文件(按基名比)
//	".git"        —— 任意层级名为 .git 的目录(按路径分段比)
//	"build/*"     —— 打包根下 build 目录里的条目(按"相对打包根的路径"比)
//
// full = 磁盘上的真实路径, rel = 相对本次打包根的路径。两种都要比:
// 用户写模式时心里想的是归档里看到的路径(rel), 而 ".git" 这种又希望任意层级都生效(分段)。
func excluded(full, rel string, excl []string) bool {
	if len(excl) == 0 {
		return false
	}
	norm := filepath.ToSlash(full)
	cands := []string{norm}
	if rel != "" && rel != "." {
		cands = append(cands, filepath.ToSlash(rel))
	}
	for _, pat := range excl {
		if pat == "" {
			continue
		}
		for _, c := range cands {
			if ok, _ := filepath.Match(pat, c); ok {
				return true
			}
		}
		for _, seg := range strings.Split(norm, "/") {
			if ok, _ := filepath.Match(pat, seg); ok {
				return true
			}
		}
	}
	return false
}

// 同一个输入给了两遍(pack out.hcax dup dup): 老实现会把 dup 下每个文件存两份,
// 归档里躺着重复条目, 解包时后一个静默覆盖前一个 —— 既浪费又容易让人误解。
// 按 Clean 后的路径去重(./dup 与 dup 与 dup/ 都算同一个)。
func dedupPaths(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		k := filepath.Clean(p)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// 固实流是"把所有文件拼成一条再压缩", 于是**相邻的块是什么**直接影响压率:
// 一段文本后面紧跟一段随机二进制, 压缩器刚建立起来的上下文就被打断;
// 同类文件挨在一起, lzma2/zstd 能少花不少比特。
// 排序键: 扩展名 -> 大小量级 -> 原路径。最后一项保证同输入结果稳定(可复现)。
// 目录/链接条目不参与(它们不产生数据块, 顺序只影响元数据, 保持遍历序更直观)。
func orderForCompression(files []string, spec modeSpec) []string {
	if os.Getenv("HCAX_NO_ORDER") != "" { // A/B 对照用
		return files
	}
	// CM(text) 不走这套: 它的模型是**沿流自适应**的, 实测重排后反而差 7%
	// (12MB 混合语料 3.39MB -> 3.64MB)。固实流的"同类相邻"直觉对它不成立。
	if spec.backend == "cm" {
		return files
	}
	type ent struct {
		p    string
		ext  string
		size int64
	}
	es := make([]ent, 0, len(files))
	for _, p := range files {
		var sz int64
		if fi, e := os.Lstat(p); e == nil {
			sz = fi.Size()
		}
		es = append(es, ent{p: p, ext: strings.ToLower(filepath.Ext(p)), size: sz})
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].ext != es[j].ext {
			return es[i].ext < es[j].ext
		}
		bi, bj := sizeBucket(es[i].size), sizeBucket(es[j].size)
		if bi != bj {
			return bi < bj
		}
		return es[i].p < es[j].p
	})
	out := make([]string, len(es))
	for i := range es {
		out[i] = es[i].p
	}
	return out
}

// 大小按 2 的幂分桶(同一量级的文件放一起), 避免"1KB 和 100MB 交替"
func sizeBucket(n int64) int {
	b := 0
	for n > 1<<20 && b < 40 {
		n >>= 1
		b++
	}
	return b
}

// 输入去重之后仍可能撞名: pack out.hcax dup dup/f.txt —— dup/f.txt 既从遍历 dup
// 收集到, 又被显式点名。存两份会让解包时后者静默覆盖前者, 用户却以为两个都存了。
// 同一个归档内名字只可能来自同一份数据, 所以保留一份即可(并提示), 不必报错。
func dropDupNames(files, dirs, links []string, root string) ([]string, []string, []string) {
	seen := make(map[string]bool, len(files)+len(dirs)+len(links))
	dropped := 0
	keep := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			k := storedName(p, root)
			if seen[k] {
				dropped++
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
		return out
	}
	files, dirs, links = keep(files), keep(dirs), keep(links)
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "警告: 跳过 %d 个重复条目(输入重叠, 同一个归档内名字被覆盖到多次)\n", dropped)
	}
	return files, dirs, links
}

// 归档文件自己不能进归档: `hcax pack arch.hcax .` 会把上一次的 arch.hcax 也收进去,
// 于是每打一次包归档就胖一圈(154B -> 278B -> 389B...), 还把上一版内容当新数据存下来。
// tar 遇到这种情形会明确跳过并提示, 这里照做。
func excludeSelf(files, dirs, links []string, outPath string) ([]string, []string, []string) {
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return files, dirs, links
	}
	n := 0
	drop := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			if abs, e := filepath.Abs(p); e == nil && abs == outAbs {
				n++
				continue
			}
			out = append(out, p)
		}
		return out
	}
	files, dirs, links = drop(files), drop(dirs), drop(links)
	if n > 0 {
		fmt.Fprintf(os.Stderr, "警告: 跳过归档文件自身 %s(不能把自己打进自己)\n", outPath)
	}
	return files, dirs, links
}

var nExcluded int // 被 --exclude 排除的条目数(打包结束时报告)

func collectPaths(inputs []string, excl []string) (files []string, dirs []string, links []string) {
	for _, p := range inputs {
		fi, err := os.Lstat(p)
		if err != nil {
			fatal("stat %s: %v", p, err)
		}
		if excluded(p, "", excl) {
			nExcluded++
			continue
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
				rel := ""
				if r, e := filepath.Rel(p, q); e == nil {
					rel = r
				}
				if q != p && excluded(q, rel, excl) {
					nExcluded++
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if info.Mode()&os.ModeSymlink != 0 {
					links = append(links, q)
				} else if info.IsDir() {
					dirs = append(dirs, q)
				} else if !isRegular(info) {
					// socket / fifo / 设备节点: 读它会阻塞或直接失败,
					// 备份整个目录时不该因为这一个条目就让整次打包报销
					skipped = append(skipped, q)
				} else {
					files = append(files, q)
				}
				return nil
			})
		} else if !isRegular(fi) {
			skipped = append(skipped, p)
		} else {
			files = append(files, p)
		}
	}
	return
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 只收普通文件: 其它类型(socket/fifo/设备/管道)无法有意义地归档,
// 读它会阻塞(fifo)或直接失败(设备), 不该让整次打包报销。
func isRegular(fi os.FileInfo) bool {
	return fi.Mode()&os.ModeType == 0
}

var skipped []string // 被跳过的非普通文件(打包结束时一并报告)

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

// 取文件模式位: 只要权限位 + setuid/setgid/sticky。
// 老实现用 os.FileMode.Perm(), 它只取 0777 —— 于是 setuid/setgid/sticky 三个位
// 在归档里被静默丢掉(备份 /usr/bin 这类目录后, 程序会莫名失去提权位)。
// 类型位(目录/链接/设备)不存: 由 isDir/isLink 标志位表达, 存档更省。
func entryMode(fi os.FileInfo) uint32 {
	const keep = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return uint32(fi.Mode() & keep)
}

// 写入用的纳秒时间戳: 有些条目(如单元测试直接构造的)只填了整秒的 mtime,
// 这里统一补成纳秒, 免得写进 v10 归档后时间变成 0。
func entryNano(fe fileEntry) int64 {
	if fe.nano != 0 {
		return fe.nano
	}
	if fe.mtime != 0 {
		return int64(fe.mtime) * 1e9
	}
	return 0
}

// 还原时间戳: v10 用纳秒精度, 老归档只有整秒
func entryTime(fe fileEntry) time.Time {
	if fe.nano != 0 {
		return time.Unix(0, fe.nano)
	}
	return time.Unix(int64(fe.mtime), 0)
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

func pack(inputs []string, outPath, mode string, excl []string, precise bool) {
	spec, ok := modes[mode]
	if !ok {
		fatal("未知模式 %s", mode)
	}
	inputs = dedupPaths(inputs)

	files, dirs, links := collectPaths(inputs, excl)
	files = orderForCompression(files, spec)
	files, dirs, links = excludeSelf(files, dirs, links, outPath)
	files, dirs, links = dropDupNames(files, dirs, links, commonRoot(inputs))
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
	hardSeen := map[fileIDKey]int{} // (dev,ino) -> 首个条目在 fileEntries 中的下标
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
			// 单个文件读不到(权限/被删除)不该让整次备份报销: 警告并跳过
			fmt.Fprintf(os.Stderr, "警告: 跳过无法读取的文件 %s: %v\n", fp, err)
			skipped = append(skipped, fp)
			continue
		}
		rel := storedName(fp, root)
		st, _ := f.Stat()

		// 硬链接(v11): 同一 inode 的第二个及以后的名字, 直接复用首个条目的块索引 ——
		// CDC 去重本来就会命中同一批块, 所以既不占归档空间, 也省掉一整遍读盘/分块。
		if first, dup := hardSeen[fileKeyOf(st)]; dup {
			src := fileEntries[first]
			fileEntries = append(fileEntries, fileEntry{
				name: rel, size: src.size, mtime: uint64(st.ModTime().Unix()),
				nano: st.ModTime().UnixNano(), mode: entryMode(st),
				chunks: src.chunks, xform: src.xform, isHard: true, link: src.name,
			})
			totalUncomp += src.size
			f.Close()
			continue
		}
		if k, ok := fileKey(st); ok && k.nlink > 1 {
			hardSeen[k] = len(fileEntries) // 本条目即将 append 到的下标
		}
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
				nano: st.ModTime().UnixNano(),
				mode: entryMode(st), chunks: curChunks, xform: fileXform,
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
			nano: st.ModTime().UnixNano(),
			mode: entryMode(st), chunks: curChunks,
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
			nano: di.ModTime().UnixNano(),
			mode: entryMode(di), isDir: true,
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
			mtime: uint64(li.ModTime().Unix()), nano: li.ModTime().UnixNano(),
			mode:   entryMode(li),
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
			// 训练字典是要**写进归档**的, 小语料上它可能比省下的空间还多:
			// 实测 200KB 语料训出 114KB 字典, 占归档 57%, 整体压率反而 100%+。
			// 数据量不大时压两次比真实总大小(压缩帧 + 字典), 只有确实更小才用字典。
			// 大语料上字典开销相对可忽略, 且压一次 level-19 很贵, 故不比较。
			if be.hasDict && compSolidSize <= dictTrialMaxBytes {
				if _, e := csf.Seek(0, io.SeekStart); e != nil {
					fatal("seek 固实流: %v", e)
				}
				be.hasDict = false
				alt := be.compressZstdStream(csf)
				if len(alt) < len(frame)+len(be.trainDict) {
					frame = alt
					be.trainDict = nil // 字典不划算: 不写字典区
					if os.Getenv("HCAX_T") != "" {
						fmt.Fprintf(os.Stderr, "[t] dict dropped: %d B dict not worth it\n", len(be.trainDict))
					}
				} else {
					be.hasDict = true
				}
				trainDict = be.trainDict
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

	// 原子落盘: 先写同目录临时文件, 全部写成功后再 rename。
	// 直接 os.Create(outPath) 会先把已有的同名归档截断成 0 —— 万一中途失败
	// (磁盘满最常见), 旧备份没了、新的又只写了一半, 一次失败毁两份。见 util.go。
	aw := openArchiveForWrite(outPath)
	out := aw.f
	addCleanup(aw.abort) // 失败路径删掉半成品; 成功时 commit 已把 tmp 置空, 空操作
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
	wv := writeVer(fileEntries, precise)
	metaRaw := serializeMeta(chunkMetas, fileEntries, solidHash, rawHash, wv)
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
	var hdr bytes.Buffer
	hdr.WriteString(magic)
	hdr.Write([]byte{wv, spec.code, byte(spec.window), 0})
	binary.Write(&hdr, binary.LittleEndian, uint64(len(metaFrame)))
	binary.Write(&hdr, binary.LittleEndian, uint64(len(metaRaw)))
	binary.Write(&hdr, binary.LittleEndian, uint64(len(frame)))
	binary.Write(&hdr, binary.LittleEndian, uint64(rawRegionSize))
	binary.Write(&hdr, binary.LittleEndian, uint32(len(chunkMetas)))
	binary.Write(&hdr, binary.LittleEndian, uint32(len(fileEntries)))
	// trailer: "XACH" + totalUncomp(8) + nFiles(4) + nChunks(4)
	var trl bytes.Buffer
	trl.WriteString(trailerMag)
	binary.Write(&trl, binary.LittleEndian, totalUncomp)
	binary.Write(&trl, binary.LittleEndian, uint32(len(fileEntries)))
	binary.Write(&trl, binary.LittleEndian, uint32(len(chunkMetas)))

	// 布局: metaFrame | dataFrame | rawRegion | dictRegion
	mustWrite(out, hdr.Bytes(), "头部")
	mustWrite(out, metaFrame, "元数据帧")
	mustWrite(out, frame, "数据帧")
	if rawRegionSize > 0 {
		if _, err := rsf.Seek(0, io.SeekStart); err != nil {
			fatal("临时文件定位失败: %v", err)
		}
		mustCopy(out, rsf, "原样区")
	}
	mustWrite(out, dictBuf.Bytes(), "训练字典区")
	mustWrite(out, trl.Bytes(), "尾部")
	if err := out.Sync(); err != nil {
		fatal("刷盘失败(磁盘可能已满): %v", err)
	}
	sz, serr := out.Stat()
	// 先 Stat 再 Close: Close 之后 fd 失效, 取不到大小了
	if err := out.Close(); err != nil {
		fatal("关闭归档失败(磁盘可能已满): %v", err)
	}
	if serr != nil {
		fatal("取归档大小失败: %v", serr)
	}
	aw.commit() // 到此才算"打包成功": 旧归档在这一刻才被完整的新归档替换

	raw := totalUncomp
	// 全是空文件/空目录时 raw==0, 直接除会得到 +Inf% —— 显示成 "-" 更诚实
	ratio := "-"
	if raw > 0 {
		ratio = fmt.Sprintf("%.2f%%", 100.0*float64(sz.Size())/float64(raw))
	}
	runCleanups() // 成功路径也要删临时文件(清理钩子本身幂等, 重复执行无副作用)
	fmt.Printf("打包完成: %s  原始 %d B -> %d B  压率 %s  模式=%s  唯一块=%d(可压%d/原样%d)\n",
		outPath, raw, sz.Size(), ratio, mode, len(chunkMetas), nComp, nStored)
	if nExcluded > 0 {
		fmt.Printf("排除 %d 个条目(--exclude)\n", nExcluded)
	}
	if len(skipped) > 0 {
		fmt.Printf("跳过 %d 个条目(无权限或非普通文件): %s%s\n",
			len(skipped), strings.Join(skipped[:minInt(3, len(skipped))], ", "),
			func() string {
				if len(skipped) > 3 {
					return fmt.Sprintf(" 等 %d 个", len(skipped))
				}
				return ""
			}())
	}
}
