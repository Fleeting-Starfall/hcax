#!/usr/bin/env bash
# hcax perf / memory benchmark script
# runs every mode on a given input file: ratio / time / peak RSS (with byte-exact check).
# usage: ./bench.sh [input file]   (default ../1_copy.txt, the local 350MB+ test file)
set -u

IN=${1:-../1_copy.txt}
OUT=/tmp/hcax_bench
rm -rf "$OUT"; mkdir -p "$OUT"

if [ ! -f "$IN" ]; then
  echo "test file does not exist: $IN" >&2
  exit 1
fi

go build -o hcax . || { echo "build failed" >&2; exit 1; }

size() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1"; }

BYTES=$(size "$IN")
echo "input: $IN  ($(numfmt --to=iec $BYTES 2>/dev/null || echo ${BYTES}B) bytes)"
printf "%-8s %12s %12s %9s %9s %10s %s\n" mode origB archiveB ratio timeS peakRSS lossless

for m in fast best max ultra; do
  # text mode is very slow on large files (bit-by-bit modeling); skip by default; TEXT=1 enables
  if [ "$m" = "text" ] && [ "${TEXT:-0}" != "1" ]; then continue; fi
  tf=$(/usr/bin/time -l ./hcax pack "$OUT/o.hcax" "$IN" -m "$m" 2>/tmp/hcax_bench_t.txt)
  orig=$(echo "$tf" | grep -oE 'raw [0-9]+ B' | grep -oE '[0-9]+')
  arch=$(echo "$tf" | grep -oE -- '-> [0-9]+ B' | grep -oE '[0-9]+')
  pct=$(echo "$tf"  | grep -oE 'ratio [0-9.]+%' | grep -oE '[0-9.]+')
  sec=$(grep -E '^[[:space:]]*[0-9.]+ +real' /tmp/hcax_bench_t.txt | awk '{print $1}')
  mem=$(grep -E 'peak memory' /tmp/hcax_bench_t.txt | awk '{printf "%.0fMB", $1/1048576}')
  base=$(basename "$IN")
  ./hcax unpack "$OUT/o.hcax" "$OUT/un" --verify >/dev/null 2>&1
  if cmp -s "$IN" "$OUT/un/$base"; then los="OK"; else los="FAIL"; fi
  printf "%-8s %12s %12s %8s%% %8ss %9s %s\n" "$m" "$orig" "$arch" "$pct" "$sec" "$mem" "$los"
done
