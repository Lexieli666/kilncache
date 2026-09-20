package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
)

// Corpus is a deterministic set of objects.
//
// Deterministic because a benchmark whose workload differs between runs is
// comparing two things at once. The same seed produces the same bytes, so a
// number measured today can be compared with one measured next month, and a
// regression is a regression rather than a different corpus.
//
// The bytes are pseudo-random rather than zeros: a store that accidentally
// deduplicated, compressed, or truncated would look fast on a corpus of zeros
// and the benchmark would report the wrong thing.
type Corpus struct {
	Name    string
	Seed    int64
	Size    int
	Objects []Object
}

// Object is one benchmark object with its content and its key.
type Object struct {
	Key     string
	Content []byte
}

// NewCorpus builds count objects of size bytes each.
func NewCorpus(name string, count, size int, seed int64) *Corpus {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // reproducibility beats unpredictability in a fixture
	c := &Corpus{Name: name, Seed: seed, Size: size, Objects: make([]Object, count)}
	for i := 0; i < count; i++ {
		buf := make([]byte, size)
		_, _ = r.Read(buf)
		sum := sha256.Sum256(buf)
		c.Objects[i] = Object{Key: hex.EncodeToString(sum[:]), Content: buf}
	}
	return c
}

// Bytes is the total size of the corpus.
func (c *Corpus) Bytes() int64 { return int64(len(c.Objects)) * int64(c.Size) }

// MissKeys returns count keys that are valid but certainly absent, for
// measuring the miss path.
//
// They are derived by hashing a distinct prefix, so they are real SHA-256 keys
// that no corpus object can collide with. A miss measured against a malformed
// key would be measuring the parser, not the lookup.
func (c *Corpus) MissKeys(count int, seed int64) []string {
	r := rand.New(rand.NewSource(seed ^ 0x6d1553)) //nolint:gosec // fixture
	out := make([]string, count)
	for i := range out {
		buf := make([]byte, 32)
		_, _ = r.Read(buf)
		sum := sha256.Sum256(append([]byte("kilncache/bench/miss/"), buf...))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// Describe summarises the corpus for the result file.
func (c *Corpus) Describe() map[string]any {
	return map[string]any{
		"name":              c.Name,
		"objects":           len(c.Objects),
		"object_size_bytes": c.Size,
		"total_bytes":       c.Bytes(),
		"seed":              c.Seed,
		"content":           "pseudo-random from the seed; not zeros, so compression or dedup cannot flatter the result",
	}
}
