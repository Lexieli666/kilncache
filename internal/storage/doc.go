// Package storage implements the on-disk content-addressed object store: a
// sharded directory tree, streaming writes through a temp file, digest
// verification, atomic publication, and the SQLite metadata index that drives
// quota enforcement and eviction.
package storage
