package main

import (
	"math"
	"sort"
	"time"
)

// Latencies collects every observation rather than sampling.
//
// A benchmark that reports p99 from a reservoir sample is reporting an estimate
// of a tail, and the tail is the interesting part. At the scale these runs
// reach -- hundreds of thousands of observations at 8 bytes each -- keeping all
// of them costs a few megabytes and removes a whole class of "is that number
// real?" from the results.
type Latencies struct {
	samples []time.Duration
}

// NewLatencies preallocates for an expected number of observations.
func NewLatencies(expect int) *Latencies {
	if expect < 1024 {
		expect = 1024
	}
	return &Latencies{samples: make([]time.Duration, 0, expect)}
}

// Add records one observation.
func (l *Latencies) Add(d time.Duration) { l.samples = append(l.samples, d) }

// Merge folds another collector in, for combining per-worker results.
func (l *Latencies) Merge(o *Latencies) { l.samples = append(l.samples, o.samples...) }

// Len is the number of observations.
func (l *Latencies) Len() int { return len(l.samples) }

// Summary is the distribution, in milliseconds.
type Summary struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"min_ms"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P90Ms  float64 `json:"p90_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	P999Ms float64 `json:"p999_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// Summarize sorts the observations and computes the distribution.
func (l *Latencies) Summarize() Summary {
	if len(l.samples) == 0 {
		return Summary{}
	}
	sort.Slice(l.samples, func(i, j int) bool { return l.samples[i] < l.samples[j] })

	var total time.Duration
	for _, d := range l.samples {
		total += d
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

	return Summary{
		Count:  len(l.samples),
		MinMs:  ms(l.samples[0]),
		MeanMs: ms(total / time.Duration(len(l.samples))),
		P50Ms:  ms(l.percentile(0.50)),
		P90Ms:  ms(l.percentile(0.90)),
		P95Ms:  ms(l.percentile(0.95)),
		P99Ms:  ms(l.percentile(0.99)),
		P999Ms: ms(l.percentile(0.999)),
		MaxMs:  ms(l.samples[len(l.samples)-1]),
	}
}

// percentile uses nearest-rank on the sorted samples.
//
// Nearest-rank rather than interpolation because an interpolated p99 is a value
// that was never observed, and for a latency tail the observed value is the
// honest one.
func (l *Latencies) percentile(p float64) time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(len(l.samples))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(l.samples) {
		rank = len(l.samples)
	}
	return l.samples[rank-1]
}
