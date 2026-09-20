// Package metrics defines the Prometheus collectors KilnCache exposes.
//
// The set is chosen so that the three questions an operator actually asks in an
// incident can be answered from the metrics alone:
//
//   - Is the cache serving? (requests, statuses, latency, hit ratio)
//   - Is it losing data or lying? (verification failures, insufficient replicas)
//   - Why is it slow or full? (bytes stored against quota, eviction rate,
//     repair backlog, replication failures)
//
// Nothing here is labelled by object key. A cache with a million objects would
// produce a million time series, which is how monitoring systems get taken down
// by the thing they are monitoring.
package metrics

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "kilncache"

// Metrics holds the collectors that the request path updates directly.
//
// Everything else is registered as a *Func collector reading the atomic
// counters the subsystems already keep. That is deliberate: the alternative is
// a second set of counters incremented alongside the first, which is twice the
// bookkeeping and, more importantly, two numbers that can disagree. When
// /stats and /metrics disagree during an incident, the time goes into deciding
// which one to believe rather than into the incident.
type Metrics struct {
	reg  *prometheus.Registry
	node string

	RequestsTotal    *prometheus.CounterVec
	RequestDuration  *prometheus.HistogramVec
	RequestsInFlight *prometheus.GaugeVec
	Forwarded        *prometheus.CounterVec
}

// New builds the request-path collectors on a private registry.
//
// Private rather than the default registry: the default one is process-global
// mutable state, so two nodes in one test process would panic on duplicate
// registration, and any dependency that registers a collector would silently
// appear in this service's metrics.
func New(node string) *Metrics {
	reg := prometheus.NewRegistry()
	factory := prometheus.WrapRegistererWith(prometheus.Labels{"node": node}, reg)

	m := &Metrics{reg: reg, node: node}

	m.RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "requests_total",
		Help: "Cache protocol requests by operation, namespace and status.",
	}, []string{"op", "ns", "status"})

	// Buckets span 100 µs to 10 s. The low end matters because a warm 64 KiB
	// hit should be well under a millisecond and the default bucket set would
	// put every one of them in the same bucket; the high end matters because a
	// large object transfer legitimately takes seconds and must not disappear
	// into +Inf.
	m.RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "request_duration_seconds",
		Help: "Cache protocol request latency by operation and namespace.",
		Buckets: []float64{
			0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05,
			0.1, 0.25, 0.5, 1, 2.5, 5, 10,
		},
	}, []string{"op", "ns"})

	m.RequestsInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "requests_in_flight",
		Help: "Cache protocol requests currently being served.",
	}, []string{"op"})

	m.Forwarded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "forwarded_requests_total",
		Help: "Requests received from another node, by hop role.",
	}, []string{"hop"})

	factory.MustRegister(m.RequestsTotal, m.RequestDuration, m.RequestsInFlight, m.Forwarded)

	// Go runtime and process collectors: heap, goroutines, GC, open file
	// descriptors. The descriptor count is the one that matters most here -- a
	// node that leaks one per forwarded request dies hours later with a
	// confusing error, and this makes it visible beforehand.
	factory.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Source describes one derived metric: a name, a help string, and a function
// that reads the value from wherever it already lives.
type Source struct {
	Name    string
	Help    string
	Counter bool // true for monotonic counters, false for gauges
	Value   func() float64
	Labels  prometheus.Labels
}

// RegisterSources registers derived metrics that read existing counters.
//
// It is called once at startup with everything the node knows how to report.
// Registering a duplicate returns an error rather than panicking, so a mistake
// here fails the node's startup with a clear message instead of taking down a
// running process on its first scrape.
func (m *Metrics) RegisterSources(sources []Source) error {
	// Prometheus requires every series sharing a metric name to share its help
	// string too. Two sources that differ only in labels -- hits_total for
	// local and for peer, say -- must therefore describe the metric, not the
	// particular series. The client library's own message for this names an
	// internal descriptor and is hard to act on, so the check happens here
	// where both help strings can be shown side by side.
	helps := make(map[string]string, len(sources))
	for _, s := range sources {
		if prev, ok := helps[s.Name]; ok && prev != s.Help {
			return fmt.Errorf(
				"metric %q registered twice with different help strings; every series of one "+
					"metric must share one help string describing the metric, not the series:\n  %q\n  %q",
				s.Name, prev, s.Help)
		}
		helps[s.Name] = s.Help
	}

	base := prometheus.WrapRegistererWith(prometheus.Labels{"node": m.node}, m.reg)
	for _, s := range sources {
		reg := base
		if len(s.Labels) > 0 {
			reg = prometheus.WrapRegistererWith(s.Labels, reg)
		}
		opts := prometheus.Opts{Namespace: namespace, Name: s.Name, Help: s.Help}
		var c prometheus.Collector
		if s.Counter {
			c = prometheus.NewCounterFunc(prometheus.CounterOpts(opts), s.Value)
		} else {
			c = prometheus.NewGaugeFunc(prometheus.GaugeOpts(opts), s.Value)
		}
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// Registry exposes the registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		// Scrapes are not free, and a monitoring system that retries a slow
		// scrape can amplify a problem into an outage.
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	})
}

// ObserveRequest records one completed cache request.
func (m *Metrics) ObserveRequest(op, ns, status string, d time.Duration) {
	m.RequestsTotal.WithLabelValues(op, ns, status).Inc()
	m.RequestDuration.WithLabelValues(op, ns).Observe(d.Seconds())
}

// InFlight returns a function that decrements the in-flight gauge, so callers
// can write `defer m.InFlight(op)()`.
func (m *Metrics) InFlight(op string) func() {
	g := m.RequestsInFlight.WithLabelValues(op)
	g.Inc()
	return g.Dec
}

// ObserveForwarded records a request that arrived from another node.
func (m *Metrics) ObserveForwarded(hop string) {
	if hop == "" {
		hop = "client"
	}
	m.Forwarded.WithLabelValues(hop).Inc()
}
