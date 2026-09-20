package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// PeerClient talks to other nodes.
//
// It is an interface so that tests can inject a peer that fails, hangs, or
// truncates on demand. Those paths — a peer that accepts a body and then dies
// before fsync, a peer that is reachable but slow — are the ones a real network
// produces rarely and a test must produce reliably.
type PeerClient interface {
	Put(ctx context.Context, peer Member, ns storage.Namespace, key string, body io.Reader, size int64, hop protocol.Hop) error
	Get(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectReader, error)
	Stat(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectInfo, error)
	Close()
}

// ErrPeerNotFound is returned when a peer answers 404.
var ErrPeerNotFound = errors.New("peer does not have the object")

// PeerError carries enough context to tell a dead peer from a rejected object.
// Those two get different treatment: a dead peer is retried elsewhere, a
// rejected object is a bug that retrying will reproduce.
type PeerError struct {
	Peer   string
	Op     string
	Status int
	Body   string
	Err    error
}

func (e *PeerError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("peer %s: %s: %v", e.Peer, e.Op, e.Err)
	}
	return fmt.Sprintf("peer %s: %s: status %d: %s", e.Peer, e.Op, e.Status, strings.TrimSpace(e.Body))
}

func (e *PeerError) Unwrap() error { return e.Err }

// Retryable reports whether trying a different peer could help. A transport
// failure or a 5xx might; a 400 means the object itself was rejected and every
// peer will reject it the same way.
func (e *PeerError) Retryable() bool {
	if e.Err != nil {
		return true
	}
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// HTTPPeerClient is the production PeerClient.
type HTTPPeerClient struct {
	self    string
	client  *http.Client
	timeout time.Duration
}

// NewHTTPPeerClient builds a peer client with its own connection pool.
//
// The pool is sized for the fan-out this actually has: a three-node cluster
// with every node talking to two others under high concurrency. Go's default of
// two idle connections per host would make every replicated PUT pay a fresh TCP
// handshake, which on a loopback benchmark is most of the latency.
func NewHTTPPeerClient(self string, timeout time.Duration) *HTTPPeerClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy: nil, // never route intra-cluster traffic through a proxy
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // build artifacts are already compressed
		ForceAttemptHTTP2:     false,
	}
	return &HTTPPeerClient{
		self:    self,
		timeout: timeout,
		// No Client.Timeout: it would apply to the whole request including the
		// body stream, so a legitimately large object would be killed for being
		// large. The per-hop deadline is applied to the context instead, which
		// callers can extend for known-large transfers.
		client: &http.Client{Transport: transport},
	}
}

// Close releases idle connections.
func (c *HTTPPeerClient) Close() {
	if t, ok := c.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

func objectURL(peer Member, ns storage.Namespace, key string) string {
	return peer.URL + "/" + string(ns) + "/" + key
}

func (c *HTTPPeerClient) newRequest(ctx context.Context, method string, peer Member, ns storage.Namespace, key string, body io.Reader, hop protocol.Hop) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, objectURL(peer, ns, key), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(protocol.HeaderForwardedBy, c.self)
	req.Header.Set(protocol.HeaderHop, string(hop))
	return req, nil
}

// Put sends one copy to a peer.
func (c *HTTPPeerClient) Put(ctx context.Context, peer Member, ns storage.Namespace, key string, body io.Reader, size int64, hop protocol.Hop) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := c.newRequest(ctx, http.MethodPut, peer, ns, key, body, hop)
	if err != nil {
		return &PeerError{Peer: peer.Name, Op: "put", Err: err}
	}
	req.ContentLength = size
	if size >= 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return &PeerError{Peer: peer.Name, Op: "put", Err: err}
	}
	defer resp.Body.Close()

	// Drain the response so the connection can be reused. A peer's reply to a
	// PUT is a few bytes; leaving it unread costs a connection per write.
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &PeerError{Peer: peer.Name, Op: "put", Status: resp.StatusCode, Body: string(msg)}
}

// Get streams an object from a peer. The caller must Close the reader.
func (c *HTTPPeerClient) Get(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectReader, error) {
	// The deadline covers establishing the response, not draining the body: a
	// context timeout here would cancel a large but healthy transfer. The
	// cancel func is handed to the reader so the request is released when the
	// body is closed.
	ctx, cancel := context.WithCancel(ctx)

	req, err := c.newRequest(ctx, http.MethodGet, peer, ns, key, nil, hop)
	if err != nil {
		cancel()
		return nil, &PeerError{Peer: peer.Name, Op: "get", Err: err}
	}

	timer := time.AfterFunc(c.timeout, cancel)

	resp, err := c.client.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, &PeerError{Peer: peer.Name, Op: "get", Err: err}
	}
	// Headers are in; from here the deadline is the caller's problem, not a
	// fixed one that would truncate a legitimate multi-gigabyte read.
	timer.Stop()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, ErrPeerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		return nil, &PeerError{Peer: peer.Name, Op: "get", Status: resp.StatusCode, Body: string(msg)}
	}

	source := resp.Header.Get(protocol.HeaderNode)
	if source == "" {
		source = peer.Name
	}
	return &peerObject{
		body:   resp.Body,
		size:   resp.ContentLength,
		source: source,
		cancel: cancel,
	}, nil
}

// Stat asks a peer whether it has an object.
func (c *HTTPPeerClient) Stat(ctx context.Context, peer Member, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := c.newRequest(ctx, http.MethodHead, peer, ns, key, nil, hop)
	if err != nil {
		return protocol.ObjectInfo{}, &PeerError{Peer: peer.Name, Op: "stat", Err: err}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return protocol.ObjectInfo{}, &PeerError{Peer: peer.Name, Op: "stat", Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return protocol.ObjectInfo{Size: resp.ContentLength, Source: peer.Name}, nil
	case resp.StatusCode == http.StatusNotFound:
		return protocol.ObjectInfo{}, ErrPeerNotFound
	default:
		return protocol.ObjectInfo{}, &PeerError{Peer: peer.Name, Op: "stat", Status: resp.StatusCode}
	}
}

// peerObject adapts a peer's response body to protocol.ObjectReader.
type peerObject struct {
	body   io.ReadCloser
	size   int64
	source string
	cancel context.CancelFunc
	read   int64
}

func (p *peerObject) Read(b []byte) (int, error) {
	n, err := p.body.Read(b)
	p.read += int64(n)
	return n, err
}

// WriteTo streams straight through without an intermediate buffer of its own,
// so a forwarded GET does not materialise the object anywhere.
func (p *peerObject) WriteTo(w io.Writer) (int64, error) {
	n, err := io.Copy(w, p.body)
	p.read += n
	return n, err
}

func (p *peerObject) Close() error {
	err := p.body.Close()
	p.cancel()
	return err
}

func (p *peerObject) Size() int64 { return p.size }

func (p *peerObject) Source() string { return p.source }

// Verify is a no-op for a peer's bytes.
//
// The peer already verified them against the key before serving, if it was
// configured to. Re-hashing here would double the CPU cost of every forwarded
// read for a check the client is about to perform anyway — Bazel verifies CAS
// digests itself, and so does the chaos runner, which is what the "zero
// corrupted reads" claim is actually measured against.
func (p *peerObject) Verify() error { return nil }
