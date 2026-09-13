#!/usr/bin/env bash
# 一轮改动后的"三轮验证"
#   第1轮 功能回归: 5模式×2语料无损 + 边界 + 命令 + 损坏检测 + 资源卫生
#   第2轮 无损往返: 更大语料 × 更多模式, 逐字节 diff(含抽取/--verify)
#   第3轮 兼容矩阵: 参考版(v8)打的归档当前版本能解; 当前版本打的归档参考版能解
# 用法: bash test/verify3.sh [工作目录]
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
WORK="${1:-/tmp/hcax-verify3}"
PY="${PYTHON:-python3}"
REF="${HCAX_REF:-$HOME/.hcax-ref/hcax-v8}"
BIN="$ROOT/hcax"

PASS=0; FAIL=0
ok(){ printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad(){ printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
note(){ printf '\n\033[1m%s\033[0m\n' "$1"; }

[ -x "$BIN" ] || { echo "缺少 $BIN"; exit 1; }
rm -rf "$WORK"; mkdir -p "$WORK"; cd "$WORK" || exit 1

# ------------------------------------------------------------ 第1轮
note "第1轮 功能回归"
if out=$(PYTHON="$PY" bash "$HERE/regress.sh" "$BIN" "$WORK/w1" 2>&1); then
  echo "$out" | tail -1 | sed 's/^/  /'; PASS=$((PASS+1)); ok "regress.sh 全通过"
else
  echo "$out" | grep FAIL | sed 's/^/  /'; FAIL=$((FAIL+1)); bad "regress.sh 有失败项"
fi

# 跨平台: fileid_unix.go / fileid_windows.go 分了 build tag, 但平时只编 darwin,
# 哪个平台的代码坏了要等别人交叉编译时才发现
xok=1
while read -r goos goarch; do
  if ! (cd "$ROOT" && GOOS="$goos" GOARCH="$goarch" go build -o /dev/null . >/dev/null 2>&1); then
    xok=0; bad "GOOS=$goos GOARCH=$goarch 编译失败"; fi
done <<'XARCH'
windows amd64
linux amd64
XARCH
[ "$xok" -eq 1 ] && ok "windows / linux 交叉编译通过"

# 单元测试(含随机目录树往返 / 随机损坏不崩): 端到端脚本跑一遍要几十秒,
# 而很多 bug(R18 那种"打得开解不开")在毫秒级的用例里就能撞出来
if (cd "$ROOT" && go test ./... >"$WORK/gotest.log" 2>&1); then
  ok "go test 全通过"; else bad "go test 有失败项"; sed 's/^/    /' "$WORK/gotest.log" | tail -20; fi

# ------------------------------------------------------------ 第2轮
note "第2轮 无损往返(较大语料)"
"$PY" "$HERE/mkcorpus.py" "$WORK/c2" $((16*1024*1024)) >/dev/null
mkdir -p "$WORK/c2/nest/deep/deeper"
head -c 800000 /dev/urandom > "$WORK/c2/nest/deep/deeper/blob.bin"
ln -sf novel.txt "$WORK/c2/link"
for m in fast best max ultra; do
  rm -f "a_$m.hcax"; rm -rf "b_$m"
  "$BIN" pack "a_$m.hcax" c2 -m "$m" >/dev/null 2>&1 || { bad "[$m] 打包"; continue; }
  "$BIN" unpack "a_$m.hcax" "b_$m" >/dev/null 2>&1 || { bad "[$m] 解包"; continue; }
  if diff -r c2 "b_$m/c2" >/dev/null 2>&1; then ok "[$m] 往返逐字节一致"; else bad "[$m] 往返不一致"; fi
  "$BIN" verify "a_$m.hcax" >/dev/null 2>&1 && ok "[$m] verify 通过" || bad "[$m] verify"
  "$BIN" unpack "a_$m.hcax" "bv_$m" --verify >/dev/null 2>&1 && ok "[$m] --verify 解包" || bad "[$m] --verify"
done
# 抽取: 抽一个文件必须与源一致
rm -rf x1
if "$BIN" extract a_max.hcax x1 novel.txt >/dev/null 2>&1 && cmp -s c2/novel.txt x1/c2/novel.txt; then
  ok "extract 单文件一致"; else bad "extract 单文件"; fi
rm -rf x2
if "$BIN" extract a_max.hcax x2 novel.txt records.json >/dev/null 2>&1 && \
   cmp -s c2/novel.txt x2/c2/novel.txt && cmp -s c2/records.json x2/c2/records.json; then
  ok "extract 多文件一致"; else bad "extract 多文件"; fi
# 符号链接
if [ -L b_max/c2/link ]; then ok "符号链接还原"; else bad "符号链接未还原"; fi

# ------------------------------------------------------------ 第3轮
note "第3轮 兼容矩阵"
if [ -x "$REF" ]; then
  # 兼容矩阵必须用**不含符号链接**的语料: 含链接会走 v9, 老版本按设计拒绝读取
  "$PY" "$HERE/mkcorpus.py" "$WORK/c3" $((8*1024*1024)) >/dev/null
  rm -f old.hcax new.hcax; rm -rf o1 o2
  "$REF" pack old.hcax c3 -m max >/dev/null 2>&1
  "$BIN" pack new.hcax c3 -m max >/dev/null 2>&1
  if "$BIN" unpack old.hcax o1 >/dev/null 2>&1 && diff -r c3 o1 >/dev/null 2>&1; then
    ok "参考版(v8)归档 → 当前版本可无损解开"; else bad "参考版归档解不开"; fi
  if "$REF" unpack new.hcax o2 >/dev/null 2>&1 && diff -r c3 o2/c3 >/dev/null 2>&1; then
    ok "当前版本归档 → 参考版(v8)可无损解开"; else bad "当前版本归档老版解不开"; fi
  "$BIN" verify old.hcax >/dev/null 2>&1 && ok "当前版本可校验参考版归档" || bad "校验参考版归档"
  # 含链接的归档应当被老版本明确拒绝(而不是解出错误数据)
  if "$REF" list a_max.hcax >/dev/null 2>&1; then
    bad "含链接的 v9 归档被老版接受了(应当拒绝)"; else ok "含链接的 v9 归档被老版明确拒绝"; fi
  # v10/v11 归档必须被老版本明确拒绝, 而不是解出错位的元数据
  mkdir -p pv10 && printf 'x\n' > pv10/s.bin && chmod 1755 pv10/s.bin
  "$BIN" pack pv10.hcax pv10 -m fast >/dev/null 2>&1
  if "$REF" list pv10.hcax >/dev/null 2>&1; then
    bad "v10 归档被老版接受了(应当拒绝)"; else ok "v10 归档被老版明确拒绝"; fi
  mkdir -p pv11 && printf 'y\n' > pv11/o.bin && ln pv11/o.bin pv11/l.bin 2>/dev/null
  if [ -e pv11/l.bin ]; then
    "$BIN" pack pv11.hcax pv11 -m fast >/dev/null 2>&1
    if "$REF" list pv11.hcax >/dev/null 2>&1; then
      bad "v11 归档被老版接受了(应当拒绝)"; else ok "v11(硬链接)归档被老版明确拒绝"; fi
  fi
  # CM(text) 码流必须与参考版逐字节一致: CM 只能做"不改变数值路径"的实现优化,
  # 一旦改了模型/混合逻辑, 旧 text 归档就再也解不开了 —— 这条用例专门拦它
  head -c 300000 c2/novel.txt > t.txt
  "$REF" pack t_ref.hcax t.txt -m text >/dev/null 2>&1
  "$BIN" pack t_new.hcax t.txt -m text >/dev/null 2>&1
  if cmp -s t_ref.hcax t_new.hcax; then ok "CM(text) 码流与参考版逐字节一致"; else bad "CM 码流变了(会破坏旧 text 归档)"; fi
else
  printf '  \033[33mSKIP\033[0m 未找到参考版二进制 %s\n' "$REF"
fi

note "三轮验证合计: $PASS 通过 / $FAIL 失败"
[ "$FAIL" -eq 0 ]
