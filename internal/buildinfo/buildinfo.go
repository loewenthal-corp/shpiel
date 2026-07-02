// Package buildinfo carries version metadata stamped at build time via ldflags.
package buildinfo

import "runtime/debug"

var (
	// Version is the semantic version. The default tracks the latest
	// release via release-please; builds may override it with -ldflags.
	Version = "0.1.0" // x-release-please-version
	// Commit is the short git commit hash, set via -ldflags.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp, set via -ldflags.
	BuildTime = "unknown"
)

// String returns a single-line human-readable version string.
func String() string {
	return Version + " (commit " + Commit + ", built " + BuildTime + ", " + goVersion() + ")"
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.GoVersion
	}
	return "unknown go version"
}
