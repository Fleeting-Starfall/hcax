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

# 目录的权限与 mtime: 边解边设会被"随后写入子文件"冲成当前时间,
# 提前 chmod 成只读(0555)更会让子文件根本写不进去 —— 必须推到最后统一补
mkdir -p dirmeta/ro
printf 'inside\n' > dirmeta/ro/f.txt
chmod 0555 dirmeta/ro
"$PY" -c "import os; os.utime('dirmeta/ro', (1577836800, 1577836800))"
rm -f dirmeta.hcax; rm -rf r_dirmeta
"$BIN" pack dirmeta.hcax dirmeta -m fast >/dev/null 2>&1
"$BIN" unpack dirmeta.hcax r_dirmeta >/dev/null 2>&1
if [ -f r_dirmeta/dirmeta/ro/f.txt ] && cmp -s dirmeta/ro/f.txt r_dirmeta/dirmeta/ro/f.txt; then
  ok "只读目录(0555)里的文件仍能解出"; else bad "只读目录里的文件没解出来"; fi
if stat -f '%Sp' r_dirmeta/dirmeta/ro 2>/dev/null | grep -q 'xr-xr-x'; then
  ok "目录权限还原(0555)"; else bad "目录权限未还原"; fi
dmt_src=$("$PY" -c "import os;print(int(os.stat('dirmeta/ro').st_mtime))")
dmt_out=$("$PY" -c "import os;print(int(os.stat('r_dirmeta/dirmeta/ro').st_mtime))")
if [ "$dmt_src" = "$dmt_out" ]; then
  ok "目录 mtime 还原(不再全部变成解包时刻)"; else bad "目录 mtime 丢失: $dmt_src -> $dmt_out"; fi
chmod 755 dirmeta/ro

# 打包可复现: 同样的输入必须得到**逐字节相同**的归档。
# 归档里到处是 map(去重表/硬链接表/字典), 只要有一处按 map 遍历顺序写出去,
# 同一个命令跑两遍就会得到两个不同的文件 —— 备份类工具里这很要命(增量/比对全废)。
rm -f det1.hcax det2.hcax
for dm in fast max text; do
  rm -f det1.hcax det2.hcax
  "$BIN" pack det1.hcax corpus -m "$dm" >/dev/null 2>&1
  "$BIN" pack det2.hcax corpus -m "$dm" >/dev/null 2>&1
  if cmp -s det1.hcax det2.hcax; then
    ok "[$dm] 同输入两次打包逐字节一致(不依赖 map 遍历顺序)"
  else
    bad "[$dm] 两次打包结果不同(不可复现)"
  fi
done

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
# 注意用 tree 语料(纯 JSON + 随机块)而不是 corpus —— corpus 里有位图, 会正当升 v12
if [ "$(verbyte o_max_tree.hcax)" = "8" ]; then ok "普通归档仍写 v8(不无故与旧版绝缘)"; else bad "普通归档版本异常"; fi
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

# 硬链接(v11): 同一 inode 的多个名字只存一份内容, 解包后重新共享 inode
mkdir -p hardl
head -c 200000 /dev/urandom > hardl/orig.bin
if ln hardl/orig.bin hardl/a.bin 2>/dev/null && ln hardl/orig.bin hardl/b.bin 2>/dev/null; then
  if "$BIN" pack hardl.hcax hardl -m fast >/dev/null 2>&1; then ok "硬链接可打包"; else bad "硬链接打包失败"; fi
  if [ "$(verbyte hardl.hcax)" = "11" ]; then ok "硬链接归档升 v11"; else bad "硬链接未升 v11"; fi
  # 内容只存一份: 200KB×3 = 600KB, 归档必须明显小于 300KB
  if [ "$(stat -f%z hardl.hcax 2>/dev/null || stat -c%s hardl.hcax)" -lt 300000 ]; then
    ok "硬链接内容只存一份"; else bad "硬链接内容被重复存储"; fi
  rm -rf hardl_out
  if "$BIN" unpack hardl.hcax hardl_out >/dev/null 2>&1; then
    i1=$("$PY" -c "import os; print(os.stat('hardl_out/hardl/orig.bin').st_ino)")
    i2=$("$PY" -c "import os; print(os.stat('hardl_out/hardl/a.bin').st_ino)")
    if [ -n "$i1" ] && [ "$i1" = "$i2" ]; then ok "硬链接还原为共享 inode"; else bad "硬链接未还原"; fi
    if cmp -s hardl/orig.bin hardl_out/hardl/b.bin; then ok "硬链接内容一致"; else bad "硬链接内容不一致"; fi
  else
    bad "硬链接解包失败"
  fi
  # 只抽硬链接本身(源不在解包范围内)时必须退化为内容完整的普通文件
  rm -rf hardl_x
  if "$BIN" extract hardl.hcax hardl_x hardl/b.bin >/dev/null 2>&1 && \
     cmp -s hardl/orig.bin hardl_x/hardl/b.bin; then
    ok "只抽硬链接时退化为完整文件"; else bad "只抽硬链接时内容缺失"; fi
else
  printf '  \033[33mSKIP\033[0m 当前文件系统不支持硬链接\n'
fi

# 解包进度: 大归档要有反馈(打包侧早就有, 解包侧一直缺), 小归档不该有噪音
mkdir -p progb && head -c 35000000 /dev/zero > progb/z.bin
if "$BIN" pack progb.hcax progb -m fast >/dev/null 2>&1; then ok "进度用大语料打包"; else bad "进度用大语料打包失败"; fi
rm -rf progb_out
if "$BIN" unpack progb.hcax progb_out 2>&1 >/dev/null | grep -q '解包中'; then
  ok "大归档解包有进度反馈"; else bad "大归档解包无进度"; fi
rm -rf progb_out2
if "$BIN" unpack edge.hcax progb_out2 2>&1 >/dev/null | grep -q '解包中'; then
  bad "小归档也打进度(噪音)"; else ok "小归档不打进度"; fi

# 高内部重复度的文件: 引用块数 >> 唯一块数。
# 旧代码拿"文件引用的块数"和"块表条目数"比大小, 认定引用数不可能超过唯一块数 ——
# 而去重的意义恰恰就是多次引用同一块。结果这类归档打得开、却再也解不开(被自己锁死)。
mkdir -p repd
"$PY" -c "
blk = b'hcax' * 16384          # 64KB 一段
with open('repd/r.bin', 'wb') as f:
    for _ in range(200):
        f.write(blk)
"
if "$BIN" pack repd.hcax repd -m fast >/dev/null 2>&1; then ok "高重复度文件可打包"; else bad "高重复度打包失败"; fi
if "$BIN" list repd.hcax >/dev/null 2>&1; then
  ok "高重复度归档可读取(引用数 > 唯一块数)"; else bad "高重复度归档被误判为损坏"; fi
rm -rf repd_out
if "$BIN" unpack repd.hcax repd_out >/dev/null 2>&1 && cmp -s repd/r.bin repd_out/repd/r.bin; then
  ok "高重复度往返一致"; else bad "高重复度往返不一致"; fi

# 归档文件自身不能进归档: `hcax pack arch.hcax .` 老实现会把上一次的归档也收进去,
# 于是每打一次包就胖一圈(154 -> 278 -> 389...), 还把上一版内容当新数据存下来
mkdir -p selfd && printf 'data\n' > selfd/a.txt
sz() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1"; }
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s1=$(sz selfd/arch.hcax)
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s2=$(sz selfd/arch.hcax)
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s3=$(sz selfd/arch.hcax)
if [ -n "$s1" ] && [ "$s1" = "$s2" ] && [ "$s2" = "$s3" ]; then
  ok "归档自身不打进自己(重复打包大小稳定: $s1 B)"; else bad "归档自我包含: $s1 -> $s2 -> $s3"; fi
if "$BIN" list selfd/arch.hcax 2>/dev/null | grep -q 'arch\.hcax'; then
  bad "归档里含有自己"; else ok "归档内不含自身"; fi

# 输入重复/重叠: 老实现会存出重复条目, 解包时后者静默覆盖前者
mkdir -p dupd && printf 'x\n' > dupd/f.txt
"$BIN" pack dupd.hcax dupd dupd -m fast >/dev/null 2>&1
if [ "$("$BIN" list dupd.hcax 2>/dev/null | grep -c 'dupd/f.txt')" = "1" ]; then
  ok "重复输入同一目录不再产生重复条目"; else bad "重复输入产生了重复条目"; fi
"$BIN" pack dupd2.hcax dupd ./dupd dupd/ -m fast >/dev/null 2>&1
if [ "$("$BIN" list dupd2.hcax 2>/dev/null | grep -c 'dupd/f.txt')" = "1" ]; then
  ok "./dup 与 dup 与 dup/ 视为同一输入"; else bad "等价路径未归一"; fi
# 目录 + 目录内文件: 归档内名字重叠
"$BIN" pack dupd3.hcax dupd dupd/f.txt -m fast 2>/dev/null | grep -q '重复条目' && \
if "$BIN" list dupd3.hcax 2>/dev/null | grep -q 'dupd/f.txt'; then
  ok "输入重叠时提示并保留一份"; else bad "输入重叠处理异常"; fi

# 命令行参数缺值: -m 后面不给值曾经直接 index out of range 崩掉
if "$BIN" pack needarg.hcax edge -m 2>&1 | grep -qE '需要一个模式参数|错误'; then
  ok "-m 缺参数时明确报错(不崩)"; else bad "-m 缺参数时崩溃或提示含糊"; fi
if "$BIN" pack needarg.hcax edge --exclude 2>&1 | grep -qE '需要一个模式参数|错误'; then
  ok "--exclude 缺参数时明确报错"; else bad "--exclude 缺参数处理异常"; fi
if "$BIN" pack onlyout.hcax 2>&1 | grep -q '需要 输出 和 输入'; then
  ok "缺输入时提示用法"; else bad "缺输入时提示含糊"; fi

# 解包到非空目录: 会静默改写同名条目(与 tar 同), 至少要让人知道改了几个
rm -rf ovw && "$BIN" unpack o_max_corpus.hcax ovw >/dev/null 2>&1
if "$BIN" unpack o_max_corpus.hcax ovw 2>/dev/null | grep -q '覆盖 [0-9]* 个已存在条目'; then
  ok "重复解包提示覆盖了几个已存在条目"; else bad "重复解包未提示覆盖"; fi
rm -rf ovw2
if "$BIN" unpack o_max_corpus.hcax ovw2 2>/dev/null | grep -q '覆盖'; then
  bad "首次解包到空目录也报覆盖(误报)"; else ok "首次解包不报覆盖"; fi

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

# 原样区(不可压块)单独有流级哈希: 只测"数据区中间"可能一直落在固实流里,
# 原样区那条校验路径就永远没人跑过。这里专门造一个有原样区的归档来测。
mkdir -p rawcorp
head -c 300000 /dev/urandom > rawcorp/rand.bin
printf 'compressible text %.0s' $(seq 1 3000) > rawcorp/txt.txt
rm -f rawcorp.hcax
"$BIN" pack rawcorp.hcax rawcorp -m max >/dev/null 2>&1
if "$BIN" info rawcorp.hcax 2>/dev/null | grep -q '原样区'; then
  ok "max 档确有原样区(不可压块走原样存储)"
else
  bad "造不出原样区, 下面的原样区校验用例是空转的"
fi
"$PY" - "$PWD/rawcorp.hcax" <<'PYX'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
d[-40] ^= 0xFF          # 尾部魔数(20B)与字典区之前 —— 一定是原样区
open(p + '.bad', 'wb').write(bytes(d))
PYX
if "$BIN" verify rawcorp.hcax.bad >/dev/null 2>&1; then
  bad "原样区被篡改后 verify 仍通过"; else ok "原样区篡改被 verify 捕获"; fi
# list 只读元数据、不读数据区, 所以"list 正常"不等于数据完好 —— 把这点钉住,
# 免得以后有人拿 list 当校验用
if "$BIN" list rawcorp.hcax.bad >/dev/null 2>&1; then
  ok "list 不读数据区(损坏仍能列出, 故不能当校验用)"
else
  bad "list 读了数据区(预期: 只看元数据)"
fi
rm -rf rcbad
if "$BIN" unpack rawcorp.hcax.bad rcbad --verify >/dev/null 2>&1; then
  bad "unpack --verify 放过了损坏的原样区"; else ok "unpack --verify 拦住损坏的原样区"; fi

# 元数据区(压缩帧)损坏: 元数据是先解压再解析的, 解压结果长度与声明不符必须报错
"$PY" - "$PWD/rawcorp.hcax" <<'PYX'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
d[60] ^= 0xFF           # 头部 48B 之后即压缩的元数据帧
open(p + '.meta', 'wb').write(bytes(d))
PYX
if "$BIN" list rawcorp.hcax.meta >/dev/null 2>&1; then
  bad "元数据区损坏被接受"; else ok "元数据区损坏被拒绝"; fi

# 最表层的两个字节: magic 与版本号
"$PY" - "$PWD/rawcorp.hcax" <<'PYX'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
d[0] = 0x41
open(p + '.magic', 'wb').write(bytes(d))
d2 = bytearray(open(p, 'rb').read())
d2[4] = 99
open(p + '.ver', 'wb').write(bytes(d2))
PYX
if "$BIN" list rawcorp.hcax.magic 2>&1 | grep -q '不是 HCAX'; then
  ok "magic 不对时报'不是 HCAX 文件'"; else bad "magic 校验异常"; fi
if "$BIN" list rawcorp.hcax.ver 2>&1 | grep -q '版本不兼容'; then
  ok "版本号过高时报'版本不兼容'"; else bad "版本号校验异常"; fi

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

# 光栅图(BMP)端到端: 24bpp 宽 101 的行跨距 304 不是 3 的倍数,
# 逐行变换(8/9 号)就是为它加的; 一旦退化成"整块每 3 字节一组", 压率会掉 6%
if [ -f corpus/photo24.bmp ]; then
  if cmp -s corpus/photo24.bmp r_best_corpus/corpus/photo24.bmp; then
    ok "24bpp BMP(带行填充)往返逐字节一致"
  else
    bad "24bpp BMP 往返损坏"
  fi
  if cmp -s corpus/photo32.bmp r_best_corpus/corpus/photo32.bmp; then
    ok "32bpp BMP(无行填充)往返逐字节一致"
  else
    bad "32bpp BMP 往返损坏"
  fi
  # 含逐行光栅变换 -> v12; 不含的普通归档仍应是 v8(防止"一律升版本"的回归)
  if "$BIN" info o_best_corpus.hcax 2>/dev/null | grep -q 'v12'; then
    ok "含逐行光栅变换的归档写 v12"; else bad "含逐行光栅变换却没升 v12"; fi
  rm -f norast.hcax
  mkdir -p norast && printf 'plain text only\n' > norast/a.txt
  "$BIN" pack norast.hcax norast -m fast >/dev/null 2>&1
  if "$BIN" info norast.hcax 2>/dev/null | grep -q 'v8'; then
    ok "无光栅文件时仍写 v8(没有无谓升版本)"; else bad "无光栅文件也升了版本"; fi
  # 打包一个"行填充非 3 倍数"的 BMP 后照片体积必须明显小于原始 BMP
  bsz=$(sz corpus/photo24.bmp)
  rm -f bmp1.hcax; "$BIN" pack bmp1.hcax corpus/photo24.bmp -m best >/dev/null 2>&1
  asz=$(sz bmp1.hcax)
  if [ "$asz" -lt "$bsz" ]; then
    ok "24bpp BMP 被压缩($bsz -> $asz B)"
  else
    bad "24bpp BMP 没被压缩($bsz -> $asz B)"
  fi
else
  bad "语料里缺少 photo24.bmp(光栅路径无端到端覆盖)"
fi

# ---------------------------------------------------------------- 汇总
note "汇总: $PASS 通过 / $FAIL 失败"
[ "$FAIL" -eq 0 ]
