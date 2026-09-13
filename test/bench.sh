#!/usr/bin/env bash
# bench.sh - 重算 README §8「速度/内存」表格的可复现基准
#
# 用法:
#   bash test/bench.sh [语料目录] [重复次数]
#
# 默认语料: 用 test/mkcorpus.py 生成的标准混合语料(12,582,539 B, 7 个文件)。
#   生成: python3 test/mkcorpus.py /tmp/hcaxbench/corpus
#   mkcorpus.py 用固定种子(random.Random(20260912)), 所以语料逐字节可复现。
#
# 为什么单独出这个脚本:
#   README 里的速度数字此前是"随手跑一次"记下来的, 其中 max(5.88s) 明显慢于
#   ultra(3.84s) —— 但两者输出大小完全相同, 不合常理; 重测才发现那次机器上有别的
#   负载(load average 6.6)。单次墙钟采样不可信。
#
# 因此本脚本:
#   1. 每项跑 N 次, 取**最小值**。取最小而不是中位数: 同机其他负载只会让结果变差,
#      不会变好, 所以最小值最接近"无干扰"下的真实耗时(中位数会把负载算进去)。
#   2. 墙钟与 CPU 时间(user+sys)都给 —— 两者不可互相替代:
#        · 墙钟 = 用户实际等待的时间, 但会被同机负载抬高;
#        · CPU  = 本进程(含子进程)消耗的 CPU 总和, 不受同机负载影响, 但会把
#                max 档"并发跑两个 xz 选参"的并行度算成耗时(所以 max 的 CPU
#                约为墙钟的 2 倍, 这不是变慢, 是同时在用两个核)。
#   3. 顺带做一次往返一致性校验, 免得"测了个错的"。
#
# 注意: macOS 没有 GNU time。/usr/bin/time -l 的 maximum resident set size
# 单位是**字节**(不是 KB), 别按 Linux 习惯除以 1024 当 KB。

set -u

CORPUS="${1:-/tmp/hcaxbench/corpus}"
N="${2:-5}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$HERE/hcax"
WORK="${WORK:-/tmp/hcaxbench/run}"
MODES="fast best max ultra text"

if [ ! -d "$CORPUS" ]; then
	echo "语料目录不存在: $CORPUS" >&2
	echo "先生成: python3 $HERE/test/mkcorpus.py $CORPUS" >&2
	exit 1
fi
if [ ! -x "$BIN" ]; then
	echo "先编译: (cd $HERE && go build -o hcax .)" >&2
	exit 1
fi

rm -rf "$WORK"; mkdir -p "$WORK"
SIZE=$(find "$CORPUS" -type f -exec ls -l {} \; | awk '{s+=$5} END{print s}')
NFILES=$(find "$CORPUS" -type f | wc -l | tr -d ' ')
# 解包目录输入的归档时, 会多出一层"被打包目录的基名"
BASE=$(basename "$CORPUS")

echo "# 语料: $CORPUS  ($NFILES 个文件, $SIZE 字节 = $(echo "scale=2; $SIZE/1048576" | bc) MiB)"
echo "# 每项跑 $N 次取最小值(墙钟 / CPU); 内存取中位数"
# 系统 xz 在不在 PATH 里, 对 max/ultra 的结果是决定性的(有 26.89% / 无 33.40%),
# 不在表头写清楚的话, 换台机器重跑就会得到完全对不上的数字。
if command -v xz >/dev/null 2>&1; then
	echo "# 系统 xz: 有 ($(command -v xz)) —— max/ultra 走系统 lzma2"
else
	echo "# 系统 xz: 无 —— max/ultra 回退纯 Go 实现, 压率不可与本表对照"
fi
echo

# /usr/bin/time -l 的输出形如:
#         0.13 real         0.13 user         0.01 sys
#            109510656  maximum resident set size
# 字段: "0.13 real  0.13 user  0.01 sys" -> $1=real $3=user $5=sys
cpu_of() { awk '/real/{print $3+$5}'; }        # user + sys
wall_of() { awk '/real/{print $1}'; }          # 墙钟
rss_of() { awk '/maximum resident/{print $1}'; }

median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}'; }
# 耗时取最小值(见文件头说明); 内存取中位数(它不受负载影响, 更稳)
best() { printf '%s\n' "$@" | sort -n | head -1; }

printf "%-6s %16s %16s %12s %12s\n" "模式" "打包s(墙钟/CPU)" "解包s(墙钟/CPU)" "峰值内存(MB)" "归档(字节)"
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
		echo "  !! $m 往返不一致" >&2
	fi
	pw2=$(best $pw); pc2=$(best $pc)
	uw2=$(best $uw); uc2=$(best $uc)
	mb=$(echo "scale=0; $(median $pr)/1048576" | bc)
	printf "%-6s %8s /%-7s %8s /%-7s %12s %12s\n" \
		"$m" "$pw2" "$pc2" "$uw2" "$uc2" "$mb" "$(ls -l "$arc" | awk '{print $5}')"
done
