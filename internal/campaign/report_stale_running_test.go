package campaign

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/manifest"
)

// stateManifest returns a manifest with the positive durations the loader
// guarantees in practice; State round-trips through the same JSON encoding a
// real campaign directory uses, so a zero-value manifest fails to marshal.
func stateManifest() manifest.Manifest {
	return manifest.Manifest{Campaign: manifest.CampaignLimits{
		MaxDuration:           manifest.Duration(time.Minute),
		DiscoveryStallTimeout: manifest.Duration(time.Minute),
		MinimumCommandTimeout: manifest.Duration(time.Second),
	}}
}

// TestLoadReportReportsStaleRunningAsInterrupted pins the fix for a SIGKILLed
// campaign staying "running" forever: bbolt's exclusive lock is only free
// when no process holds it, so if LoadReport can open the store at all, no
// process is renewing "running" -- the campaign died without recording a
// stop. That must render as interrupted, in both the Markdown report and
// `gotorque report --json`, without writing anything back (LoadReport is
// read-only).
func TestLoadReportReportsStaleRunningAsInterrupted(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	require.NoError(t, store.Save(State{ID: "killed-campaign", Status: StatusRunning, Manifest: stateManifest()}))
	require.NoError(t, store.Close())

	loaded, err := LoadReport(dir)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, loaded.Status)
	require.NotEmpty(t, loaded.StopReason)

	// Read-only: bbolt's own record must still say "running".
	reopened, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	onDisk, err := reopened.Load()
	require.NoError(t, err)
	require.Equal(t, StatusRunning, onDisk.Status, "LoadReport must not persist the correction")

	markdown := RenderMarkdown(loaded)
	require.Contains(t, markdown, string(StatusInterrupted))
}

// TestLoadReportKeepsRunningWhenLockIsHeld makes sure the stale-"running"
// correction never fires on the snapshot fallback path, where the campaign
// really is live and the lock is what proves it.
func TestLoadReportKeepsRunningWhenLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	holder, err := OpenStore(filepath.Join(dir, DatabaseName))
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()

	require.NoError(t, WriteReports(dir, State{ID: "live-campaign", Status: StatusRunning, Manifest: stateManifest()}))

	loaded, err := LoadReport(dir)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, loaded.Status, "a live campaign's snapshot must not be reported as interrupted")
}
