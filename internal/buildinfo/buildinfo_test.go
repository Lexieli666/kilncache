package buildinfo

import (
	"strings"
	"testing"
)

func TestStringWithExplicitValues(t *testing.T) {
	origV, origC := Version, Commit
	t.Cleanup(func() { Version, Commit = origV, origC })

	Version, Commit = "v1.0.0", "0123456789abcdef0123456789abcdef01234567"
	got := String()
	if got != "v1.0.0 (0123456789ab)" {
		t.Errorf("String() = %q, want the commit truncated to 12 characters", got)
	}
	if Revision() != Commit {
		t.Errorf("Revision() = %q, want the full commit", Revision())
	}
}

func TestStringWithShortCommit(t *testing.T) {
	origV, origC := Version, Commit
	t.Cleanup(func() { Version, Commit = origV, origC })

	Version, Commit = "dev", "abc123"
	if got := String(); got != "dev (abc123)" {
		t.Errorf("String() = %q", got)
	}
}

// TestStringWithoutCommit covers the path where no commit was stamped and the
// binary was not built from a VCS-aware tree either. It must degrade to the
// version alone rather than printing an empty parenthesis, because this string
// goes into every benchmark result file.
func TestStringWithoutCommit(t *testing.T) {
	origV, origC := Version, Commit
	t.Cleanup(func() { Version, Commit = origV, origC })

	Version, Commit = "dev", ""
	got := String()
	if strings.Contains(got, "()") {
		t.Errorf("String() = %q; an unknown commit must not print empty parentheses", got)
	}
	if !strings.HasPrefix(got, "dev") {
		t.Errorf("String() = %q, want it to start with the version", got)
	}
}

func TestGoVersionIsReported(t *testing.T) {
	// Built by `go test`, so build info is always available here.
	if v := GoVersion(); !strings.HasPrefix(v, "go1.") {
		t.Errorf("GoVersion() = %q, want a go1.x string", v)
	}
}

func TestRevisionFallsBackToBuildInfo(t *testing.T) {
	origC := Commit
	t.Cleanup(func() { Commit = origC })
	Commit = ""
	// Under `go test` the VCS stamp is usually absent, so the only assertion
	// that holds everywhere is that this does not panic and returns a string.
	// A non-empty answer is checked when one is available.
	rev := Revision()
	if rev != "" && len(rev) < 7 {
		t.Errorf("Revision() = %q, want either empty or a plausible revision", rev)
	}
}
