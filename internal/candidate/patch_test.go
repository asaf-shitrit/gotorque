package candidate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateUnifiedDiff(t *testing.T) {
	result, err := ValidateUnifiedDiff("diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n", Policy{})
	require.NoError(t, err)
	require.Equal(t, []string{"main.go"}, result.Files)
}

func TestValidateUnifiedDiffRejectsDependenciesAndEscapes(t *testing.T) {
	for _, patch := range []string{
		"--- a/go.mod\n+++ b/go.mod\n@@ -1 +1 @@\n-a\n+b\n",
		"--- a/x\n+++ ../../outside\n@@ -1 +1 @@\n-a\n+b\n",
		"--- a/vendor/x\n+++ b/vendor/x\n@@ -1 +1 @@\n-a\n+b\n",
	} {
		_, err := ValidateUnifiedDiff(patch, Policy{})
		require.Error(t, err)
	}
}

// Ordinary source edits keep passing: new and deleted regular files, GNU diff
// timestamps after the name, and a quoted name git writes for a space.
func TestValidateUnifiedDiffAcceptsOrdinarySourceChanges(t *testing.T) {
	cases := map[string]struct {
		patch string
		files []string
	}{
		"new file": {
			"diff --git a/fast.go b/fast.go\nnew file mode 100644\nindex 0000000..e69de29\n--- /dev/null\n+++ b/fast.go\n@@ -0,0 +1 @@\n+package main\n",
			[]string{"fast.go"},
		},
		"deleted file": {
			"diff --git a/slow.go b/slow.go\ndeleted file mode 100644\nindex e69de29..0000000\n--- a/slow.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package main\n",
			[]string{"slow.go"},
		},
		"timestamps": {
			"--- a/cmd/tool/main.go\t2026-01-01 00:00:00.000000000 +0000\n+++ b/cmd/tool/main.go\t2026-01-02 00:00:00.000000000 +0000\n@@ -1 +1 @@\n-a\n+b\n",
			[]string{"cmd/tool/main.go"},
		},
		"edited in place with its mode": {
			"diff --git a/main.go b/main.go\nindex 1111111..2222222 100644\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-a\n+b\n",
			[]string{"main.go"},
		},
		"quoted name": {
			"--- \"a/my dir/main.go\"\n+++ \"b/my dir/main.go\"\n@@ -1 +1 @@\n-a\n+b\n",
			[]string{"my dir/main.go"},
		},
		"CRLF line endings": {
			"diff --git a/fast.go b/fast.go\r\nnew file mode 100644\r\n--- /dev/null\r\n+++ b/fast.go\r\n@@ -0,0 +1 @@\r\n+package main\r\n",
			[]string{"fast.go"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := ValidateUnifiedDiff(tc.patch, Policy{})
			require.NoError(t, err)
			require.Equal(t, tc.files, result.Files)
		})
	}
}

// Every header git apply or GNU patch can take a file name from is checked,
// and so is every Git header that changes what a path is. Each case is a
// shape that passed the old +++-only check.
func TestValidateUnifiedDiffRejectsEveryRouteToAProtectedPath(t *testing.T) {
	hunk := "@@ -1 +1 @@\n-a\n+b\n"
	cases := map[string]struct {
		patch string
		want  string
	}{
		"deletion of go.mod": {
			"diff --git a/go.mod b/go.mod\ndeleted file mode 100644\n--- a/go.mod\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-module x\n-\n-go 1.26\n",
			`"go.mod" is off-limits`,
		},
		"old name differs from validated new name": {
			"--- a/go.mod\n+++ b/other.go\n" + hunk,
			`"go.mod" is off-limits`,
		},
		"rename of a vendored file": {
			"diff --git a/vendor/x/x.go b/x.go\nsimilarity index 90%\nrename from vendor/x/x.go\nrename to x.go\n--- a/x.go\n+++ b/x.go\n" + hunk,
			`"vendor/x/x.go" is off-limits`,
		},
		"rename headers alone name the vendored file": {
			"rename from vendor/x/x.go\nrename to x.go\n--- a/main.go\n+++ b/main.go\n" + hunk,
			`"vendor/x/x.go" is off-limits`,
		},
		"copy onto go.sum": {
			"copy from main.go\ncopy to go.sum\n--- a/main.go\n+++ b/main.go\n" + hunk,
			`"go.sum" is off-limits`,
		},
		"nested vendor directory": {
			"--- a/internal/vendor/x.go\n+++ b/internal/vendor/x.go\n" + hunk,
			"off-limits",
		},
		"go.work swaps a module": {
			"--- /dev/null\n+++ b/go.work\n@@ -0,0 +1 @@\n+use ./fake\n",
			`"go.work" is off-limits`,
		},
		"test file edit": {
			"--- a/pkg/parse_test.go\n+++ b/pkg/parse_test.go\n" + hunk,
			`test file "pkg/parse_test.go" is off-limits`,
		},
		"new test file": {
			"--- /dev/null\n+++ b/zz_test.go\n@@ -0,0 +1 @@\n+package main\n",
			`test file "zz_test.go"`,
		},
		"golden file under testdata": {
			"--- a/cli/testdata/golden.txt\n+++ b/cli/testdata/golden.txt\n" + hunk,
			`test file "cli/testdata/golden.txt"`,
		},
		"quoted and octal-escaped testdata": {
			"--- \"a/test\\144ata/x\"\n+++ \"b/test\\144ata/x\"\n" + hunk,
			`test file "testdata/x"`,
		},
		"git header names a protected path": {
			"diff --git a/main_test.go b/main_test.go\n--- a/main.go\n+++ b/main.go\n" + hunk,
			`test file "main_test.go"`,
		},
		"context diff names": {
			"*** a/go.sum\n--- b/main.go\n" + hunk,
			`"go.sum" is off-limits`,
		},
		"Index line": {
			"Index: go.mod\n--- a/main.go\n+++ b/main.go\n" + hunk,
			`"go.mod" is off-limits`,
		},
		"escape hidden from a cleaned raw name": {
			"--- a/main.go\n+++ z/../x.go\n" + hunk,
			"escapes repository",
		},
		"git metadata": {
			"--- a/.git\n+++ b/.git\n" + hunk,
			"git metadata",
		},
		"gitattributes": {
			"--- /dev/null\n+++ b/.gitattributes\n@@ -0,0 +1 @@\n+* -diff\n",
			"git metadata",
		},
		"symlink creation": {
			"diff --git a/link.go b/link.go\nnew file mode 120000\n--- /dev/null\n+++ b/link.go\n@@ -0,0 +1 @@\n+go.mod\n",
			"symlinks",
		},
		"symlink target edit": {
			"diff --git a/link.go b/link.go\nindex 1111111..2222222 120000\n--- a/link.go\n+++ b/link.go\n" + hunk,
			"symlinks",
		},
		"submodule": {
			"diff --git a/sub b/sub\nnew file mode 160000\n--- /dev/null\n+++ b/sub\n@@ -0,0 +1 @@\n+Subproject commit 1\n",
			"submodules",
		},
		"mode change": {
			"diff --git a/main.go b/main.go\nold mode 100644\nnew mode 100755\n--- a/main.go\n+++ b/main.go\n" + hunk,
			"file mode changes",
		},
		"git binary patch": {
			"diff --git a/main.go b/main.go\nGIT binary patch\nliteral 0\nHcmV?d00001\n\n--- a/main.go\n+++ b/main.go\n" + hunk,
			"binary",
		},
		"unterminated quote": {
			"--- \"a/main.go\n+++ b/main.go\n" + hunk,
			"unterminated quoted",
		},
		"bad quoted escape": {
			"--- \"a/main\\'.go\"\n+++ b/main.go\n" + hunk,
			"malformed quoted",
		},
		"empty header": {
			"--- \n+++ b/main.go\n" + hunk,
			"malformed patch path",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateUnifiedDiff(tc.patch, Policy{})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
