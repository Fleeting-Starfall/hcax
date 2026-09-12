package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Println("hcax - 极高压缩率无损归档 (Go 原生, 去重+流式+随机解压+预处理+原样存储)")
	fmt.Println("用法:")
	fmt.Println("  hcax pack   输出.hcax 文件/目录... -m fast|best|max|ultra|text")
	fmt.Println("    fast   : zstd-3, 最快")
	fmt.Println("    best   : zstd-19 + 训练字典(相似文件) + 轻量预处理, 快且压率好")
	fmt.Println("    max    : lzma2(系统xz极限参数) + 自适应预处理 + 并行分组(-mmt), 压率/速度均衡")
	fmt.Println("    ultra  : lzma2 整文件固实(关CDC去重) + 自适应预处理, 单线程求最低压率")
	fmt.Println("    text   : 上下文混合(CM)建模, 不靠'重复'而靠'预测', 对非重复文本/代码再省 8~16%(纯算法, 较慢)")
	fmt.Println("  hcax unpack 输入.hcax 输出目录 [--verify]")
	fmt.Println("  hcax extract 输入.hcax 输出目录 [文件...] [--verify]")
	fmt.Println("  hcax list   输入.hcax")
	fmt.Println("  hcax verify 输入.hcax")
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
		for i := 2; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "-m":
				mode = os.Args[i+1]
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
		pack(inputs, out, mode)
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
		if len(os.Args) < 3 {
			fatal("list 需要 输入")
		}
		listArchive(os.Args[2])
	case "verify":
		if len(os.Args) < 3 {
			fatal("verify 需要 输入")
		}
		verifyArchive(os.Args[2])
	default:
		usage()
		os.Exit(1)
	}
}
