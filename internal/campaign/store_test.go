package campaign

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/gotorque/internal/manifest"
	"github.com/stretchr/testify/require"
	bolterrors "go.etcd.io/bbolt/errors"
)

// TestOpenStoreWrapsLockTimeout pins the fix for a lock conflict surfacing as
// bbolt's bare "timeout": OpenStore uses a one-second lock timeout, and a
// second process (or a resume against a still-running one) hit that raw
// sentinel with nothing to explain it.
func TestOpenStoreWrapsLockTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DatabaseName)
	holder, err := OpenStore(path)
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()

	_, err = OpenStore(path)
	require.Error(t, err)
	require.ErrorIs(t, err, bolterrors.ErrTimeout, "the sentinel must still be reachable through errors.Is")
	require.Contains(t, err.Error(), "another gotorque process holds the campaign database", "the bare bbolt timeout must be explained")
}

// TestOpenStorePropagatesOtherErrors makes sure the timeout-specific wrapping
// does not swallow unrelated failures, such as a path that cannot be a
// database at all.
func TestOpenStorePropagatesOtherErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DatabaseName)
	require.NoError(t, os.WriteFile(path, []byte("not a bolt database"), 0o600))

	_, err := OpenStore(path)
	require.Error(t, err)
	require.NotErrorIs(t, err, bolterrors.ErrTimeout)
}

// TestLoadReportFromDatabaseIsVersioned pins the fix for `gotorque report DIR`
// labelling a campaign the current engine just wrote as "unversioned": the
// command reads the database, and only report.json carried the stamp.
func TestLoadReportFromDatabaseIsVersioned(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	// Load decodes through the manifest's duration type, which rejects zero.
	require.NoError(t, store.Save(State{ID: "c1", Manifest: manifest.Manifest{Campaign: manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}}}))
	require.NoError(t, store.Close())

	state, err := LoadReport(dir)
	require.NoError(t, err)
	require.Equal(t, ReportSchemaVersion, state.SchemaVersion)
}
