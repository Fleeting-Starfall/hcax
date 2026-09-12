#!/usr/bin/env python3
"""生成回归测试语料。

设计要点:
- 可压(不是随机数据, 否则会被判为"原样存储", 绕开压缩器)
- 非重复(否则 CDC 去重会把语料折叠成几十块, 测不出真实内存/耗时)
- 类型多样(文本/代码/JSON/结构化二进制/不可压图片), 贴近真实归档场景
"""
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


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "/tmp/hcaxlab/corpus"
    total = int(sys.argv[2]) if len(sys.argv) > 2 else 12 << 20
    os.makedirs(root, exist_ok=True)
    rng = random.Random(20260912)

    parts = [
        ("novel.txt", gen_text, 0.30),
        ("source.go", gen_code, 0.20),
        ("records.json", gen_json, 0.25),
        ("series.bin", gen_struct, 0.15),
        ("random.bin", gen_random, 0.10),
    ]
    for name, fn, frac in parts:
        n = max(1, int(total * frac))
        with open(os.path.join(root, name), "wb") as f:
            f.write(fn(n, rng))
        print(f"  {name:14s} {n:>10,} B")
    print(f"语料就绪: {root}")


if __name__ == "__main__":
    main()
