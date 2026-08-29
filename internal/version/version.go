// Package version exposes build metadata embedded at link time.
//
// Values are injected via -ldflags -X by the Makefile; the defaults here keep
// `go run ./cmd/boop` usable without a build wrapper.
package version

import (
	"fmt"
	"runtime"
	"strings"
)

var (
	// Version is the semantic version of this build.
	Version = "0.1.0-dev"
	// Commit is the Git commit this build came from.
	Commit = "unknown"
	// Date is the build date, empty when reproducibility is preferred.
	Date = ""
	// Dirty is "true" when the working tree had uncommitted changes.
	Dirty = "false"
)

// Info is a structured snapshot of the build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date,omitempty"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build metadata for this binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		Dirty:     Dirty == "true",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders the short human-readable form used by `boop version`.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "boop v%s", i.Version)
	if i.Dirty {
		b.WriteString(" (dirty)")
	}
	fmt.Fprintf(&b, "\ncommit: %s", i.Commit)
	if i.Date != "" {
		fmt.Fprintf(&b, "\nbuilt:  %s", i.Date)
	}
	fmt.Fprintf(&b, "\ngo:     %s\nos/arch: %s", i.GoVersion, i.Platform)
	return b.String()
}
