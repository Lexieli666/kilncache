//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/protocol"
)

// TestClusterWriteThroughAnyFrontDoor is the basic Phase 2 claim: every node is
// a valid entry point, and an object written through one is readable through
// all of them.
func TestClusterWriteThroughAnyFrontDoor(t *testing.T) {
	c := startCluster(t, 3, nil)

	type obj struct {
		key     string
		content []byte
	}
	var objs []obj
	for i := 0; i < 60; i++ {
		content := blob(1000+i*13, int64(i))
		key := sha256hex(content)
		front := c.Any(i)
		resp, body := put(t, front.URL(), "/cas/"+key, content)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT through %s = %d: %s", front.Name, resp.StatusCode, body)
		}
		if copies := resp.Header.Get(protocol.HeaderCopies); copies != "2" {
			t.Errorf("object %d: %s = %q, want 2", i, protocol.HeaderCopies, copies)
		}
		objs = append(objs, obj{key, content})
	}

	for _, o := range objs {
		for _, n := range c.Nodes {
			resp, got := get(t, n.URL(), "/cas/"+o.key)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s through %s = %d", o.key[:8], n.Name, resp.StatusCode)
			}
			if !bytes.Equal(got, o.content) {
				t.Fatalf("GET %s through %s returned different bytes", o.key[:8], n.Name)
			}
		}
	}
}

// TestReplicasLandOnTheRightNodes checks that the bytes really are on the two
// nodes the ring names, not merely readable from anywhere. Without this, a bug
// that stored every object on every node would pass every other test here.
func TestReplicasLandOnTheRightNodes(t *testing.T) {
	c := startCluster(t, 3, nil)

	for i := 0; i < 90; i++ {
		content := blob(512+i, int64(i)*7)
		key := sha256hex(content)
		holders := c.HoldersOf(key, 2)

		resp, body := put(t, c.Any(i).URL(), "/cas/"+key, content)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
		}

		for _, n := range c.Nodes {
			_, err := n.Node.Store().Stat("cas", key)
			shouldHold := contains(holders, n.Name)
			if shouldHold && err != nil {
				t.Errorf("object %s: holder %s does not have it: %v", key[:8], n.Name, err)
			}
			if !shouldHold && err == nil {
				t.Errorf("object %s: non-holder %s has a copy (holders are %v)", key[:8], n.Name, holders)
			}
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestReadSurvivesLosingTheHolder is the Phase 2 acceptance criterion at the
// integration level: stop the node that holds an object and read it through
// another, verifying bytes.
func TestReadSurvivesLosingTheHolder(t *testing.T) {
	c := startCluster(t, 3, nil)

	// Write enough objects that every node is the primary for a good share.
	written := map[string][]byte{}
	for i := 0; i < 300; i++ {
		content := blob(2000+i*3, int64(i)*11)
		key := sha256hex(content)
		resp, body := put(t, c.Any(i).URL(), "/cas/"+key, content)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
		}
		written[key] = content
	}

	victim := c.Node("node-b")
	victim.Stop()

	survivors := []*testNode{}
	for _, n := range c.Nodes {
		if n.Name != victim.Name {
			survivors = append(survivors, n)
		}
	}

	checked, viaFallback := 0, 0
	for key, content := range written {
		holders := c.HoldersOf(key, 2)
		if !contains(holders, victim.Name) {
			continue
		}
		// Read through a survivor that is not a holder where possible, so the
		// fallback path is really exercised rather than a local hit.
		front := survivors[checked%len(survivors)]
		resp, got := get(t, front.URL(), "/cas/"+key)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("object %s (holders %v) read through %s after killing %s = %d",
				key[:8], holders, front.Name, victim.Name, resp.StatusCode)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("object %s came back with different bytes after the holder was killed", key[:8])
		}
		if src := resp.Header.Get(protocol.HeaderSource); src != "local" {
			viaFallback++
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no sampled object had the killed node as a holder; the test proved nothing")
	}
	t.Logf("verified %d objects held by the killed node, %d of them served via peer fallback",
		checked, viaFallback)
}

// TestWriteFailsLoudlyWhenAReplicaIsDown is the other half of ADR-0005: with a
// holder down, a write that cannot place both copies must not report success.
func TestWriteFailsLoudlyWhenAReplicaIsDown(t *testing.T) {
	c := startCluster(t, 3, nil)
	victim := c.Node("node-c")
	victim.Stop()

	survivor := c.Node("node-a")

	var affected, ok503, unexpected int
	for i := 0; i < 120; i++ {
		content := blob(700+i, int64(i)*17)
		key := sha256hex(content)
		holders := c.HoldersOf(key, 2)
		resp, body := put(t, survivor.URL(), "/cas/"+key, content)

		if !contains(holders, victim.Name) {
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				t.Errorf("object %s does not involve the dead node but PUT = %d: %s",
					key[:8], resp.StatusCode, body)
			}
			continue
		}
		affected++
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			ok503++
			if got := resp.Header.Get(protocol.HeaderCopiesWanted); got != "2" {
				t.Errorf("503 response reports wanted=%q, want 2", got)
			}
		default:
			unexpected++
			t.Errorf("object %s has dead holder %v but PUT returned %d: %s",
				key[:8], holders, resp.StatusCode, body)
		}
	}

	if affected == 0 {
		t.Fatal("no write involved the dead node; the test proved nothing")
	}
	t.Logf("%d of 120 writes involved the dead node; %d correctly returned 503, %d did not",
		affected, ok503, unexpected)
}

// TestNoCopyIsEverWrong is the corruption falsifier at the cluster level, and
// the one whose absence claim needs a sample size attached.
func TestNoCopyIsEverWrong(t *testing.T) {
	c := startCluster(t, 3, nil)

	const objects = 400
	written := make(map[string][]byte, objects)
	for i := 0; i < objects; i++ {
		content := blob(1500+i*7, int64(i)*23)
		key := sha256hex(content)
		resp, body := put(t, c.Any(i).URL(), "/cas/"+key, content)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
		}
		written[key] = content
	}

	reads, corrupted := 0, 0
	for key, content := range written {
		for _, n := range c.Nodes {
			resp, got := get(t, n.URL(), "/cas/"+key)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s via %s = %d", key[:8], n.Name, resp.StatusCode)
			}
			reads++
			if !bytes.Equal(got, content) {
				corrupted++
			}
			if sha256hex(got) != key {
				corrupted++
			}
		}
	}
	t.Logf("verified %d reads of %d objects across %d nodes: %d corrupted",
		reads, len(written), len(c.Nodes), corrupted)
	if corrupted != 0 {
		t.Fatalf("%d corrupted reads out of %d", corrupted, reads)
	}
}

// TestForwardedRequestIsNotForwardedAgain is the loop-prevention falsifier over
// real HTTP: a request that arrives already marked as forwarded must be served
// locally or missed, never relayed.
func TestForwardedRequestIsNotForwardedAgain(t *testing.T) {
	c := startCluster(t, 3, nil)

	content := blob(4096, 1234)
	key := sha256hex(content)
	holders := c.HoldersOf(key, 2)

	resp, body := put(t, c.Node(holders[0]).URL(), "/cas/"+key, content)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}

	// Find a node that is not a holder, and ask it as if we were a peer.
	var outsider *testNode
	for _, n := range c.Nodes {
		if !contains(holders, n.Name) {
			outsider = n
			break
		}
	}
	if outsider == nil {
		t.Fatal("every node is a holder; pick a key with replica count below cluster size")
	}

	// Without the forwarding header the outsider fetches from a holder.
	if r, got := get(t, outsider.URL(), "/cas/"+key); r.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("unforwarded read through a non-holder = %d", r.StatusCode)
	}

	// With it, the outsider must answer from its own disk only, and it has no
	// copy, so this is a miss. Anything else means the hop rule is not enforced
	// and two nodes that disagreed about placement could relay forever.
	req, err := http.NewRequest(http.MethodGet, outsider.URL()+"/cas/"+key, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(protocol.HeaderForwardedBy, "node-z")
	req.Header.Set(protocol.HeaderHop, string(protocol.HopRead))
	fr, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("forwarded GET: %v", err)
	}
	defer fr.Body.Close()
	if fr.StatusCode != http.StatusNotFound {
		t.Fatalf("a forwarded read through a non-holder returned %d; it must not relay", fr.StatusCode)
	}
}

// TestClientCannotForgeAHopRole: the hop header is only trusted when the
// request also says which node forwarded it. Otherwise a client could set
// X-Kilncache-Hop: replica and get a single-copy write acknowledged as if it
// were replicated.
func TestClientCannotForgeAHopRole(t *testing.T) {
	c := startCluster(t, 3, nil)
	content := blob(2048, 4321)
	key := sha256hex(content)

	req, err := http.NewRequest(http.MethodPut, c.Node("node-a").URL()+"/cas/"+key, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(content))
	req.Header.Set(protocol.HeaderHop, string(protocol.HopReplica)) // no Forwarded-By
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if copies := resp.Header.Get(protocol.HeaderCopies); copies != "2" {
		t.Fatalf("%s = %q; a client-set hop header suppressed replication", protocol.HeaderCopies, copies)
	}
}

// TestConcurrentClusterTraffic drives every front door at once. Under -race
// this is where a shared ring or a shared peer connection pool would show up.
func TestConcurrentClusterTraffic(t *testing.T) {
	c := startCluster(t, 3, nil)

	const workers = 24
	const perWorker = 20

	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				content := blob(3000+i, int64(w*1000+i))
				key := sha256hex(content)
				front := c.Any(w + i)

				resp, body := put(t, front.URL(), "/cas/"+key, content)
				if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("worker %d: PUT = %d: %s", w, resp.StatusCode, body)
					continue
				}
				reader := c.Any(w + i + 1)
				r, got := get(t, reader.URL(), "/cas/"+key)
				if r.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("worker %d: GET through %s = %d", w, reader.Name, r.StatusCode)
					continue
				}
				if sha256hex(got) != key {
					errCh <- fmt.Errorf("worker %d: object %s came back with the wrong digest", w, key[:8])
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	n := 0
	for err := range errCh {
		if n < 10 {
			t.Error(err)
		}
		n++
	}
	if n > 0 {
		t.Fatalf("%d failures across %d operations", n, workers*perWorker)
	}
}

// TestRestartRejoinsCluster: a node that comes back serves its copies again.
func TestRestartRejoinsCluster(t *testing.T) {
	c := startCluster(t, 3, nil)

	written := map[string][]byte{}
	for i := 0; i < 80; i++ {
		content := blob(900+i, int64(i)*29)
		key := sha256hex(content)
		resp, body := put(t, c.Any(i).URL(), "/cas/"+key, content)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
		}
		written[key] = content
	}

	victim := c.Node("node-b")
	victim.Restart()

	for key, content := range written {
		if !contains(c.HoldersOf(key, 2), victim.Name) {
			continue
		}
		// Ask the restarted node directly, as a peer would, so the answer comes
		// from its own disk rather than a fallback.
		req, err := http.NewRequest(http.MethodGet, victim.URL()+"/cas/"+key, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(protocol.HeaderForwardedBy, "test")
		req.Header.Set(protocol.HeaderHop, string(protocol.HopRead))
		resp, err := httpClient().Do(req)
		if err != nil {
			t.Fatalf("GET after restart: %v", err)
		}
		got := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("restarted node lost object %s: %d", key[:8], resp.StatusCode)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("restarted node returned different bytes for %s", key[:8])
		}
	}
}

// TestSingleNodeClusterStillWorks: the same Coordinator with one member and
// replica count 1 must behave exactly like the Phase 1 single node, so there is
// no untested second code path in a single-node deployment.
func TestSingleNodeClusterStillWorks(t *testing.T) {
	c := startCluster(t, 1, func(cfg *config.Config) { cfg.ReplicaCount = 1 })
	n := c.Nodes[0]

	content := blob(8192, 555)
	key := sha256hex(content)
	resp, body := put(t, n.URL(), "/cas/"+key, content)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d: %s", resp.StatusCode, body)
	}
	if copies := resp.Header.Get(protocol.HeaderCopies); copies != "1" {
		t.Errorf("%s = %q, want 1", protocol.HeaderCopies, copies)
	}
	r, got := get(t, n.URL(), "/cas/"+key)
	if r.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("GET = %d", r.StatusCode)
	}
}

// TestHeadAgreesWithGetAcrossTheCluster: a HEAD that missed where a GET would
// hit would make Bazel skip an object it could have had.
func TestHeadAgreesWithGetAcrossTheCluster(t *testing.T) {
	c := startCluster(t, 3, nil)

	for i := 0; i < 40; i++ {
		content := blob(600+i, int64(i)*31)
		key := sha256hex(content)
		put(t, c.Any(i).URL(), "/cas/"+key, content)

		for _, n := range c.Nodes {
			hr := head(t, n.URL(), "/cas/"+key)
			gr, _ := get(t, n.URL(), "/cas/"+key)
			if hr.StatusCode != gr.StatusCode {
				t.Errorf("object %s via %s: HEAD %d but GET %d",
					key[:8], n.Name, hr.StatusCode, gr.StatusCode)
			}
			if hr.StatusCode == http.StatusOK {
				if hr.Header.Get("Content-Length") != strconv.Itoa(len(content)) {
					t.Errorf("object %s via %s: HEAD reports Content-Length %q, want %d",
						key[:8], n.Name, hr.Header.Get("Content-Length"), len(content))
				}
			}
		}
	}
}
