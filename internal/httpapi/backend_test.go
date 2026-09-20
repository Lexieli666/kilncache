package httpapi

import (
	"context"
	"io"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// storeBackend is a protocol.Backend over a bare local store: no placement, no
// replication, no peers.
//
// The handler tests use it rather than a real Coordinator so that this
// package's tests stay about HTTP -- status codes, headers, path parsing,
// streaming -- and do not quietly become cluster tests. Replication behaviour
// is tested where it lives, in internal/cluster and the integration suite.
type storeBackend struct {
	store *storage.Store
}

func (b *storeBackend) Put(ctx context.Context, ns storage.Namespace, key string, body io.Reader, size int64, _ protocol.Hop) (protocol.PutOutcome, error) {
	res, err := b.store.Put(ctx, ns, key, body, size)
	if err != nil {
		return protocol.PutOutcome{}, err
	}
	return protocol.PutOutcome{
		Size:          res.Size,
		AlreadyStored: res.AlreadyStored,
		Copies:        1,
		Wanted:        1,
		Holders:       []string{"node-a"},
		Durable:       res.Durable,
	}, nil
}

func (b *storeBackend) Open(_ context.Context, ns storage.Namespace, key string, _ protocol.Hop) (protocol.ObjectReader, error) {
	obj, err := b.store.Get(ns, key)
	if err != nil {
		return nil, err
	}
	return &localReader{Object: obj}, nil
}

func (b *storeBackend) Stat(_ context.Context, ns storage.Namespace, key string, _ protocol.Hop) (protocol.ObjectInfo, error) {
	st, err := b.store.Stat(ns, key)
	if err != nil {
		return protocol.ObjectInfo{}, err
	}
	return protocol.ObjectInfo{Size: st.Size, Source: "local"}, nil
}

type localReader struct {
	*storage.Object
}

func (l *localReader) Size() int64    { return l.Object.Stat.Size }
func (l *localReader) Source() string { return "local" }

// failingBackend lets a test drive the error paths the handler maps to status
// codes without having to arrange the underlying failure for real.
type failingBackend struct {
	putErr  error
	openErr error
	statErr error
	outcome protocol.PutOutcome
}

func (b *failingBackend) Put(_ context.Context, _ storage.Namespace, _ string, body io.Reader, _ int64, _ protocol.Hop) (protocol.PutOutcome, error) {
	// Drain so the handler's own behaviour, not an unread body, decides what
	// happens to the connection.
	_, _ = io.Copy(io.Discard, body)
	return b.outcome, b.putErr
}

func (b *failingBackend) Open(context.Context, storage.Namespace, string, protocol.Hop) (protocol.ObjectReader, error) {
	return nil, b.openErr
}

func (b *failingBackend) Stat(context.Context, storage.Namespace, string, protocol.Hop) (protocol.ObjectInfo, error) {
	return protocol.ObjectInfo{}, b.statErr
}

var (
	_ protocol.Backend      = (*storeBackend)(nil)
	_ protocol.Backend      = (*failingBackend)(nil)
	_ protocol.ObjectReader = (*localReader)(nil)
)
