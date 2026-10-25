package version

import (
	"runtime/debug"
	"strings"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Unknown is the revision reported when neither the release build nor the Go
// VCS stamp names one: `go run`, or a build with VCS stamping turned off.
const Unknown = "unknown"

// shortCommitLen is how much of a revision a report shows, as `git log
// --oneline` does.
const shortCommitLen = 7

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}

// Build identifies the gotorque binary itself, as opposed to the target a
// campaign measures. A campaign records it because its verdicts depend on the
// build: a report otherwise names only the target's revision, and the same
// campaign directory can be continued by a later binary.
type Build struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
	// Modified marks a binary built from a checkout with uncommitted changes,
	// from the Go VCS stamp. A bullet measured on a modified engine is not a
	// claim about any commit, so the record says so.
	Modified bool `json:"modified,omitempty"`
}

// CurrentBuild identifies the running binary. A release build stamps the
// version and commit through ldflags (.goreleaser.yaml); any other `go build`
// from a checkout leaves Commit at Unknown, and the VCS stamp the toolchain
// embeds (debug.ReadBuildInfo) supplies the revision and whether that checkout
// had uncommitted changes instead.
func CurrentBuild() Build {
	info, _ := debug.ReadBuildInfo()
	return buildFrom(Version, Commit, info)
}

// buildFrom assembles a Build from the release build's ldflags values and what
// the toolchain stamped into the binary. A commit the release build set wins,
// because that is the revision the release names; the stamp still says whether
// the checkout it was built in had uncommitted changes.
func buildFrom(version, commit string, info *debug.BuildInfo) Build {
	revision, modified := vcsSettings(info)
	build := Build{Version: version, Commit: commit, Modified: modified}
	if build.Commit == "" || build.Commit == Unknown {
		build.Commit = revision
	}
	if build.Commit == "" {
		build.Commit = Unknown
	}
	return build
}

// vcsSettings reads the revision and modified flag out of a build's VCS
// settings. They are absent from a binary built without a checkout to stamp,
// and from one built with -buildvcs=false.
func vcsSettings(info *debug.BuildInfo) (revision string, modified bool) {
	if info == nil {
		return "", false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

// Describe is the build as one short line for a report or a scorecard cell:
// the version, the revision shortened the way `git log --oneline` shortens it,
// and a "-dirty" mark for a checkout with uncommitted changes. A build that
// recorded nothing describes itself as Unknown, so a campaign written before
// this was recorded reads as the honest gap it is rather than as a blank.
func (b Build) Describe() string {
	commit := b.Commit
	if commit == "" || commit == Unknown {
		return Unknown
	}
	if len(commit) > shortCommitLen {
		commit = commit[:shortCommitLen]
	}
	if b.Modified {
		commit += "-dirty"
	}
	if b.Version == "" {
		return commit
	}
	return strings.TrimSpace(b.Version + " " + commit)
}
