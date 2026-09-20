package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
)

// injectFaults stops and restarts one node at a time on a schedule.
//
// One at a time, never two. With replica count 2 over three nodes, stopping two
// simultaneously can leave an object with no live holder, and a read that then
// fails is correct behaviour rather than a bug -- so a run that did it would
// produce failures the report could not interpret. The interesting question is
// what happens when the cluster is degraded but still able to answer, and that
// is a single node down.
func injectFaults(ctx context.Context, opts options, record func(faultEvent)) {
	r := rand.New(rand.NewSource(opts.seed ^ 0x5eed))
	ticker := time.NewTicker(opts.faultEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		victim := nodeOrder[r.Intn(len(nodeOrder))].Name
		ev := faultEvent{Node: victim, StoppedAt: time.Now().UTC()}

		fmt.Fprintf(os.Stderr, "kilnchaos: stopping %s\n", victim)
		if out, err := compose(opts, "stop", "-t", "3", victim); err != nil {
			ev.StopErr = fmt.Sprintf("%v: %s", err, strings.TrimSpace(out))
			fmt.Fprintf(os.Stderr, "kilnchaos: stop %s failed: %s\n", victim, ev.StopErr)
			record(ev)
			continue
		}

		select {
		case <-ctx.Done():
			// Always bring it back, even on cancellation: leaving a node down
			// would make the convergence measurement meaningless and leave the
			// operator's cluster broken.
		case <-time.After(opts.faultDown):
		}

		fmt.Fprintf(os.Stderr, "kilnchaos: starting %s\n", victim)
		if out, err := compose(opts, "start", victim); err != nil {
			ev.StartErr = fmt.Sprintf("%v: %s", err, strings.TrimSpace(out))
			fmt.Fprintf(os.Stderr, "kilnchaos: start %s failed: %s\n", victim, ev.StartErr)
		}
		ev.StartedAt = time.Now().UTC()
		ev.DownFor = ev.StartedAt.Sub(ev.StoppedAt).Round(time.Millisecond).String()
		record(ev)

		if ctx.Err() != nil {
			return
		}
	}
}

func compose(opts options, args ...string) (string, error) {
	full := append([]string{"compose", "-p", opts.composeProject, "-f", opts.composeFile}, args...)
	cmd := exec.Command("docker", full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// restoreAll starts every node, whatever state the run left them in.
func restoreAll(ctx context.Context, opts options) error {
	if opts.noFaults {
		return nil
	}
	var firstErr error
	for _, n := range nodeOrder {
		if out, err := compose(opts, "start", n.Name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("start %s: %v: %s", n.Name, err, strings.TrimSpace(out))
			}
		}
	}
	_ = ctx
	return firstErr
}

func waitAllReady(ctx context.Context, client *http.Client, within time.Duration) error {
	deadline := time.Now().Add(within)
	for _, n := range nodeOrder {
		ok := false
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.URL+"/readyz", http.NoBody)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ok = true
					break
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !ok {
			return fmt.Errorf("node %s at %s did not become ready within %v", n.Name, n.URL, within)
		}
	}
	return nil
}

// measureConvergence asks every node directly whether it holds each object, and
// waits until every object has at least two copies.
//
// It asks with the forwarding header set, so each node answers from its own
// disk rather than fetching from a peer. Without that, every node would report
// "yes" for every object and the measurement would be vacuous.
func measureConvergence(ctx context.Context, client *http.Client, opts options, led *ledger, rep *report) {
	acked := led.all()
	refused := led.refusedKeys()
	if len(acked) == 0 && len(refused) == 0 {
		return
	}

	// The sample is weighted towards objects left behind by refused writes.
	//
	// An acknowledged write had two copies at the moment it was acknowledged,
	// and a stopped container comes back with its named volume intact, so those
	// objects are replicated by construction and prove nothing about repair. A
	// refused write is the interesting case: the coordinator kept its copy, the
	// peer never got one, and the only thing that can fix that is the repair
	// worker noticing after the peer returns. Every refused object is checked,
	// up to the cap, and acknowledged ones fill the rest as a control.
	const maxSample = 3000
	sort.Strings(refused)
	sort.Strings(acked)

	sample := make([]string, 0, maxSample)
	sample = append(sample, subsample(refused, maxSample*2/3)...)
	rep.Convergence.FromRefusedWrites = len(sample)
	sample = append(sample, subsample(acked, maxSample-len(sample))...)
	rep.Convergence.FromAckedWrites = len(sample) - rep.Convergence.FromRefusedWrites

	rep.Convergence.Attempted = true
	rep.Convergence.ObjectsChecked = len(sample)
	rep.Convergence.Note = "objects from refused (503) writes are the ones that were genuinely " +
		"under-replicated: the coordinator kept a copy and the down peer never got one. " +
		"Acknowledged writes are included as a control and are replicated by construction. " +
		"An object absent from every node is not a failure for a refused write -- nothing was promised."

	start := time.Now()
	deadline := start.Add(opts.convergeWait)
	fmt.Fprintf(os.Stderr, "kilnchaos: waiting up to %v for %d sampled objects to reach 2 replicas (%d from refused writes)\n",
		opts.convergeWait, len(sample), rep.Convergence.FromRefusedWrites)

	first := true
	for {
		full, under, absent, unreadable := countReplicas(ctx, client, sample)
		if first {
			rep.Convergence.AtStartReplicated = full
			first = false
		}
		rep.Convergence.FullyReplicated = full
		rep.Convergence.UnderReplicated = under
		rep.Convergence.Absent = absent
		rep.Convergence.Unreadable = unreadable
		rep.Convergence.WaitedSeconds = time.Since(start).Seconds()

		if under == 0 && unreadable == 0 {
			rep.Convergence.Converged = true
			rep.Convergence.ConvergedSeconds = time.Since(start).Seconds()
			fmt.Fprintf(os.Stderr, "kilnchaos: converged after %.1fs: %d objects at >= 2 replicas, %d absent everywhere, 0 under-replicated\n",
				rep.Convergence.ConvergedSeconds, full, absent)
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "kilnchaos: did NOT converge: %d under-replicated, %d unreadable after %.0fs\n",
				under, unreadable, rep.Convergence.WaitedSeconds)
			return
		}
		fmt.Fprintf(os.Stderr, "kilnchaos: %d at 2+ replicas, %d under-replicated, %d absent, %d unreadable (%.0fs elapsed)\n",
			full, under, absent, unreadable, time.Since(start).Seconds())
		time.Sleep(5 * time.Second)
	}
}

// subsample takes up to n evenly spaced elements.
func subsample(keys []string, n int) []string {
	if n <= 0 || len(keys) == 0 {
		return nil
	}
	if len(keys) <= n {
		out := make([]string, len(keys))
		copy(out, keys)
		return out
	}
	step := len(keys) / n
	out := make([]string, 0, n)
	for i := 0; i < len(keys) && len(out) < n; i += step {
		out = append(out, keys[i])
	}
	return out
}

// countReplicas asks each node, locally, whether it holds each object.
func countReplicas(ctx context.Context, client *http.Client, keys []string) (full, under, absent, unreadable int) {
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		copies := 0
		errs := 0
		for _, n := range nodeOrder {
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, n.URL+"/cas/"+key, http.NoBody)
			if err != nil {
				errs++
				continue
			}
			// Terminal hop: answer from your own disk, do not go asking peers.
			req.Header.Set(protocol.HeaderForwardedBy, "kilnchaos")
			req.Header.Set(protocol.HeaderHop, string(protocol.HopRead))
			resp, err := client.Do(req)
			if err != nil {
				errs++
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				copies++
			}
		}
		switch {
		case errs == len(nodeOrder):
			unreadable++
		case copies >= 2:
			full++
		case copies == 0:
			// Present nowhere. For a refused write that is correct -- nothing
			// was promised -- and for an acknowledged one the final
			// verification pass reports it as missing. Either way it is not
			// something repair can converge, so it is counted separately rather
			// than held against convergence forever.
			absent++
		default:
			under++
		}
	}
	return full, under, absent, unreadable
}

// collectNodeStats reads each node's own counters from /stats.
//
// Without them, a missing object at the end of a run is unattributable: a 404
// looks identical whether the object was evicted under quota pressure or lost.
// The eviction totals here are what let the verdict tell those apart.
func collectNodeStats(ctx context.Context, client *http.Client, rep *report) {
	stats := map[string]any{}
	for _, n := range nodeOrder {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.URL+"/stats", http.NoBody)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var body map[string]any
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		resp.Body.Close()
		if decodeErr != nil {
			continue
		}
		stats[n.Name] = body

		if ev, ok := body["eviction"].(map[string]any); ok {
			rep.Eviction.ObjectsEvicted += asInt64(ev["evicted"])
			rep.Eviction.BytesReclaimed += asInt64(ev["bytes_reclaimed"])
		}
		if usage, ok := body["usage"].(map[string]any); ok {
			if over, ok := usage["over_high_water"].(bool); ok && over {
				rep.Eviction.AnyNodeOverHighWater = true
			}
		}
	}
	rep.NodeStats = stats
	rep.Eviction.Observed = rep.Eviction.ObjectsEvicted > 0

	if rep.Eviction.Observed {
		rep.FinalVerification.Note = "Eviction was active during this run, so an object that is " +
			"missing at the end may have been evicted under quota pressure rather than lost. " +
			"See eviction.objects_evicted and node_stats."
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

// finalVerify re-reads every acknowledged object and checks it against the
// ledger, after the cluster has been restored.
func finalVerify(ctx context.Context, client *http.Client, opts options, led *ledger, c *counters, rep *report) {
	keys := led.all()
	const maxSample = 5000
	sample := keys
	if len(sample) > maxSample {
		step := len(sample) / maxSample
		picked := make([]string, 0, maxSample)
		for i := 0; i < len(sample); i += step {
			picked = append(picked, sample[i])
		}
		sample = picked
	}
	rep.FinalVerification.ObjectsChecked = len(sample)

	for i, key := range sample {
		if ctx.Err() != nil {
			break
		}
		base := nodeOrder[i%len(nodeOrder)].URL
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/cas/"+key, http.NoBody)
		if err != nil {
			rep.FinalVerification.Unreadable++
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			rep.FinalVerification.Unreadable++
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			rep.FinalVerification.Missing++
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			rep.FinalVerification.Unreadable++
			continue
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			rep.FinalVerification.Unreadable++
			continue
		}
		c.ReadsVerified.Add(1)
		if hex.EncodeToString(h.Sum(nil)) == key {
			rep.FinalVerification.Correct++
		} else {
			rep.FinalVerification.Corrupted++
			c.CorruptedReads.Add(1)
		}
	}
	_ = opts
}

func writeReport(dir string, rep report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create report dir: %w", err)
	}
	name := fmt.Sprintf("chaos-%s.json", rep.GeneratedAt.Format("20060102-150405"))
	path := filepath.Join(dir, name)

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr, "kilnchaos: wrote %s\n", path)

	// Also write/overwrite a stable name so docs can link to the latest run.
	stable := filepath.Join(dir, "chaos-latest.json")
	if err := os.WriteFile(stable, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write latest report: %w", err)
	}
	return nil
}

func printSummary(rep *report) {
	out := os.Stdout
	fmt.Fprintf(out, "\n=== kilnchaos report ===\n")
	fmt.Fprintf(out, "duration            %.0fs across %d nodes, %d workers, seed %d\n",
		rep.Config.DurationS, len(rep.Config.Nodes), rep.Config.Workers, rep.Config.Seed)
	fmt.Fprintf(out, "faults              %d node stops\n", len(rep.Faults))
	fmt.Fprintf(out, "writes              %d ok, %d refused with 503, %d errors\n",
		rep.Traffic.WritesOK, rep.Traffic.WritesRefused, rep.Traffic.WriteErrors)
	fmt.Fprintf(out, "reads               %d hits, %d misses, %d errors\n",
		rep.Traffic.ReadHits, rep.Traffic.ReadMisses, rep.Traffic.ReadErrors)
	fmt.Fprintf(out, "throughput          %.0f ops/s, error rate %.4f\n",
		rep.Traffic.OpsPerSecond, rep.Traffic.ErrorRate)
	fmt.Fprintf(out, "errors by class     %d connection refused, %d timeouts, %d server 5xx\n",
		rep.Traffic.ConnRefused, rep.Traffic.Timeouts, rep.Traffic.ServerErrors)
	fmt.Fprintf(out, "CORRUPTED READS     %d out of %d reads verified\n",
		rep.Correctness.CorruptedReads, rep.Correctness.ReadsVerified)
	if rep.Convergence.Attempted {
		fmt.Fprintf(out, "convergence         %d/%d sampled objects at >= 2 replicas after %.1fs (converged: %v)\n",
			rep.Convergence.FullyReplicated, rep.Convergence.ObjectsChecked,
			rep.Convergence.WaitedSeconds, rep.Convergence.Converged)
	}
	fmt.Fprintf(out, "final verification  %d correct, %d corrupted, %d missing, of %d checked\n",
		rep.FinalVerification.Correct, rep.FinalVerification.Corrupted,
		rep.FinalVerification.Missing, rep.FinalVerification.ObjectsChecked)
	fmt.Fprintf(out, "verdict             %s\n\n", rep.Verdict)
}
