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
	"path/filepath"
	"sort"
	"testing"
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
				if err := os.WriteFile(p, buf, 0o644); err != nil {
					t.Fatal(err)
				}
				files = append(files, p)
			}
		}
	}
	build(root, 0)
}

// ---------- 目录树比对 ----------

type node struct {
	kind byte // 'f' 文件, 'd' 目录, 'l' 链接
	size int64
	link string
	data []byte
	ino  uint64
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
			out[rel] = node{kind: 'l', link: tgt}
		case fi.IsDir():
			out[rel] = node{kind: 'd'}
		default:
			data, _ := os.ReadFile(p)
			out[rel] = node{kind: 'f', size: fi.Size(), data: data, ino: inoOf(fi)}
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

func diffTrees(t *testing.T, want, got map[string]node, ctx string) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, ok := got[k]
		if !ok {
			t.Errorf("%s: 缺少条目 %s", ctx, k)
			continue
		}
		g := w
		_ = g
		if w.kind != got[k].kind {
			t.Errorf("%s: %s 类型不符 (%c vs %c)", ctx, k, w.kind, got[k].kind)
			continue
		}
		switch w.kind {
		case 'l':
			if w.link != got[k].link {
				t.Errorf("%s: %s 链接目标不符 (%q vs %q)", ctx, k, w.link, got[k].link)
			}
		case 'f':
			if w.size != got[k].size || !bytes.Equal(w.data, got[k].data) {
				t.Errorf("%s: %s 内容不符 (%d vs %d 字节)", ctx, k, w.size, got[k].size)
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
