package manifest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSeedExpandsRepeatedInputs(t *testing.T) {
	seed := SeedWorkload{
		Stdin: "a,1\n", StdinHeader: "k,v\n", StdinRepeat: 3,
		Files: []FixtureFile{
			{Path: "rows.csv", Header: "id\n", Content: "7\n", Repeat: 2},
			{Path: "plain.txt", Content: "x"},
		},
	}
	require.Equal(t, "k,v\na,1\na,1\na,1\n", string(seed.StdinBytes()))
	fixtures := seed.Fixtures()
	require.Equal(t, "id\n7\n7\n", string(fixtures["rows.csv"]))
	require.Equal(t, "x", string(fixtures["plain.txt"]), "repeat 0 means once")
	require.Empty(t, SeedWorkload{}.StdinBytes())
}

// repeatManifest is a minimal valid manifest whose one seed uses the repeat
// fields with the given values.
func repeatManifest(stdinRepeat, fileRepeat string) []byte {
	seed := `{"id":"seed","name":"seed","tier":"representative","args":[],"stdin":"r\n","stdin_header":"h\n","stdin_repeat":` + stdinRepeat +
		`,"files":[{"path":"f.csv","content":"x\n","header":"k\n","repeat":` + fileRepeat + `}],"provenance":"manifest"}`
	return []byte(strings.ReplaceAll(`{
      "version":"v1", "name":"test", "target":{"repository":"repo","build":{"package":"./cmd/app","binary":"app"},"command":[]},
      "workloads":{"seeds":[SEED],"discovery":{"enabled":true,"sources":["help"],"strategies":["mutate"],"seed":1,"max_cases":2,"max_depth":1},"tiers":{"representative":{"weight":1,"acceptance_eligible":true},"plausible":{"weight":0.5,"acceptance_eligible":false},"stress":{"weight":0,"acceptance_eligible":false}}},
      "sandbox":{"network":"deny","filesystem":{"read":"repo_and_assets","write":"temp_only"},"environment":{"allow":[],"passthrough":[]},"max_processes":1},
      "normalization":{"stdout":{"mode":"exact"},"stderr":{"mode":"exact"},"files":[]},
      "performance":{"primary_metric":"wall_time_ns","statistical_support_required":true,"guardrails":[]},
      "campaign":{"max_concurrent_candidates":1},"optimization_policy":"idiomatic"
    }`, "SEED", seed))
}

func TestLoadAcceptsRepeatFields(t *testing.T) {
	m, err := Load(repeatManifest("4", "2"))
	require.NoError(t, err)
	seed := m.Workloads.Seeds[0]
	require.Equal(t, "h\nr\nr\nr\nr\n", string(seed.StdinBytes()))
	require.Equal(t, "k\nx\nx\n", string(seed.Fixtures()["f.csv"]))
}

func TestLoadRejectsANonPositiveRepeat(t *testing.T) {
	_, err := Load(repeatManifest("0", "2"))
	require.Error(t, err)
	_, err = Load(repeatManifest("2", "-1"))
	require.Error(t, err)
}
