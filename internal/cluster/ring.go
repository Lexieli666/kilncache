// Package cluster implements static membership and rendezvous (highest random
// weight) placement: the pure function that maps an object key onto an ordered
// list of nodes, identically on every node, with no coordination.
package cluster

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Lexieli666/kilncache/internal/config"
)

// Member is one node in the static cluster.
type Member struct {
	Name string
	URL  string

	// seed is a 64-bit value derived from the name. It is what makes placement
	// depend on identity rather than on position in the list, so reordering the
	// --peers flag moves no keys at all.
	seed uint64
}

// Ring answers "who should hold this key" for a fixed membership.
//
// Rendezvous hashing rather than a consistent-hash ring: see
// docs/adr/0004-rendezvous-hashing.md. The short version is that rendezvous
// needs no virtual nodes to be balanced, gives a full ordered preference list
// for free (which is exactly what replica fallback needs), and is a dozen lines
// with nothing to tune.
//
// A Ring is immutable after construction and safe for concurrent use.
type Ring struct {
	members []Member
	self    string
}

// ErrEmptyMembership is returned when a ring is built with no members.
var ErrEmptyMembership = errors.New("cluster: membership must not be empty")

// New builds a ring from the static peer list.
//
// Members are sorted by name so that two nodes given the same peers in a
// different order build the identical ring. Without that, placement would
// depend on the order of a command-line flag, and a cluster whose operators
// wrote --peers differently would silently disagree about where objects live.
func New(peers []config.Peer, self string) (*Ring, error) {
	if len(peers) == 0 {
		return nil, ErrEmptyMembership
	}
	members := make([]Member, 0, len(peers))
	seen := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return nil, errors.New("cluster: member name must not be empty")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("cluster: duplicate member %q", name)
		}
		seen[name] = struct{}{}
		members = append(members, Member{
			Name: name,
			URL:  strings.TrimRight(p.URL, "/"),
			seed: memberSeed(name),
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

	if self != "" {
		if _, ok := seen[self]; !ok {
			return nil, fmt.Errorf("cluster: self %q is not a member", self)
		}
	}
	return &Ring{members: members, self: self}, nil
}

// Size returns the number of members.
func (r *Ring) Size() int { return len(r.members) }

// Self returns this node's name.
func (r *Ring) Self() string { return r.self }

// Members returns a copy of the membership, sorted by name.
func (r *Ring) Members() []Member {
	out := make([]Member, len(r.members))
	copy(out, r.members)
	return out
}

// Lookup returns a member by name.
func (r *Ring) Lookup(name string) (Member, bool) {
	for _, m := range r.members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// memberSeed derives a node's 64-bit identity from its name.
//
// SHA-256 is used here and nowhere on the hot path: it runs once per member at
// construction. What it buys is that adjacent names ("node-a", "node-b") get
// completely unrelated seeds, which is what keeps placement balanced for the
// short, similar names people actually give nodes.
func memberSeed(name string) uint64 {
	sum := sha256.Sum256([]byte("kilncache/member/" + name))
	return binary.BigEndian.Uint64(sum[:8])
}

// keySeed extracts 64 bits of entropy from an object key.
//
// Keys are already SHA-256 digests in lowercase hex, so their bits are uniform
// and the first eight bytes can be used directly — hashing them again would be
// pure cost. Anything that is not a valid-looking key falls back to hashing,
// so that placement is still defined for keys the store would reject; a
// placement function that panicked on bad input would turn a 404 into a crash.
func keySeed(key string) uint64 {
	if len(key) >= 16 && isHex(key[:16]) {
		var v uint64
		for i := 0; i < 16; i++ {
			v = v<<4 | uint64(hexVal(key[i]))
		}
		return v
	}
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func hexVal(c byte) byte {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

// mix is the SplitMix64 finalizer: a bijection on 64-bit integers with strong
// avalanche. Combining the key seed and the member seed with XOR and then
// mixing decorrelates them, which is what makes the per-member weights behave
// like independent uniform draws — the property rendezvous hashing's balance
// argument rests on.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func weight(keySeed, memberSeed uint64) uint64 {
	return mix(keySeed ^ memberSeed)
}

// Owners returns every member ordered by descending weight for key: the
// preference list. Element 0 is the primary, element 1 the replica, and the
// rest are the fallbacks a read tries when both are unreachable.
//
// Ties are broken by name, deterministically. A 64-bit collision is
// astronomically unlikely, but "astronomically unlikely" is not "impossible",
// and a tie resolved by map order would make two nodes disagree about placement
// for exactly one key — the hardest kind of bug to ever find.
func (r *Ring) Owners(key string) []Member {
	ks := keySeed(key)
	type scored struct {
		m Member
		w uint64
	}
	scoredMembers := make([]scored, len(r.members))
	for i, m := range r.members {
		scoredMembers[i] = scored{m: m, w: weight(ks, m.seed)}
	}
	sort.Slice(scoredMembers, func(i, j int) bool {
		if scoredMembers[i].w != scoredMembers[j].w {
			return scoredMembers[i].w > scoredMembers[j].w
		}
		return scoredMembers[i].m.Name < scoredMembers[j].m.Name
	})
	out := make([]Member, len(scoredMembers))
	for i := range scoredMembers {
		out[i] = scoredMembers[i].m
	}
	return out
}

// Holders returns the n members that should hold a copy of key, in preference
// order. n is clamped to the cluster size.
func (r *Ring) Holders(key string, n int) []Member {
	if n < 1 {
		n = 1
	}
	owners := r.Owners(key)
	if n > len(owners) {
		n = len(owners)
	}
	return owners[:n]
}

// HoldsKey reports whether member name is one of the n holders of key.
func (r *Ring) HoldsKey(name, key string, n int) bool {
	for _, m := range r.Holders(key, n) {
		if m.Name == name {
			return true
		}
	}
	return false
}

// SelfHolds reports whether this node is one of the n holders of key.
func (r *Ring) SelfHolds(key string, n int) bool {
	return r.HoldsKey(r.self, key, n)
}

// Primary returns the first-choice holder of key.
func (r *Ring) Primary(key string) Member {
	return r.Owners(key)[0]
}
