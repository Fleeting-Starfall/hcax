package main

import (
	"crypto/sha256"
	"math/rand"
	"sync"
)

const (
	chunkBits = 16
	chunkMin  = 8 * 1024
	chunkMax  = 256 * 1024
	mask64    = (1 << chunkBits) - 1
)

// Incompressibility threshold: store raw only when cheap zstd-l1 output >= input
// (nothing to gain); as long as l1 shrinks it at all, hand it to the real compressor
// (which may do better), avoiding a wasted ratio opportunity

type chunker struct {
	rh      uint64
	start   int
	scanned int
	pending []byte
	minS    int
	maxS    int
	mask    uint64
	gear    [256]uint64
	onChunk func([]byte)
}

var gear = func() [256]uint64 {
	var g [256]uint64
	r := rand.New(rand.NewSource(0x9E3779B1))
	for i := range g {
		g[i] = r.Uint64()
	}
	return g
}()

func hash16(b []byte) [16]byte {
	h := sha256.Sum256(b)
	var out [16]byte
	copy(out[:], h[:16])
	return out
}

// ---------- content-adaptive preprocessing (DELTA / BCJ-x86) ----------
// Transform compressible blocks before the compressor, exposing "structured
// redundancy" to LZ/entropy coding; decode inverts the transform to restore the
// original bytes. Pure Go, self-contained and reversible, no xz filter decoding.

func newChunker(on func([]byte)) *chunker {
	return &chunker{minS: chunkMin, maxS: chunkMax, mask: mask64, gear: gear, onChunk: on}
}

func newChunkerMinMax(on func([]byte), mn, mx int) *chunker {
	return &chunker{minS: mn, maxS: mx, mask: mask64, gear: gear, onChunk: on}
}

// Reuse chunk buffers to avoid per-chunk make([]byte, ~64K) allocation spikes on large
// files (especially at high GOMAXPROCS). Safe to return because onChunk copies the data
// it needs into cm.data (even for xfNone).
var chunkPool = sync.Pool{New: func() interface{} { b := make([]byte, 0, chunkMax); return &b }}

func (c *chunker) emit(n int) {
	size := n - c.start
	var chunk []byte
	pp := chunkPool.Get().(*[]byte)
	if cap(*pp) >= size {
		chunk = (*pp)[:size]
	} else {
		chunk = make([]byte, size)
		pp = nil
	}
	copy(chunk, c.pending[c.start:n])
	c.onChunk(chunk)
	if pp != nil {
		*pp = (*pp)[:0]
		chunkPool.Put(pp)
	}
	rest := make([]byte, len(c.pending)-n)
	copy(rest, c.pending[n:])
	c.pending = rest
	c.start = 0
	c.scanned = 0
	c.rh = 0
}

func (c *chunker) write(p []byte) {
	c.pending = append(c.pending, p...)
	for {
		avail := len(c.pending) - c.start
		if avail < c.minS {
			return
		}
		end := c.start + c.maxS
		if end > len(c.pending) {
			end = len(c.pending)
		}
		cut := 0
		i := c.scanned
		for ; i < end; i++ {
			c.rh = (c.rh << 1) + c.gear[c.pending[i]]
			if i >= c.start+c.minS && (c.rh&c.mask) == 0 {
				cut = i + 1
				break
			}
		}
		c.scanned = i
		if cut > 0 {
			c.emit(cut)
			continue
		}
		if avail >= c.maxS {
			c.emit(end)
			continue
		}
		return
	}
}

func (c *chunker) flush() {
	if len(c.pending) > c.start {
		c.emit(len(c.pending))
	}
}

// ---------- compression backends ----------
