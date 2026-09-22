package campaign

import (
	"os"
	"path/filepath"
	"testing"

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
