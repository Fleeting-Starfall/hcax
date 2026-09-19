#!/usr/bin/env bash
# "three-pass verification" after a round of changes
#   pass 1 functional regression: 5 modes x 2 corpora lossless + edge + commands + corruption + hygiene
#   pass 2 lossless round trips: bigger corpus x more modes, byte diffs (incl. extract/--verify)
#   pass 3 compat matrix: reference (v8) archives unpack here; ours unpack on the reference
# usage: bash test/verify3.sh [work dir]
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
WORK="${1:-/tmp/hcax-verify3}"
PY="${PYTHON:-python3}"
REF="${HCAX_REF:-$HOME/.hcax-ref/hcax-v8}"
BIN="$ROOT/hcax"

# the next line runs rm -rf "$WORK". The first arg is the **work dir**; passing the binary
# (bash test/verify3.sh ./hcax) would delete the freshly built hcax, fail everything,
# and point errors at unrelated places. Guard it here.
case "$WORK" in
  "$BIN"|"$ROOT"|""|"/"|"."|"..")
    echo "verify3.sh: refusing to delete '$WORK' as the work dir (first arg must be a work dir, not the binary)" >&2
    echo "usage: bash test/verify3.sh [work dir]" >&2
    exit 1 ;;
esac

PASS=0; FAIL=0
ok(){ printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad(){ printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
note(){ printf '\n\033[1m%s\033[0m\n' "$1"; }

[ -x "$BIN" ] || { echo "missing $BIN"; exit 1; }
rm -rf "$WORK"; mkdir -p "$WORK"; cd "$WORK" || exit 1

# ------------------------------------------------------------ pass 1
note "pass 1: functional regression"
if out=$(PYTHON="$PY" bash "$HERE/regress.sh" "$BIN" "$WORK/w1" 2>&1); then
  echo "$out" | tail -1 | sed 's/^/  /'; PASS=$((PASS+1)); ok "regress.sh all passed"
else
  echo "$out" | grep FAIL | sed 's/^/  /'; FAIL=$((FAIL+1)); bad "regress.sh has failures"
fi

# cross-platform: fileid_unix.go / fileid_windows.go carry build tags, but only darwin
# is built normally; a broken port would wait for someone else's cross-build to surface
xok=1
while read -r goos goarch; do
  if ! (cd "$ROOT" && GOOS="$goos" GOARCH="$goarch" go build -o /dev/null . >/dev/null 2>&1); then
    xok=0; bad "GOOS=$goos GOARCH=$goarch build failed"; fi
done <<'XARCH'
windows amd64
linux amd64
XARCH
[ "$xok" -eq 1 ] && ok "windows / linux cross-builds pass"

# unit tests (random tree round trips / random corruption no-crash): e2e scripts take seconds,
# but many bugs (R18's "opens, never unpacks") are caught by millisecond cases
if (cd "$ROOT" && go test ./... >"$WORK/gotest.log" 2>&1); then
  ok "go test all passed"; else bad "go test has failures"; sed 's/^/    /' "$WORK/gotest.log" | tail -20; fi

# ------------------------------------------------------------ pass 2
note "pass 2: lossless round trips (larger corpus)"
"$PY" "$HERE/mkcorpus.py" "$WORK/c2" $((16*1024*1024)) >/dev/null
mkdir -p "$WORK/c2/nest/deep/deeper"
head -c 800000 /dev/urandom > "$WORK/c2/nest/deep/deeper/blob.bin"
ln -sf novel.txt "$WORK/c2/link"
for m in fast best max ultra; do
  rm -f "a_$m.hcax"; rm -rf "b_$m"
  "$BIN" pack "a_$m.hcax" c2 -m "$m" >/dev/null 2>&1 || { bad "[$m] pack"; continue; }
  "$BIN" unpack "a_$m.hcax" "b_$m" >/dev/null 2>&1 || { bad "[$m] unpack"; continue; }
  if diff -r c2 "b_$m/c2" >/dev/null 2>&1; then ok "[$m] round trip byte-identical"; else bad "[$m] round trip differs"; fi
  "$BIN" verify "a_$m.hcax" >/dev/null 2>&1 && ok "[$m] verify passed" || bad "[$m] verify"
  "$BIN" unpack "a_$m.hcax" "bv_$m" --verify >/dev/null 2>&1 && ok "[$m] --verify unpack" || bad "[$m] --verify"
done
# extract: one file must match its source
rm -rf x1
if "$BIN" extract a_max.hcax x1 novel.txt >/dev/null 2>&1 && cmp -s c2/novel.txt x1/c2/novel.txt; then
  ok "extract single file matches"; else bad "extract single file"; fi
rm -rf x2
if "$BIN" extract a_max.hcax x2 novel.txt records.json >/dev/null 2>&1 && \
   cmp -s c2/novel.txt x2/c2/novel.txt && cmp -s c2/records.json x2/c2/records.json; then
  ok "extract multiple files match"; else bad "extract multiple files"; fi
# symlinks
if [ -L b_max/c2/link ]; then ok "symlink restored"; else bad "symlink not restored"; fi
# parallel solid grouping (mmt) only runs on >=64MiB compressible data and once had a bug
# (v8: empty group -> empty frame -> unpack OOB panic) small corpora never hit. Build 70MB.
"$PY" - "$WORK/c2big/big.json" <<'PYX'
import os, sys
p = sys.argv[1]
os.makedirs(os.path.dirname(p), exist_ok=True)
line = b'{"id":%08d,"name":"item-%08d","ok":true,"payload":"abcdefghij"}\n'
with open(p, 'wb') as f:
    i = 0
    n = 0
    while n < 70 * 1024 * 1024:
        f.write(line % (i, i)); n += 58; i += 1
PYX
if [ -s c2big/big.json ]; then
  rm -f abig.hcax; rm -rf bbig
  if "$BIN" pack abig.hcax c2big -m max >/dev/null 2>&1; then
    ok "[70MB] pack (exercises mmt parallel groups)"
  else
    bad "[70MB] pack failed"
  fi
  if "$BIN" unpack abig.hcax bbig >/dev/null 2>&1 && cmp -s c2big/big.json bbig/c2big/big.json; then
    ok "[70MB] round trip byte-identical (mmt empty-group regression covered)"
  else
    bad "[70MB] round trip differs"
  fi
  if "$BIN" verify abig.hcax >/dev/null 2>&1; then ok "[70MB] verify passed"; else bad "[70MB] verify"; fi
else
  bad "70MB corpus not built; mmt path uncovered"
fi

# real executable: the corpus is all synthetic; BCJ-x86 never ran on real code.
# use the freshly built hcax itself (4.5MB Mach-O) as corpus; no environment dependency.
mkdir -p c2exe && cp "$BIN" c2exe/hcax.bin
if [ -s c2exe/hcax.bin ]; then
  rm -f aexe.hcax; rm -rf bexe
  if "$BIN" pack aexe.hcax c2exe -m max >/dev/null 2>&1; then
    ok "[real executable] pack"
  else
    bad "[real executable] pack failed"
  fi
  if "$BIN" unpack aexe.hcax bexe >/dev/null 2>&1 && cmp -s c2exe/hcax.bin bexe/c2exe/hcax.bin; then
    ok "[real executable] round trip byte-identical (BCJ-x86 e2e)"
  else
    bad "[real executable] round trip differs"
  fi
else
  bad "no executable available as corpus"
fi

# ------------------------------------------------------------ pass 3
note "pass 3: compat matrix"
if [ -x "$REF" ]; then
  # the matrix needs a corpus with **no symlinks, no bitmaps**: links force v9, bitmaps v12,
  # and old builds reject both by design, killing the "v8 interop" test
  "$PY" "$HERE/mkcorpus.py" "$WORK/c3" $((8*1024*1024)) noimg >/dev/null
  rm -f old.hcax new.hcax; rm -rf o1 o2
  "$REF" pack old.hcax c3 -m max >/dev/null 2>&1
  "$BIN" pack new.hcax c3 -m max >/dev/null 2>&1
  if "$BIN" unpack old.hcax o1 >/dev/null 2>&1 && diff -r c3 o1 >/dev/null 2>&1; then
    ok "reference (v8) archive unpacks losslessly here"; else bad "reference archive does not unpack"; fi
  if "$REF" unpack new.hcax o2 >/dev/null 2>&1 && diff -r c3 o2/c3 >/dev/null 2>&1; then
    ok "our archive unpacks losslessly on reference (v8)"; else bad "our archive fails on reference"; fi
  "$BIN" verify old.hcax >/dev/null 2>&1 && ok "current verifies the reference archive" || bad "verifying the reference archive"
  # link-bearing archives must be explicitly rejected by old builds (no wrong data)
  if "$REF" list a_max.hcax >/dev/null 2>&1; then
    bad "link-bearing v9 accepted by old build (should reject)"; else ok "link-bearing v9 explicitly rejected by old build"; fi
  # v10/v11 archives must be explicitly rejected by old builds, not misparsed
  mkdir -p pv10 && printf 'x\n' > pv10/s.bin && chmod 1755 pv10/s.bin
  "$BIN" pack pv10.hcax pv10 -m fast >/dev/null 2>&1
  if "$REF" list pv10.hcax >/dev/null 2>&1; then
    bad "v10 archive accepted by old build (should reject)"; else ok "v10 archive explicitly rejected by old build"; fi
  mkdir -p pv11 && printf 'y\n' > pv11/o.bin && ln pv11/o.bin pv11/l.bin 2>/dev/null
  if [ -e pv11/l.bin ]; then
    "$BIN" pack pv11.hcax pv11 -m fast >/dev/null 2>&1
    if "$REF" list pv11.hcax >/dev/null 2>&1; then
      bad "v11 archive accepted by old build (should reject)"; else ok "v11 (hardlink) explicitly rejected by old build"; fi
  fi
  # v12 (row raster transforms) archives must likewise be explicitly rejected
  mkdir -p pv12 && cp c2/photo24.bmp pv12/ 2>/dev/null
  if [ -s pv12/photo24.bmp ]; then
    "$BIN" pack pv12.hcax pv12 -m fast >/dev/null 2>&1
    if "$REF" list pv12.hcax >/dev/null 2>&1; then
      bad "v12 archive accepted by old build (should reject)"; else ok "v12 (row raster) explicitly rejected by old build"; fi
  fi
  # CM (text) bitstream must match the reference byte-for-byte: CM may only be optimized
  # without changing numeric paths; any model/mix change strands old text archives -- guard it
  head -c 300000 c2/novel.txt > t.txt
  "$REF" pack t_ref.hcax t.txt -m text >/dev/null 2>&1
  "$BIN" pack t_new.hcax t.txt -m text >/dev/null 2>&1
  if cmp -s t_ref.hcax t_new.hcax; then ok "CM (text) bitstream matches reference byte-for-byte"; else bad "CM bitstream changed (would strand old text archives)"; fi
else
  printf '  \033[33mSKIP\033[0m reference binary %s not found\n' "$REF"
fi

note "three-pass verification: $PASS passed / $FAIL failed"
[ "$FAIL" -eq 0 ]
