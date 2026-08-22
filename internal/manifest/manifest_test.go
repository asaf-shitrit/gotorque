package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaults(t *testing.T) {
	data := []byte(`{
      "version":"v1", "name":"test", "target":{"repository":"repo","build":{"package":"./cmd/app","binary":"app"},"command":[]},
      "workloads":{"seeds":[{"id":"seed","name":"seed","tier":"representative","args":[],"provenance":"manifest"}],"discovery":{"enabled":true,"sources":["help"],"strategies":["mutate"],"seed":1,"max_cases":2,"max_depth":1},"tiers":{"representative":{"weight":1,"acceptance_eligible":true},"plausible":{"weight":0.5,"acceptance_eligible":false},"stress":{"weight":0,"acceptance_eligible":false}}},
      "sandbox":{"network":"deny","filesystem":{"read":"repo_and_assets","write":"temp_only"},"environment":{"allow":[],"passthrough":[]},"max_processes":1},
      "normalization":{"stdout":{"mode":"exact"},"stderr":{"mode":"exact"},"files":[]},
      "performance":{"primary_metric":"wall_time_ns","statistical_support_required":true,"guardrails":[]},
      "campaign":{"max_concurrent_candidates":1},"optimization_policy":"idiomatic"
    }`)
	manifest, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Performance.MinimumImprovementPercent != DefaultMinimumImprovementPercent || manifest.Campaign.MaxCandidatePatches != DefaultMaxCandidatePatches {
		t.Fatalf("defaults not applied: %+v", manifest)
	}
	if len(manifest.Performance.Guardrails) != 3 {
		t.Fatalf("expected default guardrails, got %d", len(manifest.Performance.Guardrails))
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	data := []byte(`{"version":"v1","name":"bad","unexpected":true}`)
	if _, err := Load(data); err == nil {
		t.Fatal("expected schema validation error")
	}
}

func TestLoadRejectsUnsafeFixturePath(t *testing.T) {
	data := []byte(`{
      "version":"v1", "name":"test", "target":{"repository":"repo","build":{"package":"./cmd/app","binary":"app"},"command":[]},
      "workloads":{"seeds":[{"id":"seed","name":"seed","tier":"representative","args":[],"files":[{"path":"../escape","content":"x"}],"provenance":"manifest"}],"discovery":{"enabled":true,"sources":["help"],"strategies":["mutate"],"seed":1,"max_cases":2,"max_depth":1},"tiers":{"representative":{"weight":1,"acceptance_eligible":true},"plausible":{"weight":0.5,"acceptance_eligible":false},"stress":{"weight":0,"acceptance_eligible":false}}},
      "sandbox":{"network":"deny","filesystem":{"read":"repo_and_assets","write":"temp_only"},"environment":{"allow":[],"passthrough":[]},"max_processes":1},"normalization":{"stdout":{"mode":"exact"},"stderr":{"mode":"exact"},"files":[]},"performance":{"primary_metric":"wall_time_ns","minimum_improvement_percent":3,"maximum_guardrail_regression_percent":2,"statistical_support_required":true,"guardrails":[]},"campaign":{"max_duration":"90m","max_candidate_patches":12,"max_concurrent_candidates":1,"stop_after_failures":4,"discovery_stall_timeout":"20m","per_command_timeout_multiple":2,"minimum_command_timeout":"30s"},"optimization_policy":"idiomatic"}`)
	if _, err := Load(data); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func TestValidationTargetAssets(t *testing.T) {
	root := filepath.Join("..", "..", "targets")
	for _, name := range []string{"gojq", "scc"} {
		data, err := os.ReadFile(filepath.Join(root, name, "manifest.json"))
		if err != nil {
			t.Fatalf("read %s asset: %v", name, err)
		}
		if _, err := Load(data); err != nil {
			t.Fatalf("validate %s asset: %v", name, err)
		}
	}
}
