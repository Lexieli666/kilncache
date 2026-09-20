// Package storage implements the on-disk content-addressed object store: a
// sharded directory tree, streaming writes through a temp file, digest
// verification, atomic publication, and (from Phase 3) the SQLite metadata
// index that drives quota enforcement and eviction.
package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Namespace separates the two halves of Bazel's HTTP cache protocol.
//
// They are stored in separate trees because they have different rules, not
// merely different names: a CAS object's key is the SHA-256 of its bytes and is
// therefore immutable and self-verifying, while an action-cache entry's key is
// a hash of the *action* and its value may legitimately be overwritten. Mixing
// them in one tree would mean one code path with two contradictory invariants.
type Namespace string

const (
	// NamespaceCAS holds content-addressed objects: key == SHA-256(content).
	NamespaceCAS Namespace = "cas"
	// NamespaceAC holds action cache entries: key is the action digest, and the
	// stored bytes are an ActionResult that the server does not interpret.
	NamespaceAC Namespace = "ac"
)

// Valid reports whether ns is one of the two known namespaces.
func (ns Namespace) Valid() bool {
	return ns == NamespaceCAS || ns == NamespaceAC
}

// ContentAddressed reports whether the key determines the bytes. Only CAS keys
// do; this is the single predicate the write path branches on.
func (ns Namespace) ContentAddressed() bool { return ns == NamespaceCAS }

func (ns Namespace) String() string { return string(ns) }

// ParseNamespace maps a URL path segment onto a Namespace.
func ParseNamespace(s string) (Namespace, error) {
	switch Namespace(strings.ToLower(s)) {
	case NamespaceCAS:
		return NamespaceCAS, nil
	case NamespaceAC:
		return NamespaceAC, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrBadNamespace, s)
	}
}

// HashLen is the length of a lowercase hex SHA-256 digest.
const HashLen = 64

// ErrBadKey is returned for anything that is not a lowercase hex SHA-256.
var ErrBadKey = errors.New("invalid key")

// ErrBadNamespace is returned for a path that is neither /cas/ nor /ac/.
var ErrBadNamespace = errors.New("invalid namespace")

// ValidateKey checks that s is exactly 64 lowercase hex characters.
//
// Uppercase is rejected rather than normalised. If both "AB..." and "ab..."
// were accepted and normalised, two keys that Bazel considers distinct would
// collide in the store; if they were accepted and *not* normalised, the same
// object would be stored twice under names that differ only in case, on a
// filesystem that may or may not be case-sensitive. Rejecting is the only
// option that behaves the same on ext4 and on APFS.
//
// The check is also the first line of defence against path traversal: a key
// that cannot contain '.' or '/' cannot escape its shard directory. The test
// for that is explicit rather than implied.
func ValidateKey(s string) error {
	if len(s) != HashLen {
		return fmt.Errorf("%w: expected %d hex characters, got %d", ErrBadKey, HashLen, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return fmt.Errorf("%w: byte %d is %q, expected lowercase hex", ErrBadKey, i, string(c))
	}
	return nil
}

// ShardDepth is the number of directory levels between a namespace root and an
// object. Two levels of one hex byte each gives 256 top-level and 65,536 leaf
// directories, created lazily.
//
// The number matters: a flat directory with a million entries makes every
// lookup a linear scan on some filesystems and makes `ls` in an incident
// unusable on all of them. Two levels keeps the expected leaf occupancy of a
// million-object cache at about fifteen entries. One level would leave four
// thousand per directory; three would create sixteen million directories to
// hold a million files. See docs/adr/0003-durability.md.
const ShardDepth = 2

// shardWidth is the number of hex characters consumed per directory level: one
// byte.
const shardWidth = 2

// ObjectPath returns the path of an object relative to the store root, and the
// directory that contains it. The caller needs both: the file to open, and the
// directory to fsync after a rename.
func (s *Store) ObjectPath(ns Namespace, key string) (dir, path string) {
	dir = s.shardDir(ns, key)
	return dir, filepath.Join(dir, key)
}

func (s *Store) shardDir(ns Namespace, key string) string {
	parts := make([]string, 0, ShardDepth+2)
	parts = append(parts, s.root, string(ns))
	for i := 0; i < ShardDepth; i++ {
		parts = append(parts, key[i*shardWidth:(i+1)*shardWidth])
	}
	return filepath.Join(parts...)
}
