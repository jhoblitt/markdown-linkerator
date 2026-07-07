// Package version exposes build metadata. The exported vars are overridden at
// build time with -ldflags -X (see .goreleaser.yaml); the runtime/debug
// fallback keeps `go install`-ed and `go run` builds informative.
package version

import "runtime/debug"

// These are set via -ldflags at release time.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Info returns the resolved version, commit, and date, falling back to the
// Go module build info embedded by the toolchain when ldflags were not set.
func Info() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return orDev(version), orUnknown(commit), orUnknown(date)
	}
	if version == "" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "" {
				date = s.Value
			}
		}
	}
	return orDev(version), orUnknown(commit), orUnknown(date)
}

func orDev(s string) string {
	if s == "" || s == "(devel)" {
		return "dev"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
