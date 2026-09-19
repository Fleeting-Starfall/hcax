package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Println("hcax - high-ratio lossless archiver (pure Go: dedup + solid streams + random-access extract + preprocessing + raw storage)")
	fmt.Println("usage:")
	fmt.Println("  hcax pack <out.hcax> <file-or-dir...> -m fast|best|max|ultra|text [--exclude glob]...")
	fmt.Println("    fast   : zstd-3, fastest")
	fmt.Println("    best   : zstd-19 + trained dictionary (similar files) + light preprocessing; fast with good ratio")
	fmt.Println("    max    : lzma2 (system xz, extreme params) + adaptive preprocessing + parallel groups; best all-round")
	fmt.Println("    ultra  : lzma2 whole-file solid (CDC dedup off) + adaptive preprocessing; single-stream extreme ratio")
	fmt.Println("    text   : context mixing (CM) modeling; prediction, not matching; 8~16% better on non-repetitive text/code (pure algorithm, slow)")
	fmt.Println("  --exclude glob: repeatable glob match; excluded if full path / any path segment / basename matches;")
	fmt.Println("                  matching directories are skipped entirely. e.g. --exclude .git --exclude '*.tmp'")
	fmt.Println("  -T/--precise-times: store timestamps in nanoseconds (bumps archive to v10; old binaries cannot read)")
	fmt.Println("  hcax unpack <in.hcax> <out-dir> [--verify]")
	fmt.Println("  hcax extract <in.hcax> <out-dir> [file...] [--verify]")
	fmt.Println("  hcax list <in.hcax> [-l]      # -l/--long: permissions + mtime + type stats")
	fmt.Println("  hcax verify <in.hcax>")
	fmt.Println("  hcax info <in.hcax>   # container layout (version/section sizes/chunk counts)")
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
				// Without this check, `hcax pack out dir -m` panics with index out of range.
				if i+1 >= len(os.Args) {
					fatal("-m needs a mode argument (fast|best|max|ultra|text)")
				}
				mode = os.Args[i+1]
				i++
			case "--exclude":
				if i+1 >= len(os.Args) {
					fatal("--exclude needs a pattern argument")
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
			fatal("pack needs an output and an input")
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
			fatal("unpack needs an input and an output")
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
			fatal("extract needs an input and an output")
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
			fatal("list needs an input")
		}
		listArchive(a, long)
	case "verify":
		if len(os.Args) < 3 {
			fatal("verify needs an input")
		}
		verifyArchive(os.Args[2])
	case "info":
		if len(os.Args) < 3 {
			fatal("info needs an input")
		}
		infoArchive(os.Args[2])
	default:
		usage()
		os.Exit(1)
	}
}
