package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Println("hcax - 极高压缩率无损归档 (Go 原生, 去重+流式+随机解压+预处理+原样存储)")
	fmt.Println("用法:")
	fmt.Println("  hcax pack   输出.hcax 文件/目录... -m fast|best|max|ultra|text [--exclude 模式]...")
	fmt.Println("    fast   : zstd-3, 最快")
	fmt.Println("    best   : zstd-19 + 训练字典(相似文件) + 轻量预处理, 快且压率好")
	fmt.Println("    max    : lzma2(系统xz极限参数) + 自适应预处理 + 并行分组(-mmt), 压率/速度均衡")
	fmt.Println("    ultra  : lzma2 整文件固实(关CDC去重) + 自适应预处理, 单线程求最低压率")
	fmt.Println("    text   : 上下文混合(CM)建模, 不靠'重复'而靠'预测', 对非重复文本/代码再省 8~16%(纯算法, 较慢)")
	fmt.Println("  --exclude 模式: 可重复, glob 匹配。按完整路径/路径分段/基名 任一命中即排除;")
	fmt.Println("                  命中目录时整棵子树跳过。例: --exclude .git --exclude '*.tmp'")
	fmt.Println("  -T/--precise-times: 时间戳存到纳秒(会把归档升到 v10, 旧版二进制读不了)")
	fmt.Println("  hcax unpack 输入.hcax 输出目录 [--verify]")
	fmt.Println("  hcax extract 输入.hcax 输出目录 [文件...] [--verify]")
	fmt.Println("  hcax list   输入.hcax [-l]      # -l/--long: 权限 + 修改时间 + 分类统计")
	fmt.Println("  hcax verify 输入.hcax")
	fmt.Println("  hcax info   输入.hcax   # 容器布局详情(版本/各区大小/块数)")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	switch cmd {
	case "pack":
		mode := "best"
		var out string
		var inputs []string
		var excl []string
		precise := false
		for i := 2; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "-T", "--precise-times":
				precise = true
			case "-m":
				// 少了这个判断, `hcax pack out dir -m` 会直接 index out of range 崩掉
				if i+1 >= len(os.Args) {
					fatal("-m 需要一个模式参数(fast|best|max|ultra|text)")
				}
				mode = os.Args[i+1]
				i++
			case "--exclude":
				if i+1 >= len(os.Args) {
					fatal("--exclude 需要一个模式参数")
				}
				excl = append(excl, os.Args[i+1])
				i++
			default:
				if out == "" {
					out = os.Args[i]
				} else {
					inputs = append(inputs, os.Args[i])
				}
			}
		}
		if out == "" || len(inputs) == 0 {
			fatal("pack 需要 输出 和 输入")
		}
		pack(inputs, out, mode, excl, precise)
	case "unpack":
		verify := false
		var a, o string
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--verify" {
				verify = true
			} else if a == "" {
				a = os.Args[i]
			} else if o == "" {
				o = os.Args[i]
			}
		}
		if a == "" || o == "" {
			fatal("unpack 需要 输入 输出")
		}
		unpack(a, o, verify, nil)
	case "extract":
		verify := false
		var a, o string
		var files []string
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--verify" {
				verify = true
			} else if a == "" {
				a = os.Args[i]
			} else if o == "" {
				o = os.Args[i]
			} else {
				files = append(files, os.Args[i])
			}
		}
		if a == "" || o == "" {
			fatal("extract 需要 输入 输出")
		}
		unpack(a, o, verify, files)
	case "list":
		long := false
		var a string
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "-l" || os.Args[i] == "--long" {
				long = true
			} else if a == "" {
				a = os.Args[i]
			}
		}
		if a == "" {
			fatal("list 需要 输入")
		}
		listArchive(a, long)
	case "verify":
		if len(os.Args) < 3 {
			fatal("verify 需要 输入")
		}
		verifyArchive(os.Args[2])
	case "info":
		if len(os.Args) < 3 {
			fatal("info 需要 输入")
		}
		infoArchive(os.Args[2])
	default:
		usage()
		os.Exit(1)
	}
}
