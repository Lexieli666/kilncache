// Package protocol defines the contract between KilnCache's HTTP front door and
// whatever is behind it: the headers nodes use to talk to each other, the hop
// roles that make forwarding loops impossible, and the Backend interface the
// handler calls.
//
// It exists to keep the dependency between internal/httpapi and
// internal/cluster pointing one way. Both need these definitions; if either
// owned them, the other would have to import it, and the natural direction is
// ambiguous enough that the import cycle would show up eventually.
package protocol

import (
	"context"
	"errors"
	"io"

	"github.com/Lexieli666/kilncache/internal/storage"
)

// ErrOverQuota means a node declined a repair write because it is already above
// its high-water mark. The HTTP layer maps it to 507 Insufficient Storage.
//
// It is deliberately not an error condition for the cluster: the sender counts
// it as "declined", not "failed". A node saying "I do not have room for a copy
// of something I already chose to evict" is the system working.
var ErrOverQuota = errors.New("node is over its storage high-water mark")

// ErrInsufficientReplicas means the required number of copies could not be
// written. The HTTP layer maps it to 503.
//
// It lives here rather than in internal/cluster because internal/httpapi has to
// match on it to choose a status code, and importing cluster from httpapi would
// point the dependency the wrong way -- which is the exact problem this package
// exists to solve. cluster.InsufficientReplicasError unwraps to this value.
var ErrInsufficientReplicas = errors.New("could not write the required number of copies")

// Headers used between nodes and towards clients.
const (
	// HeaderNode names the node that produced a response. Integration tests
	// read it to prove that an object served through one node really came from
	// another node's copy.
	HeaderNode = "X-Kilncache-Node"

	// HeaderForwardedBy carries the name of the node that forwarded a request.
	// Its presence, not its value, is what stops loops.
	HeaderForwardedBy = "X-Kilncache-Forwarded-By"

	// HeaderHop carries the role the forwarding node assigned to this request.
	// See Hop.
	HeaderHop = "X-Kilncache-Hop"

	// HeaderSource says where the bytes in a response came from: "local", or
	// the name of the peer this node fetched them from.
	HeaderSource = "X-Kilncache-Source"

	// HeaderHolders lists the nodes that hold a copy after a PUT.
	HeaderHolders = "X-Kilncache-Holders"

	// HeaderCopies reports how many copies exist after a PUT, and
	// HeaderCopiesWanted how many were required.
	HeaderCopies       = "X-Kilncache-Copies"
	HeaderCopiesWanted = "X-Kilncache-Copies-Wanted"

	// HeaderAlreadyStored marks a PUT that was a no-op.
	HeaderAlreadyStored = "X-Kilncache-Already-Stored"
)

// Hop is the role a request has in the forwarding chain.
//
// This is the whole loop-prevention mechanism, and it is a state machine with
// three states and no cycles:
//
//	HopClient      -> may forward as HopCoordinator or HopReplica or HopRead
//	HopCoordinator -> may forward as HopReplica only
//	HopReplica     -> terminal: store locally or fail
//	HopRepair      -> terminal: store locally, or decline if over quota
//	HopRead        -> terminal: serve locally or 404
//
// Because the only outgoing edge from HopCoordinator leads to a terminal state,
// a client request can produce a chain of at most three nodes regardless of how
// badly two nodes disagree about placement. Disagreement is possible: a node
// started with a stale peer list computes different owners, and without a bound
// the two could forward to each other forever. Counting hops would also work;
// naming the role additionally makes the server logs say what each request was
// for.
type Hop string

const (
	// HopClient is a request that arrived from outside the cluster.
	HopClient Hop = ""
	// HopCoordinator is a request forwarded to the node that should own the
	// key, which is then responsible for placing the remaining copies.
	HopCoordinator Hop = "coordinator"
	// HopReplica is a request to store one copy and nothing more.
	HopReplica Hop = "replica"
	// HopRepair is a replica write issued by the background repair worker
	// rather than by a client's PUT.
	//
	// It is terminal like HopReplica, and differs in exactly one way: a node
	// that is already over its high-water mark declines it. That distinction
	// exists because eviction and repair are otherwise capable of fighting each
	// other indefinitely -- a node evicts an object to get under quota, the
	// other holder's auditor sees a missing replica and sends it back, and the
	// quota is never enforced. A client's write is new data someone is waiting
	// for and is always accepted; a repair write is a copy of something this
	// node has already decided it does not have room for.
	HopRepair Hop = "repair"
	// HopRead is a request to serve a local copy and nothing more.
	HopRead Hop = "read"
)

// Terminal reports whether a request with this role may forward further.
func (h Hop) Terminal() bool {
	return h == HopReplica || h == HopRead || h == HopRepair
}

// Forwarded reports whether the request came from another node.
func (h Hop) Forwarded() bool { return h != HopClient }

// ParseHop maps a header value onto a Hop, defaulting to the most restrictive
// interpretation.
//
// An unrecognised value is treated as HopReplica — terminal — rather than as a
// client request. A node running a newer version that invents a hop kind should
// cause the old node to refuse to forward, not to forward blindly.
func ParseHop(s string) Hop {
	switch Hop(s) {
	case HopCoordinator:
		return HopCoordinator
	case HopReplica:
		return HopReplica
	case HopRepair:
		return HopRepair
	case HopRead:
		return HopRead
	case HopClient:
		return HopClient
	default:
		return HopReplica
	}
}

// PutOutcome describes where an object ended up.
type PutOutcome struct {
	Size          int64
	AlreadyStored bool

	// Copies is how many nodes hold the object now; Wanted is how many were
	// required before acknowledging. Copies < Wanted is a failure, and the
	// handler turns it into a 503 rather than a 200 — a PUT that returns
	// success without the second copy makes the replication factor a fiction.
	Copies  int
	Wanted  int
	Holders []string

	// Durable is false when the filesystem could not fsync a directory, so a
	// durability claim made on this node is not one.
	Durable bool
}

// ObjectInfo is the result of a Stat.
type ObjectInfo struct {
	Size   int64
	Source string
}

// ObjectReader streams a stored object. It is either a local file or a peer's
// response body; the handler does not care which, beyond reporting Source.
type ObjectReader interface {
	io.Reader
	io.WriterTo
	io.Closer

	// Size is the object's length in bytes, known before any byte is read so
	// the handler can set Content-Length.
	Size() int64

	// Source names where the bytes came from: "local" or a peer's node name.
	Source() string

	// Verify reports a digest mismatch after the whole object has been read.
	// It returns nil when verification is disabled.
	Verify() error
}

// Backend is what the HTTP handler calls. A single-node deployment and a
// three-node cluster use the same implementation with a different membership,
// so there is no second code path that only runs in production.
type Backend interface {
	Put(ctx context.Context, ns storage.Namespace, key string, body io.Reader, declaredSize int64, hop Hop) (PutOutcome, error)
	Open(ctx context.Context, ns storage.Namespace, key string, hop Hop) (ObjectReader, error)
	Stat(ctx context.Context, ns storage.Namespace, key string, hop Hop) (ObjectInfo, error)
}
