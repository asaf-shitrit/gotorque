package campaign

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireFreeSpace(t *testing.T) {
	saved := diskFree
	t.Cleanup(func() { diskFree = saved })

	diskFree = func(string) (uint64, bool) { return 260 << 20, true }
	err := requireFreeSpace("/campaign")
	require.ErrorContains(t, err, "only 260 MiB free on the filesystem holding /campaign")
	require.ErrorContains(t, err, "resume the campaign")

	diskFree = func(string) (uint64, bool) { return 5 << 30, true }
	require.NoError(t, requireFreeSpace("/campaign", "/tmp"))

	diskFree = func(string) (uint64, bool) { return 0, false }
	require.NoError(t, requireFreeSpace("/campaign"), "an unreadable filesystem is not evidence of a full one")

	free, ok := freeBytes(t.TempDir())
	if ok {
		require.Positive(t, free)
	}
	diskFree = func(string) (uint64, bool) { return 5 << 30, true }
	require.NoError(t, (&evaluator{dir: t.TempDir()}).requireFreeSpace())
}
