package storage

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means the object is absent. It is the normal case for a cold
	// build, not an error condition, and is logged at debug level.
	ErrNotFound = errors.New("object not found")

	// ErrDigestMismatch means the bytes the client sent do not hash to the key
	// it sent them under. The upload is discarded without ever being published.
	ErrDigestMismatch = errors.New("digest mismatch")

	// ErrTooLarge means the object exceeds the configured per-object limit.
	ErrTooLarge = errors.New("object too large")

	// ErrClosed means the store has been closed.
	ErrClosed = errors.New("store is closed")

	// ErrShortWrite means the client declared a Content-Length it did not
	// deliver. This is the interrupted-upload case, and it must never publish.
	ErrShortWrite = errors.New("body shorter than declared content length")
)

// DigestMismatchError carries both digests so the log line says what actually
// happened rather than only that something did. Debugging a cache that rejects
// a client's uploads is impossible without both values.
type DigestMismatchError struct {
	Want string
	Got  string
	Size int64
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("%v: key %s but content hashes to %s (%d bytes)",
		ErrDigestMismatch, e.Want, e.Got, e.Size)
}

func (e *DigestMismatchError) Unwrap() error { return ErrDigestMismatch }

// TooLargeError carries the limit so the operator learns which knob to turn.
type TooLargeError struct {
	Size  int64
	Limit int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%v: %d bytes exceeds the per-object limit of %d", ErrTooLarge, e.Size, e.Limit)
}

func (e *TooLargeError) Unwrap() error { return ErrTooLarge }
