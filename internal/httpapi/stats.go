package httpapi

import (
	"encoding/json"
	"net/http"
	"time"
)

// StatsProvider supplies the node's counters. It is an interface so that the
// HTTP layer does not have to import every subsystem that has a counter.
type StatsProvider interface {
	Stats() map[string]any
}

// StatsHandler serves the node's counters as JSON at /stats.
//
// It exists separately from the Prometheus endpoint, and is available whether
// or not dev mode is on, because two very different consumers need it:
//
//   - a human in an incident, who wants one readable page and not an
//     exposition format;
//   - the chaos runner, which has to tell "this object was evicted under quota
//     pressure" apart from "this object was lost". Those two look identical
//     from outside -- a 404 -- and conflating them made a chaos run report
//     1,266 objects as data loss when the cache had simply done its job.
//
// It exposes counters only: no keys, no paths, no object contents.
func StatsHandler(node, version string, p StatsProvider) http.HandlerFunc {
	started := time.Now()
	return func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"node":           node,
			"version":        version,
			"uptime_seconds": time.Since(started).Seconds(),
			"now":            time.Now().UTC().Format(time.RFC3339Nano),
		}
		if p != nil {
			for k, v := range p.Stats() {
				body[k] = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(body)
	}
}
