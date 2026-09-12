#!/usr/bin/env bash
# hcax 性能 / 内存基准脚本
# 对给定输入文件跑全部模式, 输出 压率 / 耗时 / 峰值内存(RSS), 并逐字节校验无损。
# 用法: ./bench.sh [输入文件]   (默认 ../1_副本.txt, 即本机 350MB+ 测试文件)
set -u

IN=${1:-../1_副本.txt}
OUT=/tmp/hcax_bench
rm -rf "$OUT"; mkdir -p "$OUT"

if [ ! -f "$IN" ]; then
  echo "测试文件不存在: $IN" >&2
  exit 1
fi

go build -o hcax . || { echo "build 失败" >&2; exit 1; }

size() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1"; }

BYTES=$(size "$IN")
echo "输入: $IN  ($(numfmt --to=iec $BYTES 2>/dev/null || echo ${BYTES}B) bytes)"
printf "%-8s %12s %12s %9s %9s %10s %s\n" 模式 原始B 归档B 压率 耗时s 峰值内存 无损

for m in fast best max ultra; do
  # text 模式对大文件极慢(逐 bit 建模), 默认跳过; 用 TEXT=1 启用
  if [ "$m" = "text" ] && [ "${TEXT:-0}" != "1" ]; then continue; fi
  tf=$(/usr/bin/time -l ./hcax pack "$OUT/o.hcax" "$IN" -m "$m" 2>/tmp/hcax_bench_t.txt)
  orig=$(echo "$tf" | grep -oE '原始 [0-9]+ B' | grep -oE '[0-9]+')
  arch=$(echo "$tf" | grep -oE -- '-> [0-9]+ B' | grep -oE '[0-9]+')
  pct=$(echo "$tf"  | grep -oE '压率 [0-9.]+%' | grep -oE '[0-9.]+')
  sec=$(grep -E '^[[:space:]]*[0-9.]+ +real' /tmp/hcax_bench_t.txt | awk '{print $1}')
  mem=$(grep -E 'peak memory' /tmp/hcax_bench_t.txt | awk '{printf "%.0fMB", $1/1048576}')
  base=$(basename "$IN")
  ./hcax unpack "$OUT/o.hcax" "$OUT/un" --verify >/dev/null 2>&1
  if cmp -s "$IN" "$OUT/un/$base"; then los="OK"; else los="FAIL"; fi
  printf "%-8s %12s %12s %8s%% %8ss %9s %s\n" "$m" "$orig" "$arch" "$pct" "$sec" "$mem" "$los"
done
