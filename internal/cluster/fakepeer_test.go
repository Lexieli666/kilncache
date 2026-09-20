package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// fakePeers is an in-memory cluster of peers that can be told to fail.
//
// A real network produces the interesting cases -- a peer that accepts a body
// and then dies before fsync, a peer that is reachable but takes longer than
// the hop timeout -- rarely and unpredictably. This produces them on demand and
// deterministically, which is the only way those paths get covered at all.
type fakePeers struct {
	mu sync.Mutex

	// objects[node][ns/key] = bytes
	objects map[string]map[string][]byte

	// down nodes refuse every operation with a transport error.
	down map[string]bool
	// rejecting nodes answer with an HTTP status, as a peer that is up but
	// unhappy would.
	rejecting map[string]int
	// slow nodes sleep before answering, to exercise the hop timeout.
	slow map[string]time.Duration
	// truncating nodes accept a body, report success, and store nothing.
	truncating map[string]bool

	puts  map[string]int
	gets  map[string]int
	stats map[string]int

	// hops records the hop role each peer was called with, which is how the
	// loop-prevention tests check that a coordinator only ever issues replica
	// writes.
	hops []protocol.Hop
}

func newFakePeers(nodes ...string) *fakePeers {
	f := &fakePeers{
		objects:    map[string]map[string][]byte{},
		down:       map[string]bool{},
		rejecting:  map[string]int{},
		slow:       map[string]time.Duration{},
		truncating: map[string]bool{},
		puts:       map[string]int{},
		gets:       map[string]int{},
		stats:      map[string]int{},
	}
	for _, n := range nodes {
		f.objects[n] = map[string][]byte{}
	}
	return f
}

func objKey(ns storage.Namespace, key string) string { return string(ns) + "/" + key }

func (f *fakePeers) setDown(node string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[node] = down
}

func (f *fakePeers) setRejecting(node string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejecting[node] = status
}

func (f *fakePeers) setSlow(node string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slow[node] = d
}

func (f *fakePeers) setTruncating(node string, t bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncating[node] = t
}

func (f *fakePeers) has(node string, ns storage.Namespace, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[node][objKey(ns, key)]
	return ok
}

func (f *fakePeers) put(node string, ns storage.Namespace, key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects[node] == nil {
		f.objects[node] = map[string][]byte{}
	}
	f.objects[node][objKey(ns, key)] = b
}

func (f *fakePeers) counts() (puts, gets, stats map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := func(m map[string]int) map[string]int {
		out := map[string]int{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return cp(f.puts), cp(f.gets), cp(f.stats)
}

func (f *fakePeers) recordedHops() []protocol.Hop {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]protocol.Hop, len(f.hops))
	copy(out, f.hops)
	return out
}

// gate applies the configured fault for a node before any real work happens.
func (f *fakePeers) gate(ctx context.Context, node, op string) error {
	f.mu.Lock()
	down := f.down[node]
	status := f.rejecting[node]
	delay := f.slow[node]
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return &PeerError{Peer: node, Op: op, Err: ctx.Err()}
		}
	}
	if down {
		return &PeerError{Peer: node, Op: op, Err: errors.New("connection refused")}
	}
	if status != 0 {
		return &PeerError{Peer: node, Op: op, Status: status, Body: "injected"}
	}
	return nil
}

func (f *fakePeers) Put(ctx context.Context, peer Member, ns storage.Namespace, key string, body io.Reader, size int64, hop protocol.Hop) error {
	f.mu.Lock()
	f.puts[peer.Name]++
	f.hops = append(f.hops, hop)
	f.mu.Unlock()

	if err := f.gate(ctx, peer.Name, "put"); err != nil {
		return err
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return &PeerError{Peer: peer.Name, Op: "put", Err: err}
	}
	if size >= 0 && int64(buf.Len()) != size {
		return &PeerError{Peer: peer.Name, Op: "put", Status: 400,
			Body: fmt.Sprintf("declared %d, got %d", size, buf.Len())}
	}

	f.mu.Lock()
	truncating := f.truncating[peer.Name]
	f.mu.Unlock()
	if truncating {
		// Accepted, acknowledged, stored nothing: the shape of a peer that
		// crashes between the write and the fsync.
		return nil
	}

	f.put(peer.Name, ns, key, buf.Bytes())
	return nil
}

func (f *fakePeers) Get(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectReader, error) {
	f.mu.Lock()
	f.gets[peer.Name]++
	f.hops = append(f.hops, hop)
	f.mu.Unlock()

	if err := f.gate(ctx, peer.Name, "get"); err != nil {
		return nil, err
	}

	f.mu.Lock()
	b, ok := f.objects[peer.Name][objKey(ns, key)]
	f.mu.Unlock()
	if !ok {
		return nil, ErrPeerNotFound
	}
	return &fakeReader{r: bytes.NewReader(b), size: int64(len(b)), source: peer.Name}, nil
}

func (f *fakePeers) Stat(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectInfo, error) {
	f.mu.Lock()
	f.stats[peer.Name]++
	f.hops = append(f.hops, hop)
	f.mu.Unlock()

	if err := f.gate(ctx, peer.Name, "stat"); err != nil {
		return protocol.ObjectInfo{}, err
	}
	f.mu.Lock()
	b, ok := f.objects[peer.Name][objKey(ns, key)]
	f.mu.Unlock()
	if !ok {
		return protocol.ObjectInfo{}, ErrPeerNotFound
	}
	return protocol.ObjectInfo{Size: int64(len(b)), Source: peer.Name}, nil
}

func (f *fakePeers) Close() {}

type fakeReader struct {
	r      *bytes.Reader
	size   int64
	source string
}

func (f *fakeReader) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeReader) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, f.r)
}
func (f *fakeReader) Close() error   { return nil }
func (f *fakeReader) Size() int64    { return f.size }
func (f *fakeReader) Source() string { return f.source }
func (f *fakeReader) Verify() error  { return nil }

var _ PeerClient = (*fakePeers)(nil)
var _ protocol.ObjectReader = (*fakeReader)(nil)
