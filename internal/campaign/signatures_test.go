package campaign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
)

const signaturesFixture = `package lib

type Record struct {
	Fields map[string]string
	order  []string
}

type unused struct{}

func encode(s string) string { return s }

func (r *Record) keys() []string { return r.order }

func (r *Record) Marshal() []byte {
	var out []byte
	for _, k := range r.keys() {
		out = append(out, encode(r.Fields[k])...)
	}
	return out
}

func other() {}
`

func TestTargetSignaturesListsWhatTheTargetUses(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "lib"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib", "lib.go"), []byte(signaturesFixture), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib", "lib_test.go"), []byte("package lib\n\nfunc encode2() {}\n"), 0o600))

	got := targetSignatures(repo, agents.Target{Function: "(*Record).Marshal", Location: "lib/lib.go:15"})
	require.Len(t, got, 3, "%q", got)
	require.True(t, strings.HasPrefix(got[0], "type Record struct"), got[0])
	require.Contains(t, got[0], "Fields map[string]string")
	require.Equal(t, "func (r *Record) keys() []string", got[1])
	require.Equal(t, "func encode(s string) string", got[2])
	for _, g := range got {
		require.NotContains(t, g, "unused")
		require.NotContains(t, g, "other")
		require.NotContains(t, g, "Marshal")
	}
}

func TestTargetSignaturesDegradesQuietly(t *testing.T) {
	require.Nil(t, targetSignatures(t.TempDir(), agents.Target{}))
	require.Nil(t, targetSignatures(t.TempDir(), agents.Target{Function: "f", Location: "missing/x.go:1"}))
}

func TestCutLines(t *testing.T) {
	require.Equal(t, "a\nb", cutLines("a\nb", 2))
	require.Equal(t, "a\nb\n\t// ... cut", cutLines("a\nb\nc", 2))
}

// TestTargetSignaturesOnMiller checks the case that motivated it against the
// real clone, when present: marshalJSONAuxMultiline's context must carry the
// declaration the optimizer got wrong twice.
func TestTargetSignaturesOnMiller(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	repo := filepath.Join(home, "projects", "gotorque-work", "miller")
	if _, err := os.Stat(filepath.Join(repo, "pkg", "mlrval", "mlrmap_json.go")); err != nil {
		t.Skip("miller clone not present")
	}
	got := targetSignatures(repo, agents.Target{Function: "(*Mlrmap).marshalJSONAuxMultiline", Location: "pkg/mlrval/mlrmap_json.go:57"})
	require.NotEmpty(t, got)
	require.LessOrEqual(t, len(got), maxSignatures)
	t.Logf("context:\n%s", strings.Join(got, "\n---\n"))
}
