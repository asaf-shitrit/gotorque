package manifest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"example.com/gotorque/internal/domain"
)

const (
	VersionV1 = "v1"

	DefaultPrimaryMetric              = "wall_time_ns"
	DefaultMinimumImprovementPercent  = 3.0
	DefaultGuardrailRegressionPercent = 2.0
	DefaultMaxCampaignDuration        = 90 * time.Minute
	DefaultMaxCandidatePatches        = 12
	DefaultStopAfterFailures          = 4
	DefaultDiscoveryStallTimeout      = 20 * time.Minute
	DefaultPerCommandTimeoutMultiple  = 2.0
	DefaultMinimumCommandTimeout      = 30 * time.Second
)

// Duration is encoded as a Go duration string in a manifest, for example "90m".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("duration %q must be a positive Go duration", value)
	}
	*d = Duration(parsed)
	return nil
}

type Manifest struct {
	Schema             string                    `json:"$schema,omitempty"`
	Version            string                    `json:"version"`
	Name               string                    `json:"name"`
	Target             Target                    `json:"target"`
	Workloads          WorkloadConfiguration     `json:"workloads"`
	Sandbox            Sandbox                   `json:"sandbox"`
	Normalization      Normalization             `json:"normalization"`
	Performance        PerformancePolicy         `json:"performance"`
	Campaign           CampaignLimits            `json:"campaign"`
	OptimizationPolicy domain.OptimizationPolicy `json:"optimization_policy"`
}

type Target struct {
	Repository string      `json:"repository"`
	Build      BuildTarget `json:"build"`
	// Command is a fixed command or subcommand prefix. It is empty for a root CLI.
	Command []string `json:"command"`
}

type BuildTarget struct {
	Package string `json:"package"`
	Binary  string `json:"binary"`
}

type WorkloadConfiguration struct {
	Seeds     []SeedWorkload                     `json:"seeds"`
	Discovery DiscoverySettings                  `json:"discovery"`
	Tiers     map[domain.WorkloadTier]TierPolicy `json:"tiers"`
}

type SeedWorkload struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Tier        domain.WorkloadTier `json:"tier"`
	Args        []string            `json:"args"`
	Stdin       string              `json:"stdin,omitempty"`
	Files       []FixtureFile       `json:"files,omitempty"`
	Provenance  string              `json:"provenance"`
	Description string              `json:"description,omitempty"`
	Timeout     Duration            `json:"timeout,omitempty"`
}

type FixtureFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type DiscoverySettings struct {
	Enabled    bool     `json:"enabled"`
	Sources    []string `json:"sources"`
	Strategies []string `json:"strategies"`
	Seed       uint64   `json:"seed"`
	MaxCases   int      `json:"max_cases"`
	MaxDepth   int      `json:"max_depth"`
}

type TierPolicy struct {
	Weight             float64 `json:"weight"`
	AcceptanceEligible bool    `json:"acceptance_eligible"`
}

type Sandbox struct {
	Network        string             `json:"network"`
	Filesystem     FilesystemSandbox  `json:"filesystem"`
	Environment    EnvironmentSandbox `json:"environment"`
	MaxProcesses   int                `json:"max_processes"`
	MaxMemoryBytes int64              `json:"max_memory_bytes,omitempty"`
}

type FilesystemSandbox struct {
	Read  string `json:"read"`
	Write string `json:"write"`
}

type EnvironmentSandbox struct {
	Allow       []string `json:"allow"`
	Passthrough []string `json:"passthrough"`
}

type Normalization struct {
	Stdout StreamNormalization `json:"stdout"`
	Stderr StreamNormalization `json:"stderr"`
	Files  []FileNormalization `json:"files"`
}

type StreamNormalization struct {
	Mode  string              `json:"mode"`
	Rules []NormalizationRule `json:"rules,omitempty"`
}

type FileNormalization struct {
	Path  string              `json:"path"`
	Mode  string              `json:"mode"`
	Rules []NormalizationRule `json:"rules,omitempty"`
}

type NormalizationRule struct {
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

type PerformancePolicy struct {
	PrimaryMetric                     string      `json:"primary_metric"`
	MinimumImprovementPercent         float64     `json:"minimum_improvement_percent"`
	MaximumGuardrailRegressionPercent float64     `json:"maximum_guardrail_regression_percent"`
	StatisticalSupportRequired        *bool       `json:"statistical_support_required,omitempty"`
	Guardrails                        []Guardrail `json:"guardrails"`
}

type Guardrail struct {
	Name                     string  `json:"name"`
	MaximumRegressionPercent float64 `json:"maximum_regression_percent,omitempty"`
	Required                 bool    `json:"required"`
}

type CampaignLimits struct {
	MaxDuration               Duration `json:"max_duration"`
	MaxCandidatePatches       int      `json:"max_candidate_patches"`
	MaxConcurrentCandidates   int      `json:"max_concurrent_candidates"`
	StopAfterFailures         int      `json:"stop_after_failures"`
	DiscoveryStallTimeout     Duration `json:"discovery_stall_timeout"`
	PerCommandTimeoutMultiple float64  `json:"per_command_timeout_multiple"`
	MinimumCommandTimeout     Duration `json:"minimum_command_timeout"`
}

func (m *Manifest) ApplyDefaults() {
	if m.Version == "" {
		m.Version = VersionV1
	}
	if m.Performance.PrimaryMetric == "" {
		m.Performance.PrimaryMetric = DefaultPrimaryMetric
	}
	if m.Performance.MinimumImprovementPercent == 0 {
		m.Performance.MinimumImprovementPercent = DefaultMinimumImprovementPercent
	}
	if m.Performance.MaximumGuardrailRegressionPercent == 0 {
		m.Performance.MaximumGuardrailRegressionPercent = DefaultGuardrailRegressionPercent
	}
	if m.Performance.StatisticalSupportRequired == nil {
		value := true
		m.Performance.StatisticalSupportRequired = &value
	}
	if m.Campaign.MaxDuration == 0 {
		m.Campaign.MaxDuration = Duration(DefaultMaxCampaignDuration)
	}
	if m.Campaign.MaxCandidatePatches == 0 {
		m.Campaign.MaxCandidatePatches = DefaultMaxCandidatePatches
	}
	if m.Campaign.MaxConcurrentCandidates == 0 {
		m.Campaign.MaxConcurrentCandidates = 1
	}
	if m.Campaign.StopAfterFailures == 0 {
		m.Campaign.StopAfterFailures = DefaultStopAfterFailures
	}
	if m.Campaign.DiscoveryStallTimeout == 0 {
		m.Campaign.DiscoveryStallTimeout = Duration(DefaultDiscoveryStallTimeout)
	}
	if m.Campaign.PerCommandTimeoutMultiple == 0 {
		m.Campaign.PerCommandTimeoutMultiple = DefaultPerCommandTimeoutMultiple
	}
	if m.Campaign.MinimumCommandTimeout == 0 {
		m.Campaign.MinimumCommandTimeout = Duration(DefaultMinimumCommandTimeout)
	}
	if len(m.Performance.Guardrails) == 0 {
		m.Performance.Guardrails = []Guardrail{
			{Name: "peak_memory_bytes", MaximumRegressionPercent: DefaultGuardrailRegressionPercent, Required: true},
			{Name: "cpu_time_ns", MaximumRegressionPercent: DefaultGuardrailRegressionPercent, Required: true},
			{Name: "binary_size_bytes", MaximumRegressionPercent: DefaultGuardrailRegressionPercent, Required: true},
		}
	}
	for i := range m.Performance.Guardrails {
		if m.Performance.Guardrails[i].MaximumRegressionPercent == 0 {
			m.Performance.Guardrails[i].MaximumRegressionPercent = m.Performance.MaximumGuardrailRegressionPercent
		}
	}
}

func (m Manifest) SemanticValidate() error {
	var problems []string
	if m.Version != VersionV1 {
		problems = append(problems, fmt.Sprintf("version must be %q", VersionV1))
	}
	if strings.TrimSpace(m.Name) == "" {
		problems = append(problems, "name must not be empty")
	}
	if strings.TrimSpace(m.Target.Repository) == "" || strings.TrimSpace(m.Target.Build.Package) == "" || strings.TrimSpace(m.Target.Build.Binary) == "" {
		problems = append(problems, "target repository, build.package, and build.binary are required")
	}
	if m.Workloads.Discovery.Enabled && (m.Workloads.Discovery.MaxCases <= 0 || m.Workloads.Discovery.MaxDepth <= 0) {
		problems = append(problems, "enabled discovery requires positive max_cases and max_depth")
	}
	if len(m.Workloads.Seeds) == 0 {
		problems = append(problems, "at least one workload seed is required")
	}
	seenIDs := map[string]bool{}
	for _, seed := range m.Workloads.Seeds {
		if seenIDs[seed.ID] {
			problems = append(problems, fmt.Sprintf("duplicate workload id %q", seed.ID))
		}
		seenIDs[seed.ID] = true
		if seed.Tier != domain.TierRepresentative && seed.Tier != domain.TierPlausible && seed.Tier != domain.TierStress {
			problems = append(problems, fmt.Sprintf("workload %q has invalid tier %q", seed.ID, seed.Tier))
		}
		for _, file := range seed.Files {
			if err := validateRelativePath(file.Path); err != nil {
				problems = append(problems, fmt.Sprintf("workload %q file: %v", seed.ID, err))
			}
		}
	}
	for _, tier := range []domain.WorkloadTier{domain.TierRepresentative, domain.TierPlausible, domain.TierStress} {
		config, ok := m.Workloads.Tiers[tier]
		if !ok || config.Weight < 0 {
			problems = append(problems, fmt.Sprintf("tier %q must exist with a non-negative weight", tier))
		}
	}
	if m.Sandbox.Network != "deny" && m.Sandbox.Network != "allow" {
		problems = append(problems, "sandbox.network must be deny or allow")
	}
	if m.Sandbox.Filesystem.Read != "repo_and_assets" && m.Sandbox.Filesystem.Read != "manifest_paths" && m.Sandbox.Filesystem.Read != "none" {
		problems = append(problems, "sandbox.filesystem.read must be repo_and_assets, manifest_paths, or none")
	}
	if m.Sandbox.Filesystem.Write != "temp_only" && m.Sandbox.Filesystem.Write != "manifest_paths" && m.Sandbox.Filesystem.Write != "any" {
		problems = append(problems, "sandbox.filesystem.write must be temp_only, manifest_paths, or any")
	}
	if m.Sandbox.MaxProcesses <= 0 {
		problems = append(problems, "sandbox.max_processes must be positive")
	}
	if m.Performance.PrimaryMetric == "" || m.Performance.MinimumImprovementPercent < 0 || m.Performance.MaximumGuardrailRegressionPercent < 0 {
		problems = append(problems, "performance metric and thresholds must be valid")
	}
	if m.Campaign.MaxDuration <= 0 || m.Campaign.MaxCandidatePatches <= 0 || m.Campaign.MaxConcurrentCandidates != 1 || m.Campaign.StopAfterFailures <= 0 || m.Campaign.PerCommandTimeoutMultiple <= 0 || m.Campaign.MinimumCommandTimeout <= 0 {
		problems = append(problems, "campaign limits must be positive and max_concurrent_candidates must be 1")
	}
	if m.OptimizationPolicy != domain.PolicyIdiomatic && m.OptimizationPolicy != domain.PolicySpecialized && m.OptimizationPolicy != domain.PolicyNative {
		problems = append(problems, "optimization_policy must be idiomatic, specialized, or native")
	}
	if err := validateNormalization(m.Normalization); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("manifest semantic validation failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

func validateNormalization(n Normalization) error {
	for name, stream := range map[string]StreamNormalization{"stdout": n.Stdout, "stderr": n.Stderr} {
		if err := validateRules(name, stream.Mode, stream.Rules); err != nil {
			return err
		}
	}
	for _, file := range n.Files {
		if err := validateRelativePath(file.Path); err != nil {
			return fmt.Errorf("normalization file: %v", err)
		}
		if err := validateRules("file "+file.Path, file.Mode, file.Rules); err != nil {
			return err
		}
	}
	return nil
}

func validateRules(name, mode string, rules []NormalizationRule) error {
	switch mode {
	case "exact", "trim_trailing_whitespace", "lines_sorted", "json", "ignore":
	default:
		return fmt.Errorf("normalization %s has invalid mode %q", name, mode)
	}
	for _, rule := range rules {
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			return fmt.Errorf("normalization %s rule %q has invalid pattern: %w", name, rule.Name, err)
		}
	}
	return nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) == "." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) || filepath.Clean(path) == ".." {
		return fmt.Errorf("path %q must be relative and stay within the sandbox", path)
	}
	return nil
}
