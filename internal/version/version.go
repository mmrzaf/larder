// Package version exposes the build information stamped into the binary.
package version

import "runtime"

var (
	// Version is the release version, replaced at link time.
	Version = "dev"
	// Commit is the source revision, replaced at link time.
	Commit = "none"
	// Date is the build timestamp, replaced at link time.
	Date = "unknown"
)

// Info is the complete build information reported by the CLI.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information for this binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
	}
}
