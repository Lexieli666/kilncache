package protocol

import "testing"

func TestParseHopKnownValues(t *testing.T) {
	cases := map[string]Hop{
		"":            HopClient,
		"coordinator": HopCoordinator,
		"replica":     HopReplica,
		"read":        HopRead,
	}
	for in, want := range cases {
		if got := ParseHop(in); got != want {
			t.Errorf("ParseHop(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseHopUnknownIsTerminal is the falsifier for the forwarding bound. A
// node running a newer version might invent a hop kind; an older node must
// refuse to forward rather than forward blindly, or a rolling upgrade could
// open a loop.
func TestParseHopUnknownIsTerminal(t *testing.T) {
	for _, in := range []string{"gossip", "COORDINATOR", "replica ", "42", "read-only"} {
		got := ParseHop(in)
		if !got.Terminal() {
			t.Errorf("ParseHop(%q) = %q, which is not terminal; an unknown hop must not be forwarded", in, got)
		}
	}
}

// TestHopStateMachineHasNoCycle encodes the argument in the Hop doc comment as
// a test: the only non-terminal states are client and coordinator, and a
// coordinator may only produce terminal hops. That is what bounds a forwarding
// chain to three nodes no matter how badly two nodes disagree about placement.
func TestHopStateMachineHasNoCycle(t *testing.T) {
	if HopClient.Terminal() {
		t.Error("HopClient is terminal; a client request could never be forwarded")
	}
	if HopCoordinator.Terminal() {
		t.Error("HopCoordinator is terminal; a coordinator could never replicate")
	}
	if !HopReplica.Terminal() {
		t.Error("HopReplica is not terminal; a replica write could forward and close a loop")
	}
	if !HopRead.Terminal() {
		t.Error("HopRead is not terminal; a forwarded read could forward and close a loop")
	}
}

func TestForwarded(t *testing.T) {
	if HopClient.Forwarded() {
		t.Error("HopClient reports as forwarded")
	}
	for _, h := range []Hop{HopCoordinator, HopReplica, HopRead} {
		if !h.Forwarded() {
			t.Errorf("%q does not report as forwarded", h)
		}
	}
}

func TestHeadersAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, h := range []string{
		HeaderNode, HeaderForwardedBy, HeaderHop, HeaderSource,
		HeaderHolders, HeaderCopies, HeaderCopiesWanted, HeaderAlreadyStored,
	} {
		if h == "" {
			t.Error("a header name is empty")
		}
		if seen[h] {
			t.Errorf("duplicate header name %q", h)
		}
		seen[h] = true
	}
}
