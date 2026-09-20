// Package buildinfo reports the version stamped into the binary.
//
// Every benchmark result and chaos report records this value. A performance
// number that cannot be traced back to a commit is not a measurement, it is an
// anecdote, so the value is required to be present in result files.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version is overridden at link time with -ldflags "-X .../buildinfo.Version=...".
var Version = "dev"

// Commit is overridden at link time; when it is not, it is read from the Go
// build info that the toolchain embeds for VCS-aware builds.
var Commit = ""

// String returns "version (commit)" with whatever is known.
func String() string {
	v := Version
	c := Commit
	if c == "" {
		c = vcsRevision()
	}
	if c == "" {
		return v
	}
	if len(c) > 12 {
		c = c[:12]
	}
	return v + " (" + c + ")"
}

// Revision returns the git commit the binary was built from, or "" if unknown.
func Revision() string {
	if Commit != "" {
		return Commit
	}
	return vcsRevision()
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value
		}
	}
	if rev == "" {
		return ""
	}
	if strings.EqualFold(dirty, "true") {
		return rev + "-dirty"
	}
	return rev
}

// GoVersion returns the Go toolchain version the binary was built with.
func GoVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return ""
}
