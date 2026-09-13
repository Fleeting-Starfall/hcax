package main

// 随机化测试: 手写的用例只覆盖"想得到的情形", 而 R18 那种
// "高重复度归档打得开、解不开"的 bug, 恰恰是想不到的那类。
// 这里让机器去撞: 随机目录树往返 + 随机损坏后的行为。
//
//   go test -run TestRandom -v ./...

import (
	"bytes"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- 把 fatal 变成 panic, 好在测试里断言"干净地失败" ----------

type fatalPanic struct{}

// runCatchingFatal 执行 fn; 返回 true 表示"干净地报错退出"(fatal),
// false 表示正常跑完。真正的运行时 panic(index 越界/空指针等)会继续往上抛,
// 由 go test 记成失败 —— 那正是我们想抓的东西。
func runCatchingFatal(fn func()) (fataled bool) {
	old := fatalExit
	fatalExit = func(int) { panic(fatalPanic{}) }
	defer func() {
		fatalExit = old
		if r := recover(); r != nil {
			if _, ok := r.(fatalPanic); ok {
				fataled = true
				return
			}
			panic(r) // 不是 fatal —— 真的崩了, 交给 go test
		}
	}()
	fn()
	return false
}

// ---------- 随机目录树 ----------

var namePool = []string{
	"a.txt", "b.bin", "a..b.txt", "..weird", "sp ace.txt", "中文.txt",
	".hidden", "x", "y.log", "ZZZ", "n1", "n2", "deep", "empty",
}

func randName(rng *rand.Rand, used map[string]bool) string {
	for i := 0; i < 50; i++ {
		n := namePool[rng.Intn(len(namePool))]
		if !used[n] {
			used[n] = true
			return n
		}
	}
	return "fallback"
}

// 造一棵随机目录树。返回内容可复现(rng 定种子), 失败时能重跑定位。
func buildRandomTree(t *testing.T, rng *rand.Rand, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var files []string // 已建的普通文件, 供符号链接/硬链接引用
	var dirs []string  // 目录: 权限与 mtime 必须等整棵树建完再设 ——
	// 往目录里加东西会把它自己的 mtime 冲掉, 提前设成只读更是直接写不进去
	var build func(dir string, depth int)
	build = func(dir string, depth int) {
		used := map[string]bool{}
		n := 3 + rng.Intn(6)
		for i := 0; i < n; i++ {
			name := randName(rng, used)
			p := filepath.Join(dir, name)
			switch {
			case depth < 3 && rng.Intn(4) == 0: // 子目录
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				dirs = append(dirs, p)
				build(p, depth+1)
			case len(files) > 0 && rng.Intn(8) == 0: // 符号链接
				target, _ := filepath.Rel(dir, files[rng.Intn(len(files))])
				if rng.Intn(4) == 0 {
					target = "nowhere-" + name // 悬空链接
				}
				if err := os.Symlink(target, p); err == nil {
					continue
				}
				fallthrough
			case len(files) > 0 && rng.Intn(10) == 0: // 硬链接
				if err := os.Link(files[rng.Intn(len(files))], p); err == nil {
					files = append(files, p)
					continue
				}
				fallthrough
			default: // 普通文件
				size := rng.Intn(4) * rng.Intn(60000) // 大量 0 字节文件 + 少量大文件
				buf := make([]byte, size)
				switch rng.Intn(3) {
				case 0: // 随机(不可压)
					rng.Read(buf)
				case 1: // 高度重复(内部去重路径)
					for j := range buf {
						buf[j] = byte(j % 7)
					}
				default: // 可压的伪文本
					for j := range buf {
						buf[j] = byte('a' + (j+rng.Intn(3))%20)
					}
				}
				mode := os.FileMode(0o644)
				switch rng.Intn(4) {
				case 0:
					mode = 0o600
				case 1:
					mode = 0o755
				case 2:
					mode = 0o400 // 只读: 解包时若顺序不对就会写不进去
				}
				if err := os.WriteFile(p, buf, mode); err != nil {
					t.Fatal(err)
				}
				randMeta(t, rng, p)
				files = append(files, p)
			}
		}
	}
	build(root, 0)
	// 目录的元数据最后统一设(与解包侧 applyDirMeta 对称): 深的先设,
	// 免得父目录先被改成只读后子目录设不动
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		mode := os.FileMode(0o755)
		switch rng.Intn(3) {
		case 0:
			mode = 0o700
		case 1:
			mode = 0o500 // 只读目录: 解包时若权限提前生效就会写不进子文件
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		randMeta(t, rng, d)
	}
}

// 只读目录(0500)会让 t.TempDir() 收尾时删不掉子文件。解出来的那棵树同样有
// 0500 目录, 所以要对整个临时目录(不只是源树)放开权限。
// Cleanup 后注册的先跑, 因此这条一定在 TempDir 自己删除之前执行。
func makeWritableOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

// 随机 mtime(整秒: 默认按秒存, 用整秒才好逐字节比对)。
// 权限/时间还原是"目录元数据延迟落地"那类改动的唯一防线 —— 光比对内容测不出来。
func randMeta(t *testing.T, rng *rand.Rand, p string) {
	t.Helper()
	sec := 978307200 + rng.Int63n(20*365*24*3600) // 2001-01-01 起 20 年内
	ts := time.Unix(sec, 0)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// ---------- 目录树比对 ----------

type node struct {
	kind  byte // 'f' 文件, 'd' 目录, 'l' 链接
	size  int64
	link  string
	data  []byte
	ino   uint64
	mode  os.FileMode
	mtime int64 // Unix 秒
}

func scanTree(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			// 链接自身的 mtime 还原不了(Go 没有 lutimes), 不比对
			out[rel] = node{kind: 'l', link: tgt}
		case fi.IsDir():
			out[rel] = node{kind: 'd', mode: fi.Mode().Perm(), mtime: fi.ModTime().Unix()}
		default:
			data, _ := os.ReadFile(p)
			out[rel] = node{kind: 'f', size: fi.Size(), data: data, ino: inoOf(fi),
				mode: fi.Mode().Perm(), mtime: fi.ModTime().Unix()}
		}
		return nil
	})
	return out
}

func inoOf(fi os.FileInfo) uint64 {
	if k, ok := fileKey(fi); ok {
		return k.ino
	}
	return 0
}

// checkMeta=false 时只比对"有没有、内容对不对"(用于部分抽取: 只抽了文件的
// 时候, 父目录是顺手建出来的, 归档里的目录条目没被抽中, 时间与权限自然不该有)
func diffTrees(t *testing.T, want, got map[string]node, ctx string) {
	diffTreesOpt(t, want, got, ctx, true)
}

func diffTreesOpt(t *testing.T, want, got map[string]node, ctx string, checkMeta bool) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := want[k]
		g, ok := got[k] // 注意: 老代码写成 w, ok := got[k], 于是全程拿 got 跟自己比,
		if !ok {        //       内容/权限/时间一项都没真比对 —— 这个用例一直是空转的
			t.Errorf("%s: 缺少条目 %s", ctx, k)
			continue
		}
		if w.kind != g.kind {
			t.Errorf("%s: %s 类型不符 (%c vs %c)", ctx, k, w.kind, g.kind)
			continue
		}
		switch w.kind {
		case 'l':
			if w.link != g.link {
				t.Errorf("%s: %s 链接目标不符 (%q vs %q)", ctx, k, w.link, g.link)
			}
		case 'f':
			if w.size != g.size || !bytes.Equal(w.data, g.data) {
				t.Errorf("%s: %s 内容不符 (%d vs %d 字节)", ctx, k, w.size, g.size)
			}
		}
		if checkMeta && (w.kind == 'f' || w.kind == 'd') {
			if w.mode != g.mode {
				t.Errorf("%s: %s 权限不符 (%o vs %o)", ctx, k, w.mode, g.mode)
			}
			if w.mtime != g.mtime {
				t.Errorf("%s: %s mtime 不符 (%d vs %d)", ctx, k, w.mtime, g.mtime)
			}
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: 多出条目 %s", ctx, k)
		}
	}
}

// ---------- 用例 1: 随机目录树 × 各模式 往返 ----------

func TestRandomTreeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20260913))
	for iter := 0; iter < 4; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		for _, mode := range []string{"fast", "best", "max", "ultra"} {
			arc := filepath.Join(dir, "a_"+mode+".hcax")
			out := filepath.Join(dir, "o_"+mode)
			if runCatchingFatal(func() { pack([]string{src}, arc, mode, nil, false) }) {
				t.Fatalf("第%d轮 [%s] 打包失败", iter, mode)
			}
			if runCatchingFatal(func() { unpack(arc, out, false, nil) }) {
				t.Fatalf("第%d轮 [%s] 解包失败", iter, mode)
			}
			diffTrees(t, want, scanTree(t, filepath.Join(out, "src")), "iter"+string(rune('0'+iter))+"/"+mode)
			// 硬链接必须还原成共享 inode(能解出内容还不够, 结构也得对)
			checkHardlinks(t, want, filepath.Join(out, "src"), filepath.Join(out, "src"))
		}
	}
}

// ---------- 用例 1b: 部分抽取 —— 抽谁就得谁, 一个不多一个不少 ----------
// extract 的匹配规则(全路径 / 基名 / 目录前缀)是 R13 之后才有的, 之前"抽一个
// 子目录"只会建出空目录还报成功。这里让随机树自己去撞这些分支。

func TestRandomPartialExtract(t *testing.T) {
	rng := rand.New(rand.NewSource(4242))
	for iter := 0; iter < 6; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		arc := filepath.Join(dir, "a.hcax")
		if runCatchingFatal(func() { pack([]string{src}, arc, "fast", nil, false) }) {
			t.Fatalf("第%d轮 打包失败", iter)
		}
		// 归档内的路径带 src/ 前缀
		var all []string
		for k := range want {
			all = append(all, filepath.ToSlash(filepath.Join("src", k)))
		}
		sort.Strings(all)
		if len(all) == 0 {
			continue
		}
		// 随机挑 1~3 个目标, 交替用"全路径"和"基名"两种问法
		asks := map[string]bool{}
		for i := 0; i < 1+rng.Intn(3); i++ {
			a := all[rng.Intn(len(all))]
			if rng.Intn(2) == 0 {
				a = path.Base(a)
			}
			asks[a] = true
		}
		var only []string
		for a := range asks {
			only = append(only, a)
		}
		// 期望集: 逐个条目过一遍 matchEntry(与解包侧同一套规则)
		exp := map[string]node{}
		for k, v := range want {
			full := filepath.ToSlash(filepath.Join("src", k))
			for _, a := range only {
				if matchEntry(full, a) {
					exp[full] = v
					break
				}
			}
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, false, only) }) {
			t.Fatalf("第%d轮 抽取失败 only=%v", iter, only)
		}
		got := scanTree(t, out)
		// 空目录在抽取结果里是顺带建出来的, 归档里没抽中它 —— 扫出来多算, 先剔掉
		for k, v := range got {
			if v.kind == 'd' {
				if _, ok := exp[k]; !ok {
					delete(got, k)
				}
			}
		}
		diffTreesOpt(t, exp, got, "iter"+string(rune('0'+iter))+" only="+joinStr(only), false)
	}
}

func joinStr(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// ---------- 用例 1c: --exclude 随机模式 ----------
// 排除规则要按"全路径 / 相对打包根 / 任一路径分段"三种方式匹配, 手写的用例
// 只测了一种。让随机树去撞, 断言"匹配上的一定不在、没匹配上的一定在且内容对"。

func TestRandomExcludePack(t *testing.T) {
	rng := rand.New(rand.NewSource(31337))
	for iter := 0; iter < 6; iter++ {
		dir := t.TempDir()
		makeWritableOnCleanup(t, dir)
		src := filepath.Join(dir, "src")
		buildRandomTree(t, rng, src)
		want := scanTree(t, src)
		// 取一个真实存在的扩展名做模式, 保证既排得掉又不至于全排光
		exts := map[string]bool{}
		for k := range want {
			if e := filepath.Ext(k); e != "" {
				exts[e] = true
			}
		}
		var extList []string
		for e := range exts {
			extList = append(extList, e)
		}
		sort.Strings(extList)
		if len(extList) == 0 {
			continue
		}
		pat := "*" + extList[rng.Intn(len(extList))]
		arc := filepath.Join(dir, "a.hcax")
		if runCatchingFatal(func() { pack([]string{src}, arc, "fast", []string{pat}, false) }) {
			t.Fatalf("第%d轮 打包失败", iter)
		}
		exp := map[string]node{}
		for k, v := range want {
			// 模式会跟路径的**任一分段**匹配, 所以一旦某个目录名命中,
			// 它下面整棵子树都不会进归档 —— 期望集必须照这个算, 否则会误报"缺少条目"
			drop := false
			for _, seg := range strings.Split(filepath.ToSlash(k), "/") {
				if ok, _ := filepath.Match(pat, seg); ok {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
			exp[k] = v
		}
		if len(exp) == 0 {
			continue // 这一轮会把整棵树排光, 换个模式再试
		}
		out := filepath.Join(dir, "out")
		if runCatchingFatal(func() { unpack(arc, out, false, nil) }) {
			t.Fatalf("第%d轮 解包失败", iter)
		}
		diffTrees(t, exp, scanTree(t, filepath.Join(out, "src")),
			"iter"+string(rune('0'+iter))+" exclude="+pat)
	}
}

// 源树上共享 inode 的文件, 解包后仍应共享 inode
func checkHardlinks(t *testing.T, want map[string]node, srcRoot, outRoot string) {
	t.Helper()
	byIno := map[uint64][]string{}
	for k, v := range want {
		if v.kind == 'f' && v.ino != 0 {
			byIno[v.ino] = append(byIno[v.ino], k)
		}
	}
	for ino, names := range byIno {
		if len(names) < 2 {
			continue
		}
		var outIno uint64
		for _, n := range names {
			fi, err := os.Lstat(filepath.Join(outRoot, n))
			if err != nil {
				t.Errorf("硬链接 %s 解包后不存在: %v", n, err)
				continue
			}
			cur := inoOf(fi)
			if outIno == 0 {
				outIno = cur
			} else if cur != outIno {
				t.Errorf("硬链接组(inode %d)解包后不再共享 inode: %v", ino, names)
			}
		}
	}
}

// ---------- 用例 2: 随机损坏 —— 必须"干净失败", 绝不能崩 ----------

func TestRandomCorruptionDoesNotCrash(t *testing.T) {
	rng := rand.New(rand.NewSource(777))
	dir := t.TempDir()
	makeWritableOnCleanup(t, dir)
	src := filepath.Join(dir, "src")
	buildRandomTree(t, rng, src)
	arc := filepath.Join(dir, "a.hcax")
	pack([]string{src}, arc, "best", nil, false)
	orig, err := os.ReadFile(arc)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) < 200 {
		t.Skip("归档太小, 损坏测试没意义")
	}
	var nFatal, nClean int
	for i := 0; i < 40; i++ {
		bad := append([]byte{}, orig...)
		switch rng.Intn(3) {
		case 0: // 翻一个字节
			bad[rng.Intn(len(bad))] ^= byte(1 << rng.Intn(8))
		case 1: // 截断
			bad = bad[:1+rng.Intn(len(bad)-1)]
		default: // 抹掉一段
			off := rng.Intn(len(bad))
			n := 1 + rng.Intn(64)
			for j := off; j < off+n && j < len(bad); j++ {
				bad[j] = 0
			}
		}
		p := filepath.Join(dir, "bad.hcax")
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "badout")
		os.RemoveAll(out)
		// 只要不是 panic 就行: 干净报错(fatal)和"仍然解对了"都算通过。
		// list 不校验数据区(设计如此), 损坏落在数据区时要靠 unpack --verify 才能发现,
		// 所以"被拒绝"以两者之一为准。
		rejected := runCatchingFatal(func() { listArchive(p, false) })
		os.RemoveAll(out)
		if runCatchingFatal(func() { unpack(p, out, true, nil) }) {
			rejected = true
		}
		if rejected {
			nFatal++
		} else {
			nClean++
		}
	}
	t.Logf("40 个损坏归档: %d 个被干净拒绝, %d 个改动落在无害位置(仍可读)", nFatal, nClean)
	// 反过来自检: 如果一次都没拒绝, 说明这个测试根本没起作用(别让它变成摆设)
	if nFatal == 0 {
		t.Error("40 次随机损坏一次都没被检测出来 —— 损坏检测可能形同虚设")
	}
}

// ---------- 写盘失败必须被报告, 不能静默产出截断文件 ----------

func TestMustWriteReportsError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r.Close() // 读端关掉 -> 写会返回 EPIPE(Go 对非 std fd 忽略 SIGPIPE)

	if !runCatchingFatal(func() { mustWrite(w, []byte("hello"), "测试") }) {
		t.Error("写入失败却没有报错 —— 磁盘满时会静默产出截断文件")
	}
	if !runCatchingFatal(func() { mustCopy(w, bytes.NewReader([]byte("hello")), "测试") }) {
		t.Error("Copy 失败却没有报错")
	}
	// 空切片不该触发写入(否则每个空文件都会多一次无谓的 Write 调用)
	if runCatchingFatal(func() { mustWrite(w, nil, "测试") }) {
		t.Error("写空内容不该报错")
	}
}
