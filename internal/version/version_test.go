package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildInfo is a stand-in for what the toolchain stamps into a binary. Passing
// it to buildFrom is how these tests exercise a checkout build on a machine
// that also releases: debug.ReadBuildInfo describes the test binary itself.
func buildInfo(settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{Settings: settings}
}

func TestBuildFromPrefersTheReleaseCommit(t *testing.T) {
	build := buildFrom("0.2.0", "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90", buildInfo(
		debug.BuildSetting{Key: "vcs.revision", Value: "1111111111111111111111111111111111111111"},
	))
	require.Equal(t, Build{Version: "0.2.0", Commit: "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90"}, build,
		"the revision the release names wins over a stamp from wherever it was built")
}

func TestBuildFromFallsBackToTheVCSStamp(t *testing.T) {
	// A plain `go build ./cmd/gotorque` leaves the ldflags at their defaults and
	// stamps the checkout instead, which is the case the campaign needs: a
	// verdict from an unreleased build must still name a revision.
	build := buildFrom("dev", Unknown, buildInfo(
		debug.BuildSetting{Key: "buildMode", Value: "exe"},
		debug.BuildSetting{Key: "vcs.revision", Value: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"},
	))
	require.Equal(t, Build{Version: "dev", Commit: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed"}, build)
}

func TestBuildFromMarksAModifiedCheckout(t *testing.T) {
	build := buildFrom("dev", "", buildInfo(
		debug.BuildSetting{Key: "vcs.revision", Value: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	))
	require.True(t, build.Modified)
	require.Equal(t, "dev 9b1fea7-dirty", build.Describe())
}

func TestBuildFromCarriesTheStampLeniencyIntoAReleaseBuild(t *testing.T) {
	// VCS stamping is on for a release build too, so a release built from a
	// dirty tree says dirty rather than presenting itself as the tag alone.
	build := buildFrom("0.2.0", "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90", buildInfo(
		debug.BuildSetting{Key: "vcs.revision", Value: "d72af7f4a1b2c3d4e5f60718293a4b5c6d7e8f90"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	))
	require.Equal(t, "0.2.0 d72af7f-dirty", build.Describe())
}

func TestBuildFromWithoutAnyRevisionSaysUnknown(t *testing.T) {
	for _, info := range []*debug.BuildInfo{nil, buildInfo()} {
		require.Equal(t, Build{Version: "dev", Commit: Unknown}, buildFrom("dev", Unknown, info))
	}
	require.Equal(t, "unknown", buildFrom("dev", Unknown, nil).Describe())
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		want  string
		build Build
	}{
		{"unknown", Build{}},
		{"unknown", Build{Version: "dev"}},
		{"unknown", Build{Commit: Unknown, Modified: true}},
		{"dev 9b1fea7", Build{Version: "dev", Commit: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed"}},
		{"dev 9b1fea7", Build{Version: "dev", Commit: "9b1fea7"}},
		{"0.2.0 d72af7f", Build{Version: "0.2.0", Commit: "d72af7f"}},
		{"0.2.0 d72af7f-dirty", Build{Version: "0.2.0", Commit: "d72af7f4a1b2", Modified: true}},
		{"9b1fea7-dirty", Build{Commit: "9b1fea70c6a1d1b9e5f4a3c2b1a0987654321fed", Modified: true}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, c.build.Describe(), "%+v", c.build)
	}
}

func TestCurrentBuildDescribesTheRunningBinary(t *testing.T) {
	// The test binary carries the checkout's stamp, so this build is either a
	// real revision or an explicit unknown -- never a blank.
	require.NotEmpty(t, CurrentBuild().Describe())
	require.Equal(t, Build{Version: Version, Commit: Commit, Modified: false}, buildFrom(Version, Commit, nil),
		"with nothing stamped, CurrentBuild falls back to the ldflags values")
}
