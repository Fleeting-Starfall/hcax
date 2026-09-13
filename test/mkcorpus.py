#!/usr/bin/env python3
"""生成回归测试语料。

设计要点:
- 可压(不是随机数据, 否则会被判为"原样存储", 绕开压缩器)
- 非重复(否则 CDC 去重会把语料折叠成几十块, 测不出真实内存/耗时)
- 类型多样(文本/代码/JSON/结构化二进制/不可压图片), 贴近真实归档场景
"""
import math
import os
import random
import string
import sys

VOCAB = (
    "the quick brown fox jumps over lazy dog while machine learning models "
    "compress data streams efficiently across distributed systems and storage "
    "layers that must preserve byte exact fidelity for archival purposes "
    "context mixing predicts next bits using adaptive logistic regression "
).split()


def gen_text(n, rng):
    """英文伪文本: 词表有限 -> 可压; 组合随机 -> 长距离重复极少"""
    out = []
    size = 0
    while size < n:
        k = rng.randint(6, 24)
        words = [rng.choice(VOCAB) for _ in range(k)]
        s = " ".join(words).capitalize() + rng.choice(". ! ? ,\n") + " "
        b = s.encode()
        out.append(b)
        size += len(b)
    return b"".join(out)[:n]


def gen_code(n, rng):
    """伪源码: 缩进/关键字重复 -> 可压"""
    lines = []
    size = 0
    kw = ["func", "return", "if", "for", "range", "var", "const", "type", "struct"]
    ty = ["int", "string", "[]byte", "error", "bool", "float64", "*Node"]
    while size < n:
        ind = "    " * rng.randint(0, 3)
        k = rng.choice(kw)
        if k == "func":
            ln = f"{ind}func handle{rng.randint(0,9999)}(ctx context.Context, p *Payload) ({rng.choice(ty)}, error) {{\n"
        elif k == "return":
            ln = f"{ind}return value{rng.randint(0,999)}, nil\n"
        elif k in ("if", "for"):
            ln = f"{ind}{k} err := check{rng.randint(0,99)}(); err != nil {{\n"
        else:
            ln = f"{ind}{k} x{rng.randint(0,999)} {rng.choice(ty)} = compute{rng.randint(0,999)}(a, b)\n"
        b = ln.encode()
        lines.append(b)
        size += len(b)
    return b"".join(lines)[:n]


def gen_json(n, rng):
    """伪 JSON 记录: 结构高度重复 -> 可压且是典型的"文件夹归档"负载"""
    out = []
    size = 0
    i = 0
    while size < n:
        rec = (
            '{"id":%d,"ts":%d,"user":"u%06d","action":"%s",'
            '"payload":{"k1":%d,"k2":"%s","k3":%s},"ok":%s}\n'
        ) % (
            i,
            1700000000 + i,
            rng.randint(0, 999999),
            rng.choice(["open", "close", "sync", "fetch", "put", "delete"]),
            rng.randint(0, 10**6),
            "".join(rng.choice(string.ascii_lowercase) for _ in range(12)),
            rng.choice(["true", "false", "null"]),
            rng.choice(["true", "false"]),
        )
        b = rec.encode()
        out.append(b)
        size += len(b)
        i += 1
    return b"".join(out)[:n]


def gen_struct(n, rng):
    """结构化二进制: 整型序列小幅游走 -> DELTA 预处理可吃下, lzma2 也有得压"""
    buf = bytearray()
    v = rng.randint(0, 1 << 30)
    while len(buf) < n:
        v = (v + rng.randint(-64, 64)) & 0xFFFFFFFF
        buf += v.to_bytes(4, "little")
        buf += rng.randint(0, 255).to_bytes(1, "little")
    return bytes(buf[:n])


def gen_random(n, rng):
    """真随机: 不可压, 用于验证"原样存储"路径与压率上限"""
    return rng.randbytes(n)


def _bmp_header(w, h, bpp, data_off, file_size):
    import struct

    hdr = bytearray(54)
    hdr[0:2] = b"BM"
    struct.pack_into("<I", hdr, 2, file_size)
    struct.pack_into("<I", hdr, 10, data_off)
    struct.pack_into("<I", hdr, 14, 40)
    struct.pack_into("<i", hdr, 18, w)
    struct.pack_into("<i", hdr, 22, h)  # 正=自下而上
    struct.pack_into("<H", hdr, 26, 1)
    struct.pack_into("<H", hdr, 28, bpp)
    return hdr


def gen_bmp24(n, rng):
    """24bpp 未压缩 BMP, 宽取 101 -> 每行 303 字节像素 + 1 字节对齐填充。

    选这个宽度是故意的: 行跨距(304)不是 3 的倍数, 光栅变换一旦按"整块缓冲区
    每 3 字节一组"去色彩去相关, 通道相位就会逐行漂移, 竖直预测失效。回归里
    必须有能触发它的语料, 否则这条路径永远没人跑。
    """
    import math

    w, bpp = 101, 24
    ch = 3
    stride = ((w * ch + 3) // 4) * 4
    h = max(1, max(0, n - 54) // stride)
    px = bytearray(stride * h)  # 填充字节恒为 0, 正是真实 BMP 的样子
    for y in range(h):
        base = y * stride
        for x in range(w):
            v = 128 + 60 * math.sin(x / 13 + y / 29) * math.cos(y / 41) \
                + 20 * math.sin(x / 3 + 7 + y / 5)
            nz = (x * 31 + y * 17) % 7 - 3
            o = base + x * ch
            px[o] = max(0, min(255, int(v - 10 + nz / 2)))      # B
            px[o + 1] = max(0, min(255, int(v + nz)))           # G
            px[o + 2] = max(0, min(255, int(v + 8 + nz)))       # R
    return bytes(_bmp_header(w, h, bpp, 54, 54 + len(px))) + bytes(px)


def gen_bmp32(n, rng):
    """32bpp BMP: 行跨距天然 4 字节对齐(无填充), 与 24bpp 互为对照"""
    w, bpp = 101, 32
    ch = 4
    stride = w * ch
    h = max(1, max(0, n - 54) // stride)
    px = bytearray(stride * h)
    for y in range(h):
        base = y * stride
        for x in range(w):
            v = 128 + 55 * math.sin(x / 17 + y / 23) * math.cos(x / 37)
            nz = (x * 13 + y * 29) % 5 - 2
            o = base + x * ch
            px[o] = max(0, min(255, int(v - 12 + nz)))
            px[o + 1] = max(0, min(255, int(v + nz)))
            px[o + 2] = max(0, min(255, int(v + 9 + nz)))
            px[o + 3] = 255  # alpha 恒 255
    return bytes(_bmp_header(w, h, bpp, 54, 54 + len(px))) + bytes(px)


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "/tmp/hcaxlab/corpus"
    total = int(sys.argv[2]) if len(sys.argv) > 2 else 12 << 20
    # 第三个参数 noimg: 只生成非位图语料。兼容矩阵要用它 —— 位图会正当升到 v12,
    # 老版本按设计拒绝读取, 那就测不到"v8 归档双向互通"了
    noimg = len(sys.argv) > 3 and sys.argv[3] == "noimg"
    os.makedirs(root, exist_ok=True)
    rng = random.Random(20260912)

    parts = [
        ("novel.txt", gen_text, 0.26),
        ("source.go", gen_code, 0.18),
        ("records.json", gen_json, 0.22),
        ("series.bin", gen_struct, 0.14),
        ("random.bin", gen_random, 0.10),
        # 未压缩位图: 光栅变换(逐行 MED / 逐行色彩去相关)的唯一端到端覆盖点
        ("photo24.bmp", gen_bmp24, 0.06),
        ("photo32.bmp", gen_bmp32, 0.04),
    ]
    if noimg:
        parts = [p for p in parts if not p[0].endswith(".bmp")]
        # 去掉位图后把份额重新归一化, 保持总量不变
        s = sum(p[2] for p in parts)
        parts = [(n, f, w / s) for n, f, w in parts]
    for name, fn, frac in parts:
        n = max(1, int(total * frac))
        with open(os.path.join(root, name), "wb") as f:
            f.write(fn(n, rng))
        print(f"  {name:14s} {n:>10,} B")
    print(f"语料就绪: {root}")


if __name__ == "__main__":
    main()
