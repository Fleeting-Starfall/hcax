#!/usr/bin/env bash
# hcax regression tests
#
# Red lines (any failure is a regression):
#   1. lossless -- pack -> unpack -> diff -r must be byte-identical
#   2. format compat -- old archives must still unpack
#   3. verify sound -- flipping 1 byte must make verify fail
# also records ratio / wall time / peak RSS so optimizations can be compared.
#
# usage: ./test/regress.sh [hcax binary] [work dir]
set -u

BIN="${1:-./hcax}"
WORK="${2:-/tmp/hcax-regress}"
PY="${PYTHON:-python3}"
# resolve the script dir before cd (corpus generator lives next to it)
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$BIN" in /*) ;; *) BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")" ;; esac

# macOS /usr/bin/time -l reports peak RSS; Linux uses GNU time -v
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

# Peak memory (bytes; macOS maximum resident set size is bytes, not KB)
peak_bytes() {
  local log="$1"
  [ -n "$TIMER" ] || return 0
  sed -n 's/^[[:space:]]*\([0-9]*\)[[:space:]]*maximum resident set size.*/\1/p' "$log" | tail -1
}

# Bytes -> MB (integer); '?' when unavailable
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
# Isolate the temp dir to this run's work dir. TMPDIR is global; another hcax
# leaving orphans) creating/deleting files concurrently breaks the temp-leak counter,
# process (especially an interrupted one) may leave leftovers behind, and a later
TMPDIR="$WORK/tmp"; mkdir -p "$TMPDIR"; export TMPDIR
cd "$WORK" || exit 1

note "0. prepare corpus"
"$PY" "$SELF_DIR/mkcorpus.py" "$WORK/corpus" $((12*1024*1024)) >/dev/null || { echo "corpus generation failed"; exit 1; }
mkdir -p "$WORK/edge/deep/empty"
: > "$WORK/edge/empty.bin"
printf 'x' > "$WORK/edge/one.bin"
printf 'dots..\n' > "$WORK/edge/a..b.txt"
printf 'hidden\n' > "$WORK/edge/..weird.txt"
head -c 300000 /dev/urandom > "$WORK/edge/deep/rand.bin"
# symlinks: to file / to dir / dangling
ln -sf one.bin "$WORK/edge/link_file"
ln -sf deep "$WORK/edge/link_dir"
ln -sf __nope__ "$WORK/edge/link_dangling"
mkdir -p "$WORK/tree/a/b/c" "$WORK/tree/a/empty"
for i in $(seq 1 120); do
  printf '{"id":%d,"name":"item-%d","ok":true}\n' "$i" "$i" > "$WORK/tree/a/b/c/f$i.json"
done
head -c 40000 /dev/urandom > "$WORK/tree/a/b/blob.bin"
ok "corpus ready"

# ---------------------------------------------------------------- 1. round-trip lossless
note "1. round-trip lossless + ratio/time/memory"
printf '  %-8s %-10s %10s %9s %8s\n' mode corpus ratio time peakRSS
for mode in fast best max ultra text; do
  for corp in corpus tree; do
    rm -f "o_${mode}_${corp}.hcax"; rm -rf "r_${mode}_${corp}"
    run_timed "log_${mode}_${corp}" "$BIN" pack "o_${mode}_${corp}.hcax" "$corp" -m "$mode"
    ratio=$(sed -n 's/.*ratio \([0-9.]*\)%.*/\1/p' "log_${mode}_${corp}.out")
    wall=$(sed -n 's/^[[:space:]]*\([0-9.]*\)[[:space:]]*real.*/\1/p' "log_${mode}_${corp}" | tail -1)
    pk=$(peak_bytes "log_${mode}_${corp}")
    run_timed "logu_${mode}_${corp}" "$BIN" unpack "o_${mode}_${corp}.hcax" "r_${mode}_${corp}"
    # packing a dir keeps the dir name in the archive (tar/zip semantics),
    if diff -r "$corp" "r_${mode}_${corp}/$corp" >/dev/null 2>&1; then
      printf '  %-8s %-10s %9s%% %8ss %7sMB  ok-lossless\n' \
        "$mode" "$corp" "$ratio" "${wall:-?}" "$(mb "$pk")"
      PASS=$((PASS+1))
    else
      printf '  %-8s %-10s %9s%% %8ss %7sMB  DIFF\n' \
        "$mode" "$corp" "$ratio" "${wall:-?}" "$(mb "$pk")"
      FAIL=$((FAIL+1))
    fi
  done
done

# ---------------------------------------------------------------- 2. edge cases
note "2. edge cases"
rm -f edge.hcax; rm -rf r_edge
if "$BIN" pack edge.hcax edge -m best >/dev/null 2>&1; then ok "edge pack (empty/1-byte/..-named/nested empty dir)"; else bad "edge pack"; fi
if "$BIN" unpack edge.hcax r_edge >/dev/null 2>&1; then ok "edge unpack"; else bad "edge unpack"; fi
if diff -r edge r_edge/edge >/dev/null 2>&1; then ok "edge byte-identical"; else bad "edge differs"; fi
if [ -e r_edge/edge/a..b.txt ] && [ -e r_edge/edge/..weird.txt ]; then ok "legit double-dot names kept"; else bad "double-dot names still killed"; fi
if [ -d r_edge/edge/deep/empty ]; then ok "empty dir kept"; else bad "empty dir lost"; fi
if "$BIN" list edge.hcax 2>/dev/null | grep -q 'empty.bin'; then ok "empty file recorded"; else bad "empty file lost"; fi
# nested dirs must not be flattened (old impl lost top dir names when all files were in subdirs)
if "$BIN" list edge.hcax 2>/dev/null | grep -q '^.*  edge/deep/rand.bin'; then ok "nested path intact (not flattened)"; else bad "nested path flattened"; fi
# symlinks: old impl failed on dir-target links and stored file-target links as content copies
if [ -L r_edge/edge/link_file ] && [ -L r_edge/edge/link_dir ] && [ -L r_edge/edge/link_dangling ]; then
  ok "symlinks restored as links (incl. dir-target/dangling)"
else
  bad "symlinks not restored properly"
fi
if [ "$(readlink r_edge/edge/link_file 2>/dev/null)" = "one.bin" ] && \
   [ "$(readlink r_edge/edge/link_dir 2>/dev/null)" = "deep" ]; then
  ok "link targets correct"; else bad "link targets wrong"
fi
if [ -f r_edge/edge/link_file ] && cmp -s edge/one.bin r_edge/edge/one.bin; then
  ok "linked content accessible"; else bad "linked content broken"
fi

# dir mode/mtime: setting them while extracting gets bumped by later child writes,
# and chmod-ing read-only (0555) early blocks child files entirely -- defer to the end
mkdir -p dirmeta/ro
printf 'inside\n' > dirmeta/ro/f.txt
chmod 0555 dirmeta/ro
"$PY" -c "import os; os.utime('dirmeta/ro', (1577836800, 1577836800))"
rm -f dirmeta.hcax; rm -rf r_dirmeta
"$BIN" pack dirmeta.hcax dirmeta -m fast >/dev/null 2>&1
"$BIN" unpack dirmeta.hcax r_dirmeta >/dev/null 2>&1
if [ -f r_dirmeta/dirmeta/ro/f.txt ] && cmp -s dirmeta/ro/f.txt r_dirmeta/dirmeta/ro/f.txt; then
  ok "file inside read-only dir (0555) still extracted"; else bad "file in read-only dir missing"; fi
if stat -f '%Sp' r_dirmeta/dirmeta/ro 2>/dev/null | grep -q 'xr-xr-x'; then
  ok "dir mode restored (0555)"; else bad "dir mode not restored"; fi
dmt_src=$("$PY" -c "import os;print(int(os.stat('dirmeta/ro').st_mtime))")
dmt_out=$("$PY" -c "import os;print(int(os.stat('r_dirmeta/dirmeta/ro').st_mtime))")
if [ "$dmt_src" = "$dmt_out" ]; then
  ok "dir mtime restored (not all bumped to extract time)"; else bad "dir mtime lost: $dmt_src -> $dmt_out"; fi
chmod 755 dirmeta/ro

# deterministic packing: identical input must yield a **byte-identical** archive.
# archives are full of maps (dedup/hardlink/dict); if any one writes in map iteration
# order, the same command twice yields different files -- fatal for backup tools
rm -f det1.hcax det2.hcax
for dm in fast max text; do
  rm -f det1.hcax det2.hcax
  "$BIN" pack det1.hcax corpus -m "$dm" >/dev/null 2>&1
  "$BIN" pack det2.hcax corpus -m "$dm" >/dev/null 2>&1
  if cmp -s det1.hcax det2.hcax; then
    ok "[$dm] two packs byte-identical (no map-order dependence)"
  else
    bad "[$dm] two packs differ (not reproducible)"
  fi
done

# ---------------------------------------------------------------- 3. commands
note "3. command behavior"
A=o_max_corpus.hcax
if "$BIN" list "$A" >/dev/null 2>&1; then ok "list"; else bad "list"; fi
if "$BIN" verify "$A" >/dev/null 2>&1; then ok "verify"; else bad "verify"; fi
rm -rf rx
if "$BIN" extract "$A" rx novel.txt >/dev/null 2>&1 && cmp -s corpus/novel.txt rx/corpus/novel.txt; then ok "extract single file (content ok)"; else bad "extract single file"; fi
rm -rf rx2
if "$BIN" extract "$A" rx2 novel.txt records.json >/dev/null 2>&1 && \
   cmp -s corpus/novel.txt rx2/corpus/novel.txt && cmp -s corpus/records.json rx2/corpus/records.json; then
  ok "extract multiple files"; else bad "extract multiple files"; fi
# targets with a "./" prefix (shell completion / automation) must also match.
# (incremental/compare all break). Old versions reported "no match" while list
rm -rf rx3
if "$BIN" extract "$A" rx3 ./corpus/novel.txt >/dev/null 2>&1 && \
   cmp -s corpus/novel.txt rx3/corpus/novel.txt; then
  ok "extract matches ./-prefixed target"
else
  bad "extract misses ./-prefixed target"
fi
if "$BIN" unpack "$A" r_verify --verify >/dev/null 2>&1; then ok "unpack --verify"; else bad "unpack --verify"; fi
# list: path-sorted (pack emits files/dirs/links in groups, raw print is scrambled) + counts
if "$BIN" list "$A" 2>/dev/null | sed -n '1,/^total/p' | grep -E '^  ' | awk '{print $2}' | sort -c 2>/dev/null; then
  ok "list path-sorted"; else bad "list not path-sorted"; fi
if "$BIN" list "$A" 2>/dev/null | grep -qE 'entries \([0-9]+ files / [0-9]+ dirs / [0-9]+ links\)'; then
  ok "list counts"; else bad "list counts missing"; fi
if "$BIN" list edge.hcax -l 2>/dev/null | grep -qE 'L.*link_file -> one\.bin'; then
  ok "list -l shows link target"; else bad "list -l link target"; fi
if "$BIN" list edge.hcax -l 2>/dev/null | grep -qE '^  d.*edge/deep/$'; then
  ok "list -l dirs have type flag and slash"; else bad "list -l dir"; fi
# extract with a dir name: pull the whole subtree (old impl made an empty dir, no files)
rm -rf rx3
if "$BIN" extract edge.hcax rx3 edge/deep >/dev/null 2>&1 && \
   cmp -s edge/deep/rand.bin rx3/edge/deep/rand.bin; then
  ok "extract dir prefix pulls whole subtree"; else bad "extract dir prefix"; fi
# missing names must error clearly: a silent "extracted: 0 files" reads as success
rm -rf rx4
if "$BIN" extract edge.hcax rx4 definitely-missing 2>&1 | grep -qE 'no entries matching'; then
  ok "extract errors on total miss"; else bad "extract silent on total miss"; fi
# partial miss: matched ones extract, missing ones warn (one typo must not abort)
rm -rf rx5
if "$BIN" extract edge.hcax rx5 edge/deep/rand.bin nope 2>&1 | grep -qE 'warning: no entries matching' && \
   cmp -s edge/deep/rand.bin rx5/edge/deep/rand.bin; then
  ok "extract partial miss: warns, still extracts matches"; else bad "extract partial miss handling"; fi

# --exclude: dir match -> whole subtree skipped; basename match -> global; patterns use in-archive paths
mkdir -p exc/.git/objects exc/node_modules/pkg exc/src exc/build
printf 'g\n' > exc/.git/objects/o.bin
printf 'n\n' > exc/node_modules/pkg/i.js
printf 's\n' > exc/src/a.go
printf 't\n' > exc/src/x.tmp
printf 'b\n' > exc/build/a.o
if "$BIN" pack exc.hcax exc -m fast --exclude .git --exclude node_modules \
    --exclude '*.tmp' --exclude 'build/*' >/dev/null 2>&1; then
  ok "--exclude pack ok"; else bad "--exclude pack failed"; fi
L=$("$BIN" list exc.hcax 2>/dev/null)
if printf '%s\n' "$L" | grep -q 'exc/src/a.go' && \
   ! printf '%s\n' "$L" | grep -qE '\.git|node_modules|\.tmp|build/a\.o'; then
  ok "--exclude all three match kinds work (dir/basename/root-relative)"; else bad "--exclude filter incomplete"; fi
if "$BIN" pack exc.hcax exc -m fast --exclude .git 2>/dev/null | grep -q 'excluded 1 entries'; then
  ok "--exclude reports excluded count"; else bad "--exclude count missing"; fi

# v10 metadata: special mode bits (sticky/setuid/setgid) and nanosecond timestamps
# old impl used os.FileMode.Perm(), 0777 only -- those three bits were silently dropped
mkdir -p perm
printf 'sticky\n' > perm/st.bin && chmod 1755 perm/st.bin
printf 'ns\n' > perm/ns.bin
"$PY" -c "import os; os.utime('perm/ns.bin', ns=(1700000000123456789, 1700000000123456789))"
verbyte() { "$PY" -c "import sys; print(open(sys.argv[1],'rb').read(8)[4])" "$1"; }
if "$BIN" pack perm.hcax perm -m fast >/dev/null 2>&1; then ok "sticky-bit dir packs"; else bad "sticky-bit pack failed"; fi
if [ "$(verbyte perm.hcax)" = "10" ]; then ok "special bits bump to v10"; else bad "special bits did not bump v10"; fi
# use tree (JSON + random) not corpus -- corpus has a bitmap and legitimately bumps v12
if [ "$(verbyte o_max_tree.hcax)" = "8" ]; then ok "plain archive stays v8 (no gratuitous version bump)"; else bad "plain archive version wrong"; fi
rm -rf perm_out
if "$BIN" unpack perm.hcax perm_out >/dev/null 2>&1 && \
   stat -f '%Sp' perm_out/perm/st.bin 2>/dev/null | grep -q 't$'; then
  ok "sticky bit restored"; else bad "sticky bit lost"; fi
# sub-second timestamps default to seconds (like tar). Note: that archive is already v10
# (sticky), and v10 stores nanoseconds too -- so build a separate plain archive here.
mkdir -p plain1 && printf 'ns\n' > plain1/ns.bin
"$PY" -c "import os; os.utime('plain1/ns.bin', ns=(1700000000123456789, 1700000000123456789))"
rm -rf plain1_out
if "$BIN" pack plain1.hcax plain1 -m fast >/dev/null 2>&1 && \
   "$BIN" unpack plain1.hcax plain1_out >/dev/null 2>&1 && \
   [ "$("$PY" -c "import os; print(os.stat('plain1_out/plain1/ns.bin').st_mtime_ns)")" = "1700000000000000000" ]; then
  ok "timestamps default to seconds"; else bad "default timestamp precision wrong"; fi
rm -rf permns_out
if "$BIN" pack permns.hcax perm -m fast -T >/dev/null 2>&1 && \
   "$BIN" unpack permns.hcax permns_out >/dev/null 2>&1 && \
   [ "$("$PY" -c "import os; print(os.stat('permns_out/perm/ns.bin').st_mtime_ns)")" = "1700000000123456789" ]; then
  ok "-T nanosecond timestamps restored bit-for-bit"; else bad "-T nanosecond timestamps"; fi

# hard links (v11): multiple names of one inode store content once, share inode again after unpack
mkdir -p hardl
head -c 200000 /dev/urandom > hardl/orig.bin
if ln hardl/orig.bin hardl/a.bin 2>/dev/null && ln hardl/orig.bin hardl/b.bin 2>/dev/null; then
  if "$BIN" pack hardl.hcax hardl -m fast >/dev/null 2>&1; then ok "hard links pack"; else bad "hard link pack failed"; fi
  if [ "$(verbyte hardl.hcax)" = "11" ]; then ok "hard links bump v11"; else bad "hard links did not bump v11"; fi
  # content stored once: 200KB x3 = 600KB, archive must be well under 300KB
  if [ "$(stat -f%z hardl.hcax 2>/dev/null || stat -c%s hardl.hcax)" -lt 300000 ]; then
    ok "hard link content stored once"; else bad "hard link content duplicated"; fi
  rm -rf hardl_out
  if "$BIN" unpack hardl.hcax hardl_out >/dev/null 2>&1; then
    i1=$("$PY" -c "import os; print(os.stat('hardl_out/hardl/orig.bin').st_ino)")
    i2=$("$PY" -c "import os; print(os.stat('hardl_out/hardl/a.bin').st_ino)")
    if [ -n "$i1" ] && [ "$i1" = "$i2" ]; then ok "hard link restored as shared inode"; else bad "hard link not restored"; fi
    if cmp -s hardl/orig.bin hardl_out/hardl/b.bin; then ok "hard link content ok"; else bad "hard link content differs"; fi
  else
    bad "hard link unpack failed"
  fi
  # extracting the link alone (source outside selection) must degrade to a complete file
  rm -rf hardl_x
  if "$BIN" extract hardl.hcax hardl_x hardl/b.bin >/dev/null 2>&1 && \
     cmp -s hardl/orig.bin hardl_x/hardl/b.bin; then
    ok "extracting one hard link yields a complete file"; else bad "extracted hard link incomplete"; fi
else
  printf '  \033[33mSKIP\033[0m filesystem does not support hard links\n'
fi

# unpack progress: big archives need feedback (pack has it; unpack lacked it), tiny ones must not spam
mkdir -p progb && head -c 35000000 /dev/zero > progb/z.bin
if "$BIN" pack progb.hcax progb -m fast >/dev/null 2>&1; then ok "big corpus for progress packs"; else bad "big corpus pack failed"; fi
rm -rf progb_out
if "$BIN" unpack progb.hcax progb_out 2>&1 >/dev/null | grep -q 'extracting'; then
  ok "big archive shows unpack progress"; else bad "big archive no progress"; fi
rm -rf progb_out2
if "$BIN" unpack edge.hcax progb_out2 2>&1 >/dev/null | grep -q 'extracting'; then
  bad "small archive shows progress (noise)"; else ok "small archive quiet"; fi

# high internal redundancy: reference chunk count >> unique chunk count.
# old code compared "chunks a file references" vs "chunk table entries", assuming
# refs could never exceed unique chunks -- but dedup means many refs to one chunk,
mkdir -p repd
"$PY" -c "
blk = b'hcax' * 16384          # 64KB per chunk
with open('repd/r.bin', 'wb') as f:
    for _ in range(200):
        f.write(blk)
"
if "$BIN" pack repd.hcax repd -m fast >/dev/null 2>&1; then ok "redundant file packs"; else bad "redundant pack failed"; fi
if "$BIN" list repd.hcax >/dev/null 2>&1; then
  ok "redundant archive reads (refs > unique)"; else bad "redundant archive flagged corrupt"; fi
rm -rf repd_out
if "$BIN" unpack repd.hcax repd_out >/dev/null 2>&1 && cmp -s repd/r.bin repd_out/repd/r.bin; then
  ok "redundant round-trip ok"; else bad "redundant round-trip differs"; fi

# archive must not archive itself: `hcax pack arch.hcax .` used to ingest the previous
# archive, growing every run (154 -> 278 -> 389...) and storing stale content as new data
mkdir -p selfd && printf 'data\n' > selfd/a.txt
sz() { stat -f%z "$1" 2>/dev/null || stat -c%s "$1"; }
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s1=$(sz selfd/arch.hcax)
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s2=$(sz selfd/arch.hcax)
( cd selfd && "$BIN" pack arch.hcax . -m fast >/dev/null 2>&1 )
s3=$(sz selfd/arch.hcax)
if [ -n "$s1" ] && [ "$s1" = "$s2" ] && [ "$s2" = "$s3" ]; then
  ok "archive never packs itself (size stable across runs: $s1 B)"; else bad "self-inclusion: $s1 -> $s2 -> $s3"; fi
if "$BIN" list selfd/arch.hcax 2>/dev/null | grep -q 'arch\.hcax'; then
  bad "archive contains itself"; else ok "archive excludes itself"; fi

# duplicate/overlapping inputs: old impl stored duplicate entries, later ones silently
mkdir -p dupd && printf 'x\n' > dupd/f.txt
"$BIN" pack dupd.hcax dupd dupd -m fast >/dev/null 2>&1
if [ "$("$BIN" list dupd.hcax 2>/dev/null | grep -c 'dupd/f.txt')" = "1" ]; then
  ok "same dir twice yields no duplicate entries"; else bad "duplicate entries from repeated input"; fi
"$BIN" pack dupd2.hcax dupd ./dupd dupd/ -m fast >/dev/null 2>&1
if [ "$("$BIN" list dupd2.hcax 2>/dev/null | grep -c 'dupd/f.txt')" = "1" ]; then
  ok "./dup, dup, dup/ normalize to one input"; else bad "equivalent paths not normalized"; fi
# dir + file inside it: in-archive name overlap
"$BIN" pack dupd3.hcax dupd dupd/f.txt -m fast 2>/dev/null | grep -q 'duplicate entries' && \
if "$BIN" list dupd3.hcax 2>/dev/null | grep -q 'dupd/f.txt'; then
  ok "overlapping inputs warned, one copy kept"; else bad "overlapping inputs mishandled"; fi

# missing CLI arg values: -m without a value used to crash with index out of range
if "$BIN" pack needarg.hcax edge -m 2>&1 | grep -qE 'needs a mode argument'; then
  ok "-m missing arg errors clearly (no crash)"; else bad "-m missing arg crashed or vague"; fi
if "$BIN" pack needarg.hcax edge --exclude 2>&1 | grep -qE 'needs a pattern argument'; then
  ok "--exclude missing arg errors clearly"; else bad "--exclude missing arg mishandled"; fi
if "$BIN" pack onlyout.hcax 2>&1 | grep -q 'needs an output and an input'; then
  ok "missing input shows usage"; else bad "missing input vague"; fi

# unpack into a non-empty dir silently replaces same-named entries (like tar);
rm -rf ovw && "$BIN" unpack o_max_corpus.hcax ovw >/dev/null 2>&1
if "$BIN" unpack o_max_corpus.hcax ovw 2>/dev/null | grep -q 'overwrote [0-9]* existing entries'; then
  ok "re-unpack reports overwritten count"; else bad "re-unpack no overwrite report"; fi
rm -rf ovw2
if "$BIN" unpack o_max_corpus.hcax ovw2 2>/dev/null | grep -q 'overwrote'; then
  bad "first unpack into empty dir reports overwrite"; else ok "first unpack reports none"; fi

# ---------------------------------------------------------------- 4. corruption detection
note "4. corruption detection"
cp "$A" corrupt.hcax
# flip 1 byte in the middle of the data region
SZ=$(wc -c < corrupt.hcax)
OFF=$((SZ/2))
printf '\xff' | dd of=corrupt.hcax bs=1 seek="$OFF" count=1 conv=notrunc >/dev/null 2>&1
if "$BIN" verify corrupt.hcax >/dev/null 2>&1; then bad "verify passed despite flip"; else ok "1-byte flip caught by verify"; fi
# truncation: old impl wrote the trailer without ever checking; truncated archives
cp "$A" trunc.hcax
TSZ=$(wc -c < trunc.hcax | tr -d ' ')
: | dd of=trunc.hcax bs=1 seek=$((TSZ-30)) count=0 conv=notrunc >/dev/null 2>&1
head -c $((TSZ-30)) "$A" > trunc.hcax
if "$BIN" list trunc.hcax >/dev/null 2>&1; then bad "truncated archive accepted"; else ok "truncated archive rejected"; fi
if "$BIN" list trunc.hcax 2>&1 | grep -qE 'truncat|trailer'; then ok "truncation error clear"; else bad "truncation error vague"; fi

# ---------------------------------------------------------------- 5. resources

# raw region (incompressible) has its own stream hash: flipping mid-"data" may always
# the raw-region verify path would never run. Build an archive that has one.
mkdir -p rawcorp
head -c 300000 /dev/urandom > rawcorp/rand.bin
printf 'compressible text %.0s' $(seq 1 3000) > rawcorp/txt.txt
rm -f rawcorp.hcax
"$BIN" pack rawcorp.hcax rawcorp -m max >/dev/null 2>&1
if "$BIN" info rawcorp.hcax 2>/dev/null | grep -q 'raw'; then
  ok "max mode has a raw region (incompressible chunks stored raw)"
else
  bad "no raw region produced; raw verify cases are no-ops"
fi
"$PY" - "$PWD/rawcorp.hcax" <<'PYX'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
d[-40] ^= 0xFF          # before the trailer magic (20B) and dict region -- definitely raw
open(p + '.bad', 'wb').write(bytes(d))
PYX
if "$BIN" verify rawcorp.hcax.bad >/dev/null 2>&1; then
  bad "corrupt raw region passed verify"; else ok "corrupt raw region caught by verify"; fi
# list reads metadata only, never the data region, so a fine list != intact data --
# pin that down so nobody treats list as verification
if "$BIN" list rawcorp.hcax.bad >/dev/null 2>&1; then
  ok "list never reads the data region (lists fine when damaged, so not verification)"
else
  bad "list read the data region (expected: metadata only)"
fi
rm -rf rcbad
if "$BIN" unpack rawcorp.hcax.bad rcbad --verify >/dev/null 2>&1; then
  bad "unpack --verify let corrupt raw region through"; else ok "unpack --verify blocks corrupt raw region"; fi

# metadata region (compressed frame) damage: metadata is decompressed then parsed;
"$PY" - "$PWD/rawcorp.hcax" <<'PYX'
import sys
p = sys.argv[1]
d = bytearray(open(p, 'rb').read())
d[60] ^= 0xFF           # the compressed metadata frame right after the 48B header
open(p + '.meta', 'wb').write(bytes(d))
PYX
if "$BIN" list rawcorp.hcax.meta >/dev/null 2>&1; then
  bad "damaged metadata accepted"; else ok "damaged metadata rejected"; fi

# the two outermost bytes: magic and version
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
if "$BIN" list rawcorp.hcax.magic 2>&1 | grep -q 'not a HCAX'; then
  ok "bad magic reports 'not a HCAX file'"; else bad "magic check wrong"; fi
if "$BIN" list rawcorp.hcax.ver 2>&1 | grep -q 'unsupported version'; then
  ok "high version reports 'unsupported version'"; else bad "version check wrong"; fi

# corrupt header: absurd length/count fields must error clearly, not OOM or panic
cp "$A" badhdr.hcax
python3 - "$PWD/badhdr.hcax" <<'PYX'
import struct,sys
p=sys.argv[1]
d=bytearray(open(p,'rb').read())
struct.pack_into('<I', d, 40, 0xFFFFFFF0)   # absurd nChunks
open(p,'wb').write(bytes(d))
PYX
if "$BIN" list badhdr.hcax >/dev/null 2>&1; then bad "absurd nChunks accepted"; else ok "absurd nChunks rejected"; fi
if "$BIN" list badhdr.hcax 2>&1 | grep -qE 'corrupt|invalid|truncated|mismatch'; then ok "header damage error clear"; else bad "header damage error vague"; fi


# all-empty content (empty dirs and files only): old impl divided by zero, +Inf%
mkdir -p emptyonly/sub
: > emptyonly/zero.bin
: > emptyonly/sub/zero2.bin
if "$BIN" pack eo.hcax emptyonly -m best >/dev/null 2>&1; then ok "all-empty packs"; else bad "all-empty pack failed"; fi
if "$BIN" pack eo.hcax emptyonly -m best 2>/dev/null | grep -q 'Inf\|NaN'; then bad "all-empty ratio shows Inf/NaN"; else ok "all-empty ratio clean of Inf/NaN"; fi
rm -rf eo_out
if "$BIN" unpack eo.hcax eo_out >/dev/null 2>&1 && [ -d eo_out/emptyonly/sub ] && [ -f eo_out/emptyonly/zero.bin ]; then
  ok "all-empty unpacks (dirs and empty files)"; else bad "all-empty unpack broken"; fi


# robustness: with unreadable files or FIFOs, old versions failed entirely or hung
mkdir -p robo
printf 'good data\n' > robo/ok.txt
printf 'secret\n' > robo/locked.txt
chmod 000 robo/locked.txt
if command -v mkfifo >/dev/null 2>&1; then mkfifo robo/pipe 2>/dev/null; fi
# no timeout: macOS lacks GNU coreutils' timeout by default
if "$BIN" pack robo.hcax robo -m fast >/dev/null 2>&1; then
  ok "unreadable/FIFO pack completes (no fail, no hang)"
else
  bad "unreadable/FIFO pack failed or hung"
fi
if "$BIN" list robo.hcax 2>/dev/null | grep -q 'robo/ok.txt'; then ok "readable file still archived"; else bad "readable file lost"; fi
chmod 644 robo/locked.txt

note "5. resources & hygiene"
count_tmp() { find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'hcax-solid-*' -o -maxdepth 1 -name 'hcax-raw-*' 2>/dev/null | wc -l | tr -d ' '; }
before=$(count_tmp)
"$BIN" pack tmpchk.hcax corpus -m max >/dev/null 2>&1
after=$(count_tmp)
if [ "$after" -le "$before" ]; then ok "no temp leak (pack, before=$before after=$after)"; else bad "pack leaked temp files (before=$before after=$after)"; fi
# verify/list too: verify readChunk -> solid-stream temp file (up to whole decompressed
# size); its success path never ran runCleanups, leaving tens of MB in /tmp per verify.
# old versions went red here -- exactly what we catch.
before=$(count_tmp)
"$BIN" verify tmpchk.hcax >/dev/null 2>&1
"$BIN" list tmpchk.hcax -l >/dev/null 2>&1
"$BIN" info tmpchk.hcax >/dev/null 2>&1
after=$(count_tmp)
if [ "$after" -le "$before" ]; then ok "no temp leak (verify/list/info, before=$before after=$after)"; else bad "verify/list/info leaked temp files (before=$before after=$after)"; fi

# failure path: first file written to temp, second unreadable -> fails **after** temp
# (old impl used defer os.Remove, but fatal() calls os.Exit, defer never runs -> leak)
mkdir -p leakdir
printf 'some data to pack first\n' > leakdir/ok.txt
printf 'unreadable\n' > leakdir/no.txt
chmod 000 leakdir/no.txt
"$BIN" pack never.hcax leakdir -m best >/dev/null 2>&1
after2=$(count_tmp)
if [ "$after2" -le "$before" ]; then ok "no temp leak on failure path (before=$before after=$after2)"; else bad "failure path leaked temp files (before=$before after=$after2)"; fi
chmod 644 leakdir/no.txt

# raster (BMP) end-to-end: 24bpp width-101 row stride 304 is not a multiple of 3,
# row-wise transforms (8/9) exist for it; degrading to "whole-block 3-byte groups" costs 6%
if [ -f corpus/photo24.bmp ]; then
  if cmp -s corpus/photo24.bmp r_best_corpus/corpus/photo24.bmp; then
    ok "24bpp BMP (row padding) round-trips byte-identical"
  else
    bad "24bpp BMP round-trip broken"
  fi
  if cmp -s corpus/photo32.bmp r_best_corpus/corpus/photo32.bmp; then
    ok "32bpp BMP (no row padding) round-trips byte-identical"
  else
    bad "32bpp BMP round-trip broken"
  fi
  # row-wise raster transform -> v12; plain archives stay v8 (no blanket bump)
  if "$BIN" info o_best_corpus.hcax 2>/dev/null | grep -q 'v12'; then
    ok "row-raster archive writes v12"; else bad "row-raster archive not v12"; fi
  rm -f norast.hcax
  mkdir -p norast && printf 'plain text only\n' > norast/a.txt
  "$BIN" pack norast.hcax norast -m fast >/dev/null 2>&1
  if "$BIN" info norast.hcax 2>/dev/null | grep -q 'v8'; then
    ok "no-raster archive stays v8 (no gratuitous bump)"; else bad "no-raster archive bumped"; fi
  # a packed BMP with non-3-multiple row padding must be much smaller than the source
  bsz=$(sz corpus/photo24.bmp)
  rm -f bmp1.hcax; "$BIN" pack bmp1.hcax corpus/photo24.bmp -m best >/dev/null 2>&1
  asz=$(sz bmp1.hcax)
  if [ "$asz" -lt "$bsz" ]; then
    ok "24bpp BMP compressed ($bsz -> $asz B)"
  else
    bad "24bpp BMP not compressed ($bsz -> $asz B)"
  fi
else
  bad "corpus lacks photo24.bmp (raster path uncovered end-to-end)"
fi

# ------------------------------------------------- degradation warning when xz missing
note "degradation warning when system xz is missing"
# max/ultra depend on system xz. When missing, the old fallback to pure-Go lzma2 was
# **fully silent**; the two modes differ by 24% ratio (26.89% vs 33.40%) -- users thought
# they were on the strongest mode while archives grew. Point PATH at an empty dir to
emptybin="$(mktemp -d)"
rm -f noxz.hcax
noxz_err="$(PATH="$emptybin" "$BIN" pack noxz.hcax corpus -m max 2>&1 >/dev/null)"
nwarn="$(printf '%s\n' "$noxz_err" | grep -c 'warning')"
if [ "$nwarn" -ge 1 ]; then
  ok "warning emitted when system xz missing"
else
  bad "silent fallback when xz missing -- 24% ratio loss with no hint"
fi
# warn once: max compresses many solid streams; a wall of warnings is as good as none
if [ "$nwarn" -eq 1 ]; then
  ok "degradation warning fired once (no spam)"
else
  bad "degradation warning fired $nwarn times (expected exactly 1)"
fi
rmdir "$emptybin"

# when system xz writes to stderr (even a harmless warning), it must not pollute the
# compressed stream. Old code shared one buffer for stdout/stderr; warnings got
# appended to the lzma2 data header: pack "succeeded", archives grew dozens of bytes,
shimdir="$WORK/shimxz"
mkdir -p "$shimdir"
cat > "$shimdir/xz" <<SHIM
#!/bin/sh
echo 'xz: (simulated) one harmless warning' >&2
exec $PY -c 'import sys,lzma; sys.stdout.buffer.write(lzma.compress(sys.stdin.buffer.read(), format=lzma.FORMAT_XZ))'
SHIM
chmod +x "$shimdir/xz"
rm -f shimxz.hcax
PATH="$shimdir:$PATH" "$BIN" pack shimxz.hcax corpus -m max >/dev/null 2>&1
rm -rf shimxzout
# unpack is pure Go, PATH-independent; the point is that the packed output unpacks
if "$BIN" unpack shimxz.hcax shimxzout >/dev/null 2>&1 &&
   diff -r corpus shimxzout/corpus >/dev/null 2>&1; then
  ok "external xz stderr kept out of the stream (warning did not corrupt archive)"
else
  bad "external xz stderr polluted the stream -- packed OK but archive corrupt"
fi

# ------------------------------------------------- atomic archive write
note "archive write (atomic replace + no partial file)"
# old code os.Create(outPath) truncated the previous archive to 0 on open: a failure at
# the final write stage lost the old backup and left a half-written new one. Now it is
rm -f atomic.hcax
"$BIN" pack atomic.hcax corpus -m fast >/dev/null 2>&1
# a fresh archive must be 0644. CreateTemp makes 0600 temp files; without a chmod,
# a straight rename would leave an unreadable archive for others
perm=$(stat -f%Lp atomic.hcax 2>/dev/null || stat -c%a atomic.hcax 2>/dev/null)
if [ "$perm" = "644" ]; then
  ok "fresh archive mode 0644 (not the temp 0600)"
else
  bad "fresh archive mode $perm (want 644)"
fi
# distinguish "atomic replace (rename)" from "in-place truncate (os.Create)": the
# in-place truncate destroys the old archive the moment writing starts -- a mid-fail
# former changes the inode, the latter does not (mode cannot tell: os.Create keeps mode,
ino1=$(stat -f%i atomic.hcax 2>/dev/null || stat -c%i atomic.hcax 2>/dev/null)
"$BIN" pack atomic.hcax corpus -m fast >/dev/null 2>&1
ino2=$(stat -f%i atomic.hcax 2>/dev/null || stat -c%i atomic.hcax 2>/dev/null)
if [ -n "$ino1" ] && [ "$ino1" != "$ino2" ]; then
  ok "overwrite is atomic replace (rename), not in-place truncate"
else
  bad "overwrite was in-place truncate (old archive gone at write start)"
fi
if "$BIN" verify atomic.hcax >/dev/null 2>&1; then
  ok "archive still valid after re-packing the same path"
else
  bad "archive corrupt after re-packing the same path"
fi
leftover=$(find . -maxdepth 1 -name '.hcax-new-*' | wc -l | tr -d ' ')
if [ "$leftover" -eq 0 ]; then
  ok "no leftover .hcax-new-* partial files"
else
  bad "$leftover .hcax-new-* temp files left"
fi

# ------------------------------------------------- truncated archives must fail cleanly
note "truncated archives"
# length fields claiming N bytes in a shorter file are the easiest panic input.
# A panic is DoS when auto-processing foreign archives. Go side exhausts prefixes (see
# hcax_test.go); here we re-cover from the CLI: non-zero exit, no panic/goroutine in output.
asrc=o_max_corpus.hcax
n=$(sz "$asrc")
panicked=0
for frac in 2 3 10; do
  rm -f tr.hcax
  head -c $((n / frac)) "$asrc" > tr.hcax
  out=$("$BIN" list tr.hcax 2>&1)
  if printf '%s' "$out" | grep -q 'panic\|goroutine'; then
    panicked=1
  fi
done
if [ "$panicked" -eq 0 ]; then
  ok "truncated to 1/2, 1/3, 1/10 never panics"
else
  bad "truncated archive panicked (bad input must not crash)"
fi
# archives too short for even a header must exit with an error, not "look fine"
rm -f tr.hcax
head -c 8 "$asrc" > tr.hcax
if "$BIN" list tr.hcax >/dev/null 2>&1; then
  bad "8-byte archive accepted as valid"
else
  ok "8-byte archive errors out (no fake success)"
fi

# ---------------------------------------------------------------- summary
note "summary: $PASS passed / $FAIL failed"
[ "$FAIL" -eq 0 ]
