package candidate

import (
	"github.com/stretchr/testify/require"
	"testing"
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
