#!/usr/bin/env bash
# bench.sh - reproducible benchmark regenerating the README sec.8 "speed/memory" table
#
# usage:
#   bash test/bench.sh [corpus dir] [iterations]
#
# default corpus: the standard mixed corpus from test/mkcorpus.py (12,582,539 B, 7 files).
#   build: python3 test/mkcorpus.py /tmp/hcaxbench/corpus
#   mkcorpus.py uses a fixed seed (random.Random(20260912)), so the corpus is byte-reproducible.
#
# why a dedicated script:
#   the README speed numbers were previously a one-off run; max (5.88s) looked slower than
#   ultra (3.84s) yet produced identical sizes -- impossible; re-measuring showed the box was
#   busy then (load average 6.6). A single wall-clock sample is untrustworthy.
#
# hence this script:
#   1. run each item N times, take the **minimum**. Min over median: other load only
#       makes results worse, so min best approximates unloaded time (median keeps the load).
#   2. report both wall and CPU time (user+sys) -- neither substitutes the other:
#        . wall = what the user actually waits, but inflated by machine load;
#        . CPU  = total CPU consumed by this process (incl. children), immune to load,
#                yet counts max's "two xz in parallel to pick params" as time (so max CPU
#                is ~2x its wall -- not slower, just using two cores at once).
#   3. also run one round-trip check so we are not benchmarking a broken build.
#
# note: macOS has no GNU time. /usr/bin/time -l maximum resident set size
# is **bytes** (not KB); do not divide by 1024 like on Linux.

set -u

CORPUS="${1:-/tmp/hcaxbench/corpus}"
N="${2:-5}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$HERE/hcax"
WORK="${WORK:-/tmp/hcaxbench/run}"
MODES="fast best max ultra text"

if [ ! -d "$CORPUS" ]; then
	echo "corpus dir does not exist: $CORPUS" >&2
	echo "generate it first: python3 $HERE/test/mkcorpus.py $CORPUS" >&2
	exit 1
fi
if [ ! -x "$BIN" ]; then
	echo "build it first: (cd $HERE && go build -o hcax .)" >&2
	exit 1
fi

rm -rf "$WORK"; mkdir -p "$WORK"
SIZE=$(find "$CORPUS" -type f -exec ls -l {} \; | awk '{s+=$5} END{print s}')
NFILES=$(find "$CORPUS" -type f | wc -l | tr -d ' ')
# unpacking an archive of a dir input adds one level: the packed dir's basename
BASE=$(basename "$CORPUS")

echo "# corpus: $CORPUS  ($NFILES files, $SIZE bytes = $(echo "scale=2; $SIZE/1048576" | bc) MiB)"
echo "# each item: min of $N runs (wall / CPU); memory: median"
# system xz on PATH decides max/ultra results (26.89% with / 33.40% without);
# without stating it in the header, another machine re-runs give incomparable numbers.
if command -v xz >/dev/null 2>&1; then
	echo "# system xz: present ($(command -v xz)) -- max/ultra use system lzma2"
else
	echo "# system xz: absent -- max/ultra fall back to pure Go; ratios not comparable"
fi
echo

# /usr/bin/time -l output looks like:
#         0.13 real         0.13 user         0.01 sys
#            109510656  maximum resident set size
# fields: "0.13 real  0.13 user  0.01 sys" -> $1=real $3=user $5=sys
cpu_of() { awk '/real/{print $3+$5}'; }        # user + sys
wall_of() { awk '/real/{print $1}'; }          # wall time
rss_of() { awk '/maximum resident/{print $1}'; }

median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'; }
# times: min (see header); memory: median (load-independent, stabler)
best() { printf '%s\n' "$@" | sort -n | head -1; }

printf "%-6s %16s %16s %12s %12s\n" "mode" "pack s(wall/CPU)" "unpack s(wall/CPU)" "peak RSS(MB)" "archive(bytes)"
printf "%-6s %16s %16s %12s %12s\n" "----" "---------------" "---------------" "-----------" "----------"

for m in $MODES; do
	arc="$WORK/$m.hcax"
	pc=""; pw=""; uc=""; uw=""; pr=""
	for i in $(seq 1 "$N"); do
		o=$(/usr/bin/time -l "$BIN" pack "$arc" "$CORPUS" -m "$m" 2>&1)
		pc="$pc $(printf '%s\n' "$o" | cpu_of)"
		pw="$pw $(printf '%s\n' "$o" | wall_of)"
		pr="$pr $(printf '%s\n' "$o" | rss_of)"
	done
	for i in $(seq 1 "$N"); do
		rm -rf "$WORK/out"
		o=$(/usr/bin/time -l "$BIN" unpack "$arc" "$WORK/out" 2>&1)
		uc="$uc $(printf '%s\n' "$o" | cpu_of)"
		uw="$uw $(printf '%s\n' "$o" | wall_of)"
	done
	if ! diff -r "$CORPUS" "$WORK/out/$BASE" >/dev/null 2>&1; then
		echo "  !! $m round trip mismatch" >&2
	fi
	pw2=$(best $pw); pc2=$(best $pc)
	uw2=$(best $uw); uc2=$(best $uc)
	mb=$(echo "scale=0; $(median $pr)/1048576" | bc)
	printf "%-6s %8s /%-7s %8s /%-7s %12s %12s\n" \
		"$m" "$pw2" "$pc2" "$uw2" "$uc2" "$mb" "$(ls -l "$arc" | awk '{print $5}')"
done
