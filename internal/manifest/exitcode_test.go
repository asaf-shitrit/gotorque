package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// withFirstSeedExitCode returns gron's manifest with the first seed's
// exit_code set to code.
func withFirstSeedExitCode(t *testing.T, code any) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "targets", "gron", "manifest.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	seeds := doc["workloads"].(map[string]any)["seeds"].([]any)
	seeds[0].(map[string]any)["exit_code"] = code
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func TestLoadReadsASeedsExpectedExitCode(t *testing.T) {
	m, err := Load(withFirstSeedExitCode(t, 1))
	require.NoError(t, err)
	require.Equal(t, 1, m.Workloads.Seeds[0].ExitCode)
	require.Equal(t, 0, m.Workloads.Seeds[1].ExitCode, "unset means 0")

	for _, bad := range []any{-1, 256, "1"} {
		_, err := Load(withFirstSeedExitCode(t, bad))
		require.Error(t, err, "exit_code %v", bad)
	}
}
