package campaign

import (
	"fmt"
	"os"
)

// minFreeBytes is the space a candidate needs before it is built and tested:
// a worktree, a release binary and the test binaries of its packages. Below
// it the build or the test suite fails for want of room, and that failure
// used to be recorded against the patch: on overnight-miller-4 the disk had
// 260 MB left, and two candidates were rejected for "test build or setup
// failed" in packages their patch never touched.
const minFreeBytes = 1 << 30

// diskFree is freeBytes, replaceable in tests.
var diskFree = freeBytes

// requireFreeSpace refuses to evaluate a candidate when the campaign
// directory or the temporary directory the toolchain builds in is short of
// space. The error stops the campaign instead of rejecting the candidate: a
// full disk says nothing about a patch, and a resumed campaign picks up where
// this one stopped once space is freed.
func requireFreeSpace(dirs ...string) error {
	for _, dir := range dirs {
		free, ok := diskFree(dir)
		if ok && free < minFreeBytes {
			return fmt.Errorf("only %d MiB free on the filesystem holding %s; a candidate needs at least %d MiB to build and test, and a full disk would reject it for reasons unrelated to its patch; free space and resume the campaign", free>>20, dir, minFreeBytes>>20)
		}
	}
	return nil
}

func (ev *evaluator) requireFreeSpace() error { return requireFreeSpace(ev.dir, os.TempDir()) }
