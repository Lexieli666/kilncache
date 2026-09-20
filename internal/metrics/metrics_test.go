package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", rec.Code, rec.Body.String())
	}
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScrapeIsValidExposition(t *testing.T) {
	m := New("node-a")
	m.ObserveRequest("get", "cas", "200", 3*time.Millisecond)

	body := scrape(t, m)
	for _, want := range []string{
		"# HELP kilncache_requests_total",
		"# TYPE kilncache_requests_total counter",
		`kilncache_requests_total{node="node-a",ns="cas",op="get",status="200"} 1`,
		"kilncache_request_duration_seconds_bucket",
		"go_goroutines",
		"process_open_fds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

// TestEveryMetricCarriesTheNodeLabel: a dashboard aggregating three nodes needs
// to tell them apart, and a metric that silently lacks the label disappears
// into an average.
func TestEveryMetricCarriesTheNodeLabel(t *testing.T) {
	m := New("node-c")
	if err := m.RegisterSources([]Source{
		{Name: "stored_bytes", Help: "h", Value: func() float64 { return 42 }},
		{Name: "evictions_total", Help: "h", Counter: true, Value: func() float64 { return 7 }},
	}); err != nil {
		t.Fatal(err)
	}
	m.ObserveRequest("put", "ac", "201", time.Millisecond)

	body := scrape(t, m)
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "kilncache_") {
			continue // runtime collectors are not ours to label
		}
		if !strings.Contains(line, `node="node-c"`) {
			t.Errorf("metric line has no node label: %s", line)
		}
	}
}

func TestSourcesReadLiveValues(t *testing.T) {
	m := New("node-a")
	value := 0.0
	if err := m.RegisterSources([]Source{
		{Name: "stored_bytes", Help: "h", Value: func() float64 { return value }},
	}); err != nil {
		t.Fatal(err)
	}

	if body := scrape(t, m); !strings.Contains(body, `kilncache_stored_bytes{node="node-a"} 0`) {
		t.Errorf("initial value not exposed:\n%s", body)
	}
	// The whole point of a Func collector: the value is read at scrape time, so
	// there is no second copy of the number to drift out of step.
	value = 1234
	if body := scrape(t, m); !strings.Contains(body, `kilncache_stored_bytes{node="node-a"} 1234`) {
		t.Errorf("updated value not exposed:\n%s", body)
	}
}

func TestSourcesWithDistinctLabels(t *testing.T) {
	m := New("node-a")
	const help = "Reads served, by where the bytes came from."
	if err := m.RegisterSources([]Source{
		{Name: "hits_total", Help: help, Counter: true, Labels: map[string]string{"source": "local"},
			Value: func() float64 { return 3 }},
		{Name: "hits_total", Help: help, Counter: true, Labels: map[string]string{"source": "peer"},
			Value: func() float64 { return 5 }},
	}); err != nil {
		t.Fatalf("two sources sharing a name but differing in labels must coexist: %v", err)
	}
	body := scrape(t, m)
	if !strings.Contains(body, `kilncache_hits_total{node="node-a",source="local"} 3`) {
		t.Errorf("local hits missing:\n%s", body)
	}
	if !strings.Contains(body, `kilncache_hits_total{node="node-a",source="peer"} 5`) {
		t.Errorf("peer hits missing:\n%s", body)
	}
}

// TestSameNameDifferentHelpIsRejectedClearly checks the error a misconfigured
// metric produces.
//
// Prometheus requires every series of a metric to share its help string, and
// the client library's own error names an internal descriptor. An earlier
// version of this test used the same help text for both series, so it passed
// while the production registration -- which used a different sentence per
// label value -- crashed every node at startup (docs/bugs.md, entry 10). The
// check now lives in RegisterSources and shows both strings.
func TestSameNameDifferentHelpIsRejectedClearly(t *testing.T) {
	m := New("node-a")
	err := m.RegisterSources([]Source{
		{Name: "hits_total", Help: "Reads served from the local disk.", Counter: true,
			Labels: map[string]string{"source": "local"}, Value: func() float64 { return 1 }},
		{Name: "hits_total", Help: "Reads served from a peer.", Counter: true,
			Labels: map[string]string{"source": "peer"}, Value: func() float64 { return 2 }},
	})
	if err == nil {
		t.Fatal("two help strings for one metric name = nil error")
	}
	for _, want := range []string{"hits_total", "local disk", "from a peer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so it is not actionable: %v", want, err)
		}
	}
}

// TestDuplicateSourceIsAnErrorNotAPanic: a duplicate registration must fail the
// node's startup with a message, not take down a running process on its first
// scrape.
func TestDuplicateSourceIsAnErrorNotAPanic(t *testing.T) {
	m := New("node-a")
	err := m.RegisterSources([]Source{
		{Name: "stored_bytes", Help: "h", Value: func() float64 { return 1 }},
		{Name: "stored_bytes", Help: "h", Value: func() float64 { return 2 }},
	})
	if err == nil {
		t.Fatal("registering the same metric twice = nil error")
	}
}

// TestTwoNodesInOneProcess is why the registry is private. With the default
// global registry this panics on the second New.
func TestTwoNodesInOneProcess(t *testing.T) {
	a := New("node-a")
	b := New("node-b")
	a.ObserveRequest("get", "cas", "200", time.Millisecond)
	b.ObserveRequest("get", "cas", "404", time.Millisecond)

	bodyA := scrape(t, a)
	bodyB := scrape(t, b)
	if !strings.Contains(bodyA, `node="node-a"`) || strings.Contains(bodyA, `node="node-b"`) {
		t.Error("node-a's registry is not isolated")
	}
	if !strings.Contains(bodyB, `node="node-b"`) || strings.Contains(bodyB, `node="node-a"`) {
		t.Error("node-b's registry is not isolated")
	}
}

func TestInFlightReturnsToZero(t *testing.T) {
	m := New("node-a")
	done := m.InFlight("get")
	if body := scrape(t, m); !strings.Contains(body, `kilncache_requests_in_flight{node="node-a",op="get"} 1`) {
		t.Errorf("in-flight not incremented:\n%s", body)
	}
	done()
	if body := scrape(t, m); !strings.Contains(body, `kilncache_requests_in_flight{node="node-a",op="get"} 0`) {
		t.Errorf("in-flight not decremented:\n%s", body)
	}
}

func TestObserveForwardedDefaultsToClient(t *testing.T) {
	m := New("node-a")
	m.ObserveForwarded("")
	m.ObserveForwarded("replica")
	body := scrape(t, m)
	if !strings.Contains(body, `kilncache_forwarded_requests_total{hop="client",node="node-a"} 1`) {
		t.Errorf("empty hop was not labelled as client:\n%s", body)
	}
	if !strings.Contains(body, `kilncache_forwarded_requests_total{hop="replica",node="node-a"} 1`) {
		t.Errorf("replica hop missing:\n%s", body)
	}
}

// TestLatencyBucketsCoverTheRangeThatMatters: the default bucket set starts at
// 5 ms, which would put every warm cache hit in one bucket and make a p99
// unreadable at the low end.
func TestLatencyBucketsCoverTheRangeThatMatters(t *testing.T) {
	m := New("node-a")
	m.ObserveRequest("get", "cas", "200", 300*time.Microsecond)
	body := scrape(t, m)
	for _, want := range []string{`le="0.0005"`, `le="0.001"`, `le="5"`, `le="10"`} {
		if !strings.Contains(body, want) {
			t.Errorf("histogram has no bucket %s", want)
		}
	}
	// A 300 µs observation must land below the 0.0005 bucket, not above it.
	if !strings.Contains(body, `kilncache_request_duration_seconds_bucket{node="node-a",ns="cas",op="get",le="0.0005"} 1`) {
		t.Errorf("a 300us request did not land in the 500us bucket:\n%s", body)
	}
}
