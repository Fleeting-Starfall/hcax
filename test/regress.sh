#!/usr/bin/env bash
# hcax 回归测试
#
# 红线(任一失败即视为回归):
#   1. 无损 —— pack → unpack → diff -r 必须逐字节一致
#   2. 格式兼容 —— 老归档必须仍能解开
#   3. 校验有效 —— 篡改 1 字节, verify 必须报错
# 同时记录 压率 / 耗时 / 峰值内存, 用于对比优化是否真的有效。
#
# 用法: ./test/regress.sh [hcax二进制] [工作目录]
set -u

BIN="${1:-./hcax}"
WORK="${2:-/tmp/hcax-regress}"
PY="${PYTHON:-python3}"
# 必须在 cd 之前解析出脚本自身目录(语料生成器与脚本同目录)
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$BIN" in /*) ;; *) BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")" ;; esac

# macOS 的 /usr/bin/time -l 给出峰值 RSS; Linux 用 GNU time -v
TIMER=""
if command -v /usr/bin/time >/dev/null 2>&1 && /usr/bin/time -l true 2>/dev/null; then
  TIMER="/usr/bin/time -l"
elif command -v /usr/bin/time >/dev/null 2>&1; then
  TIMER="/usr/bin/time -v"
fi

PASS=0
FAIL=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
note() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# 峰值内存(字节; macOS 的 maximum resident set size 单位是字节, 不是 KB)
peak_bytes() {
  local log="$1"
  [ -n "$TIMER" ] || return 0
  sed -n 's/^[[:space:]]*\([0-9]*\)[[:space:]]*maximum resident set size.*/\1/p' "$log" | tail -1
}

# 字节 -> MB(整数); 拿不到就显示 ?
mb() {
  if [ -n "${1:-}" ]; then echo $(( $1 / 1048576 )); else echo '?'; fi
}

run_timed() {
  # run_timed <logfile> <cmd...>
  local log="$1"; shift
  if [ -n "$TIMER" ]; then
    $TIMER "$@" >"$log.out" 2>"$log"
  else
    "$@" >"$log.out" 2>&1; : >"$log"
  fi
}

rm -rf "$WORK"; mkdir -p "$WORK"
cd "$WORK" || exit 1

note "0. 准备语料"
"$PY" "$SELF_DIR/mkcorpus.py" "$WORK/corpus" $((12*1024*1024)) >/dev/null || { echo "语料生成失败"; exit 1; }
mkdir -p "$WORK/edge/deep/empty"
: > "$WORK/edge/empty.bin"
printf 'x' > "$WORK/edge/one.bin"
printf 'dots..\n' > "$WORK/edge/a..b.txt"
printf 'hidden\n' > "$WORK/edge/..weird.txt"
head -c 300000 /dev/urandom > "$WORK/edge/deep/rand.bin"
# 符号链接: 指向文件 / 指向目录 / 悬空链接
ln -sf one.bin "$WORK/edge/link_file"
ln -sf deep "$WORK/edge/link_dir"
ln -sf __nope__ "$WORK/edge/link_dangling"
mkdir -p "$WORK/tree/a/b/c" "$WORK/tree/a/empty"
for i in $(seq 1 120); do
  printf '{"id":%d,"name":"item-%d","ok":true}\n' "$i" "$i" > "$WORK/tree/a/b/c/f$i.json"
done
head -c 40000 /dev/urandom > "$WORK/tree/a/b/blob.bin"
ok "语料就绪"

# ---------------------------------------------------------------- 1. 往返无损
note "1. 往返无损 + 压率/耗时/内存"
printf '  %-8s %-10s %10s %9s %8s\n' 模式 语料 压率 耗时 峰值RSS
for mode in fast best max ultra text; do
  for corp in corpus tree; do
    rm -f "o_${mode}_${corp}.hcax"; rm -rf "r_${mode}_${corp}"
    run_timed "log_${mode}_${corp}" "$BIN" pack "o_${mode}_${corp}.hcax" "$corp" -m "$mode"
    ratio=$(sed -n 's/.*压率 \([0-9.]*\)%.*/\1/p' "log_${mode}_${corp}.out")
    wall=$(sed -n 's/^[[:space:]]*\([0-9.]*\)[[:space:]]*real.*/\1/p' "log_${mode}_${corp}" | tail -1)
    pk=$(peak_bytes "log_${mode}_${corp}")
    run_timed "logu_${mode}_${corp}" "$BIN" unpack "o_${mode}_${corp}.hcax" "r_${mode}_${corp}"
    # 打包目录时归档内保留目录名(tar/zip 语义), 故解包结果在 r_X/<dirname>/ 下
    if diff -r "$corp" "r_${mode}_${corp}/$corp" >/dev/null 2>&1; then
      printf '  %-8s %-10s %9s%% %8ss %7sMB  ✓无损\n' \
        "$mode" "$corp" "$ratio" "${wall:-?}" "$(mb "$pk")"
      PASS=$((PASS+1))
    else
      printf '  %-8s %-10s %9s%% %8ss %7sMB  ✗有差异\n' \
        "$mode" "$corp" "$ratio" "${wall:-?}" "$(mb "$pk")"
      FAIL=$((FAIL+1))
    fi
  done
done

# ---------------------------------------------------------------- 2. 边界
note "2. 边界用例"
rm -f edge.hcax; rm -rf r_edge
if "$BIN" pack edge.hcax edge -m best >/dev/null 2>&1; then ok "边界语料打包(空文件/1字节/..文件名/嵌套空目录)"; else bad "边界语料打包"; fi
if "$BIN" unpack edge.hcax r_edge >/dev/null 2>&1; then ok "边界语料解包"; else bad "边界语料解包"; fi
if diff -r edge r_edge/edge >/dev/null 2>&1; then ok "边界语料逐字节一致"; else bad "边界语料不一致"; fi
if [ -e r_edge/edge/a..b.txt ] && [ -e r_edge/edge/..weird.txt ]; then ok "含双点的合法文件名不再被误杀"; else bad "含双点文件名仍被误杀"; fi
if [ -d r_edge/edge/deep/empty ]; then ok "空目录保留"; else bad "空目录丢失"; fi
if "$BIN" list edge.hcax 2>/dev/null | grep -q 'empty.bin'; then ok "空文件已记录"; else bad "空文件丢失"; fi
# 嵌套目录不得被拍平(旧实现: 文件全在子目录时, 上层目录名会丢失)
if "$BIN" list edge.hcax 2>/dev/null | grep -q '^.*  edge/deep/rand.bin'; then ok "嵌套路径完整(未被拍平)"; else bad "嵌套路径被拍平"; fi
# 符号链接: 旧实现遇到"指向目录的链接"会直接打包失败, 指向文件的则存成内容副本
if [ -L r_edge/edge/link_file ] && [ -L r_edge/edge/link_dir ] && [ -L r_edge/edge/link_dangling ]; then
  ok "符号链接还原为链接(含指向目录/悬空)"
else
  bad "符号链接未正确还原"
fi
if [ "$(readlink r_edge/edge/link_file 2>/dev/null)" = "one.bin" ] && \
   [ "$(readlink r_edge/edge/link_dir 2>/dev/null)" = "deep" ]; then
  ok "链接目标正确"; else bad "链接目标错误"
fi
if [ -f r_edge/edge/link_file ] && cmp -s edge/one.bin r_edge/edge/one.bin; then
  ok "链接指向的内容可正常访问"; else bad "链接内容异常"
fi

# ---------------------------------------------------------------- 3. 命令
note "3. 命令行为"
A=o_max_corpus.hcax
if "$BIN" list "$A" >/dev/null 2>&1; then ok "list"; else bad "list"; fi
if "$BIN" verify "$A" >/dev/null 2>&1; then ok "verify"; else bad "verify"; fi
rm -rf rx
if "$BIN" extract "$A" rx novel.txt >/dev/null 2>&1 && cmp -s corpus/novel.txt rx/corpus/novel.txt; then ok "extract 单文件(内容一致)"; else bad "extract 单文件"; fi
rm -rf rx2
if "$BIN" extract "$A" rx2 novel.txt records.json >/dev/null 2>&1 && \
   cmp -s corpus/novel.txt rx2/corpus/novel.txt && cmp -s corpus/records.json rx2/corpus/records.json; then
  ok "extract 多文件"; else bad "extract 多文件"; fi
if "$BIN" unpack "$A" r_verify --verify >/dev/null 2>&1; then ok "unpack --verify"; else bad "unpack --verify"; fi
# list: 按路径排序(打包顺序是 文件/目录/链接 三组, 直接打印是乱的) + 分类统计
if "$BIN" list "$A" 2>/dev/null | sed -n '1,/^共/p' | grep -E '^  ' | awk '{print $2}' | sort -c 2>/dev/null; then
  ok "list 按路径排序输出"; else bad "list 未按路径排序"; fi
if "$BIN" list "$A" 2>/dev/null | grep -qE '条目 \([0-9]+ 文件 / [0-9]+ 目录 / [0-9]+ 链接\)'; then
  ok "list 分类统计"; else bad "list 分类统计缺失"; fi
if "$BIN" list edge.hcax -l 2>/dev/null | grep -qE 'L.*link_file -> one\.bin'; then
  ok "list -l 显示链接目标"; else bad "list -l 链接目标"; fi
if "$BIN" list edge.hcax -l 2>/dev/null | grep -qE '^  d.*edge/deep/$'; then
  ok "list -l 目录带类型位与斜杠"; else bad "list -l 目录"; fi
# extract 给目录名: 应抽出整棵子树(旧实现只建出一个空目录, 里面的文件一个都没出来)
rm -rf rx3
if "$BIN" extract edge.hcax rx3 edge/deep >/dev/null 2>&1 && \
   cmp -s edge/deep/rand.bin rx3/edge/deep/rand.bin; then
  ok "extract 目录前缀抽出整棵子树"; else bad "extract 目录前缀"; fi
# 抽不到的名字必须明确报错: 静默"抽取完成: 0 文件"会让人误以为拿到了数据
rm -rf rx4
if "$BIN" extract edge.hcax rx4 definitely-missing 2>&1 | grep -qE '没有匹配'; then
  ok "extract 落空时明确报错"; else bad "extract 落空时静默成功"; fi
# 部分落空: 命中的照常抽出, 落空的给警告(不因一条拼错就整次报销)
rm -rf rx5
if "$BIN" extract edge.hcax rx5 edge/deep/rand.bin nope 2>&1 | grep -qE '警告.*没有匹配' && \
   cmp -s edge/deep/rand.bin rx5/edge/deep/rand.bin; then
  ok "extract 部分落空: 警告但仍抽出命中的"; else bad "extract 部分落空处理"; fi

# --exclude: 命中目录 -> 整棵子树跳过; 命中基名 -> 全局生效; 模式按"归档内看到的路径"写
mkdir -p exc/.git/objects exc/node_modules/pkg exc/src exc/build
printf 'g\n' > exc/.git/objects/o.bin
printf 'n\n' > exc/node_modules/pkg/i.js
printf 's\n' > exc/src/a.go
printf 't\n' > exc/src/x.tmp
printf 'b\n' > exc/build/a.o
if "$BIN" pack exc.hcax exc -m fast --exclude .git --exclude node_modules \
    --exclude '*.tmp' --exclude 'build/*' >/dev/null 2>&1; then
  ok "--exclude 打包成功"; else bad "--exclude 打包失败"; fi
L=$("$BIN" list exc.hcax 2>/dev/null)
if printf '%s\n' "$L" | grep -q 'exc/src/a.go' && \
   ! printf '%s\n' "$L" | grep -qE '\.git|node_modules|\.tmp|build/a\.o'; then
  ok "--exclude 三种模式都生效(目录/基名/相对根路径)"; else bad "--exclude 过滤不完整"; fi
if "$BIN" pack exc.hcax exc -m fast --exclude .git 2>/dev/null | grep -q '排除 1 个条目'; then
  ok "--exclude 报告排除数量"; else bad "--exclude 未报告数量"; fi

# v10 元数据: 特殊权限位(sticky/setuid/setgid)与纳秒时间戳
# 老实现用 os.FileMode.Perm(), 只取 0777 —— 这三个位在归档里被静默丢掉
mkdir -p perm
printf 'sticky\n' > perm/st.bin && chmod 1755 perm/st.bin
printf 'ns\n' > perm/ns.bin
"$PY" -c "import os; os.utime('perm/ns.bin', ns=(1700000000123456789, 1700000000123456789))"
verbyte() { "$PY" -c "import sys; print(open(sys.argv[1],'rb').read(8)[4])" "$1"; }
if "$BIN" pack perm.hcax perm -m fast >/dev/null 2>&1; then ok "含 sticky 位可打包"; else bad "含 sticky 打包失败"; fi
if [ "$(verbyte perm.hcax)" = "10" ]; then ok "含特殊权限位自动升 v10"; else bad "特殊权限位未触发 v10"; fi
if [ "$(verbyte o_max_corpus.hcax)" = "8" ]; then ok "普通归档仍写 v8(不无故与旧版绝缘)"; else bad "普通归档版本异常"; fi
rm -rf perm_out
if "$BIN" unpack perm.hcax perm_out >/dev/null 2>&1 && \
   stat -f '%Sp' perm_out/perm/st.bin 2>/dev/null | grep -q 't$'; then
  ok "sticky 位还原"; else bad "sticky 位丢失"; fi
# 亚秒时间戳默认按秒存(与 tar 一致)。注意: 上面那个归档因为有 sticky 已经是 v10,
# 而 v10 一旦启用就会顺带把纳秒存下来 —— 所以这里另建一个"普普通通"的归档来验证。
mkdir -p plain1 && printf 'ns\n' > plain1/ns.bin
"$PY" -c "import os; os.utime('plain1/ns.bin', ns=(1700000000123456789, 1700000000123456789))"
rm -rf plain1_out
if "$BIN" pack plain1.hcax plain1 -m fast >/dev/null 2>&1 && \
   "$BIN" unpack plain1.hcax plain1_out >/dev/null 2>&1 && \
   [ "$("$PY" -c "import os; print(os.stat('plain1_out/plain1/ns.bin').st_mtime_ns)")" = "1700000000000000000" ]; then
  ok "默认按秒存时间戳"; else bad "默认时间戳精度异常"; fi
rm -rf permns_out
if "$BIN" pack permns.hcax perm -m fast -T >/dev/null 2>&1 && \
   "$BIN" unpack permns.hcax permns_out >/dev/null 2>&1 && \
   [ "$("$PY" -c "import os; print(os.stat('permns_out/perm/ns.bin').st_mtime_ns)")" = "1700000000123456789" ]; then
  ok "-T 纳秒时间戳逐位还原"; else bad "-T 纳秒时间戳"; fi

# ---------------------------------------------------------------- 4. 损坏检测
note "4. 损坏检测"
cp "$A" corrupt.hcax
# 翻转数据区中间的 1 字节
SZ=$(wc -c < corrupt.hcax)
OFF=$((SZ/2))
printf '\xff' | dd of=corrupt.hcax bs=1 seek="$OFF" count=1 conv=notrunc >/dev/null 2>&1
if "$BIN" verify corrupt.hcax >/dev/null 2>&1; then bad "篡改后 verify 仍通过(无法发现损坏)"; else ok "篡改 1 字节被 verify 捕获"; fi
# 截断: 老实现只写 trailer 从不校验, 被截断的归档会一路解下去甚至静默解出部分数据
cp "$A" trunc.hcax
TSZ=$(wc -c < trunc.hcax | tr -d ' ')
: | dd of=trunc.hcax bs=1 seek=$((TSZ-30)) count=0 conv=notrunc >/dev/null 2>&1
head -c $((TSZ-30)) "$A" > trunc.hcax
if "$BIN" list trunc.hcax >/dev/null 2>&1; then bad "截断的归档被接受了(应报错)"; else ok "截断的归档被拒绝"; fi
if "$BIN" list trunc.hcax 2>&1 | grep -qE '截断|尾部'; then ok "截断报错信息明确"; else bad "截断报错信息含糊"; fi

# ---------------------------------------------------------------- 5. 资源

# 损坏的头部: 长度字段/条目数被改成荒谬值, 必须明确报错而不是 OOM 或 panic
cp "$A" badhdr.hcax
python3 - "$PWD/badhdr.hcax" <<'PYX'
import struct,sys
p=sys.argv[1]
d=bytearray(open(p,'rb').read())
struct.pack_into('<I', d, 40, 0xFFFFFFF0)   # nChunks 荒谬
open(p,'wb').write(bytes(d))
PYX
if "$BIN" list badhdr.hcax >/dev/null 2>&1; then bad "荒谬 nChunks 被接受"; else ok "荒谬 nChunks 被拒绝"; fi
if "$BIN" list badhdr.hcax 2>&1 | grep -qE '损坏|异常'; then ok "头部损坏报错明确"; else bad "头部损坏报错含糊"; fi


# 全空内容(只有空目录与空文件): 老实现压率除零输出 +Inf%
mkdir -p emptyonly/sub
: > emptyonly/zero.bin
: > emptyonly/sub/zero2.bin
if "$BIN" pack eo.hcax emptyonly -m best >/dev/null 2>&1; then ok "全空内容可打包"; else bad "全空内容打包失败"; fi
if "$BIN" pack eo.hcax emptyonly -m best 2>/dev/null | grep -q 'Inf\|NaN'; then bad "全空内容压率出现 Inf/NaN"; else ok "全空内容压率不出现 Inf/NaN"; fi
rm -rf eo_out
if "$BIN" unpack eo.hcax eo_out >/dev/null 2>&1 && [ -d eo_out/emptyonly/sub ] && [ -f eo_out/emptyonly/zero.bin ]; then
  ok "全空内容可解包(目录与空文件都在)"; else bad "全空内容解包异常"; fi


# 健壮性: 目录里有无权限文件或 FIFO 时, 旧版要么整个失败, 要么在 os.Open(FIFO) 处永久阻塞
mkdir -p robo
printf 'good data\n' > robo/ok.txt
printf 'secret\n' > robo/locked.txt
chmod 000 robo/locked.txt
if command -v mkfifo >/dev/null 2>&1; then mkfifo robo/pipe 2>/dev/null; fi
# 不用 timeout: macOS 默认没有 GNU coreutils 的 timeout
if "$BIN" pack robo.hcax robo -m fast >/dev/null 2>&1; then
  ok "含无权限文件/FIFO 仍能完成打包(不失败、不卡死)"
else
  bad "含无权限文件/FIFO 时打包失败或卡死"
fi
if "$BIN" list robo.hcax 2>/dev/null | grep -q 'robo/ok.txt'; then ok "可读文件仍被归档"; else bad "可读文件丢失"; fi
chmod 644 robo/locked.txt

note "5. 资源与卫生"
count_tmp() { find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'hcax-solid-*' -o -maxdepth 1 -name 'hcax-raw-*' 2>/dev/null | wc -l | tr -d ' '; }
before=$(count_tmp)
"$BIN" pack tmpchk.hcax corpus -m max >/dev/null 2>&1
after=$(count_tmp)
if [ "$after" -le "$before" ]; then ok "临时文件无泄漏(前$before 后$after)"; else bad "临时文件泄漏(前$before 后$after)"; fi

# 失败路径: 第一个文件已写入临时文件, 第二个文件打不开 -> 在临时文件建立**之后**失败
# (旧实现用 defer os.Remove, 而 fatal() 走 os.Exit, defer 不执行 -> 泄漏)
mkdir -p leakdir
printf 'some data to pack first\n' > leakdir/ok.txt
printf 'unreadable\n' > leakdir/no.txt
chmod 000 leakdir/no.txt
"$BIN" pack never.hcax leakdir -m best >/dev/null 2>&1
after2=$(count_tmp)
if [ "$after2" -le "$before" ]; then ok "失败路径也无临时文件泄漏(前$before 后$after2)"; else bad "失败路径泄漏临时文件(前$before 后$after2)"; fi
chmod 644 leakdir/no.txt

# ---------------------------------------------------------------- 汇总
note "汇总: $PASS 通过 / $FAIL 失败"
[ "$FAIL" -eq 0 ]
