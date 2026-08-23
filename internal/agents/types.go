// Package agents defines the judgment-heavy roles used by the optimizer.
//
// The package deliberately does not construct a concrete hosted model. Model
// selection and credentials belong to the composition root and are injected
// through ModelProvider.
package agents

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"example.com/go-agent-optimizer/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
)

// Role identifies one judgment-heavy responsibility in the campaign.
type Role string

const (
	RoleCoordinator Role = "coordinator"
	RoleExplorer    Role = "explorer"
	RoleAnalyst     Role = "analyst"
	RoleOptimizer   Role = "optimizer"
	RoleReviewer    Role = "reviewer"
)

// AllRoles is the stable construction order for the role set.
var AllRoles = []Role{
	RoleCoordinator,
	RoleExplorer,
	RoleAnalyst,
	RoleOptimizer,
	RoleReviewer,
}

// ModelProvider keeps model construction, credentials, and routing outside
// the role package. A provider may return a different model for each role.
type ModelProvider interface {
	ModelFor(context.Context, Role) (model.LLM, error)
}

// ModelProviderFunc adapts a function to ModelProvider.
type ModelProviderFunc func(context.Context, Role) (model.LLM, error)

func (f ModelProviderFunc) ModelFor(ctx context.Context, role Role) (model.LLM, error) {
	return f(ctx, role)
}

// Set contains the five agents expected by the orchestration graph. Tests may
// inject custom ADK agents directly instead of constructing LLM agents.
type Set struct {
	Coordinator adkagent.Agent
	Explorer    adkagent.Agent
	Analyst     adkagent.Agent
	Optimizer   adkagent.Agent
	Reviewer    adkagent.Agent
}

// All returns the role agents in a stable order.
func (s Set) All() []adkagent.Agent {
	return []adkagent.Agent{
		s.Coordinator,
		s.Explorer,
		s.Analyst,
		s.Optimizer,
		s.Reviewer,
	}
}

// CoordinatorResult selects the next bounded experiment. Its recommendation
// is advisory; deterministic nodes still enforce campaign policy and bounds.
type CoordinatorResult struct {
	Objective      string   `json:"objective"`
	NextExperiment string   `json:"next_experiment"`
	Rationale      []string `json:"rationale,omitempty"`
}

type ProposedFixture struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WorkloadProposal is declarative model output. Deterministic code validates
// and materializes it; agents never receive or produce shell commands.
type WorkloadProposal struct {
	Name              string              `json:"name"`
	Arguments         []string            `json:"arguments"`
	Stdin             string              `json:"stdin,omitempty"`
	Fixtures          []ProposedFixture   `json:"fixtures,omitempty"`
	Tier              domain.WorkloadTier `json:"tier"`
	Provenance        string              `json:"provenance"`
	ScalingDimensions map[string]int      `json:"scaling_dimensions,omitempty"`
	ExpectedValid     bool                `json:"expected_valid"`
}

// ExplorerResult proposes reachable CLI entry points and typed workloads.
type ExplorerResult struct {
	EntryPoints []string           `json:"entry_points"`
	Proposals   []WorkloadProposal `json:"proposals"`
	// WorkloadStrategies is accepted for state compatibility; new agents use Proposals.
	WorkloadStrategies []string `json:"workload_strategies,omitempty"`
	Rationale          []string `json:"rationale,omitempty"`
}

// HotPath is an agent interpretation of deterministic profile evidence. It is
// not accepted as measured evidence unless the runner supplied measurements.
type HotPath struct {
	Location   string  `json:"location"`
	Impact     float64 `json:"impact"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
}

// AnalystResult interprets normalized discovery and profiling evidence.
type AnalystResult struct {
	HotPaths            []HotPath `json:"hot_paths"`
	LikelyCauses        []string  `json:"likely_causes,omitempty"`
	CandidateHypotheses []string  `json:"candidate_hypotheses"`
	AdditionalChecks    []string  `json:"additional_checks,omitempty"`
}

// OptimizerResult is one focused, reversible source candidate.
type OptimizerResult struct {
	Hypothesis     string   `json:"hypothesis"`
	Patch          string   `json:"patch"`
	ExpectedEffect string   `json:"expected_effect"`
	Risks          []string `json:"risks,omitempty"`
	ValidationPlan []string `json:"validation_plan,omitempty"`
}

// ReviewerResult challenges a candidate before the policy engine sees it.
// Proceed is a recommendation only and cannot override deterministic policy.
type ReviewerResult struct {
	Proceed          bool     `json:"proceed"`
	BehaviorArgument string   `json:"behavior_argument"`
	Concerns         []string `json:"concerns,omitempty"`
	RequiredChecks   []string `json:"required_checks,omitempty"`
}

// Tolerant decoding: hosted models frequently collapse declared string
// arrays into a single string (and booleans into quoted strings). These
// unmarshalers accept both shapes so deterministic downstream policy —
// not JSON shape pedantry — decides whether a recommendation is usable.

func (r *CoordinatorResult) UnmarshalJSON(data []byte) error {
	type alias CoordinatorResult
	aux := struct {
		*alias
		NextExperiment flexText    `json:"next_experiment"`
		Rationale      flexStrings `json:"rationale,omitempty"`
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.NextExperiment = string(aux.NextExperiment)
	r.Rationale = aux.Rationale
	return nil
}

func (e *ExplorerResult) UnmarshalJSON(data []byte) error {
	type alias ExplorerResult
	aux := struct {
		*alias
		EntryPoints        flexStrings `json:"entry_points"`
		WorkloadStrategies flexStrings `json:"workload_strategies,omitempty"`
		Rationale          flexStrings `json:"rationale,omitempty"`
	}{alias: (*alias)(e)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	e.EntryPoints = aux.EntryPoints
	e.WorkloadStrategies = aux.WorkloadStrategies
	e.Rationale = aux.Rationale
	return nil
}

// flexDims decodes scaling dimensions given as numeric or numeric-string
// values, ignoring unusable entries instead of failing the whole proposal.
type flexDims map[string]int

func (f *flexDims) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] != '{' {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	dims := make(map[string]int, len(raw))
	for key, value := range raw {
		var number json.Number
		if err := json.Unmarshal(value, &number); err == nil {
			if parsed, err := strconv.Atoi(number.String()); err == nil {
				dims[key] = parsed
				continue
			}
		}
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
				dims[key] = parsed
			}
		}
	}
	*f = dims
	return nil
}

func (w *WorkloadProposal) UnmarshalJSON(data []byte) error {
	type alias WorkloadProposal
	aux := struct {
		*alias
		Arguments  flexStrings  `json:"arguments"`
		Stdin      flexText     `json:"stdin,omitempty"`
		Fixtures   flexFixtures `json:"fixtures,omitempty"`
		Tier       flexText     `json:"tier"`
		Provenance flexText     `json:"provenance"`
		Scaling    flexDims     `json:"scaling_dimensions,omitempty"`
	}{alias: (*alias)(w)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	w.Arguments = aux.Arguments
	w.Stdin = string(aux.Stdin)
	w.Fixtures = []ProposedFixture(aux.Fixtures)
	w.Tier = domain.WorkloadTier(aux.Tier)
	w.Provenance = string(aux.Provenance)
	w.ScalingDimensions = map[string]int(aux.Scaling)
	return nil
}

// flexText decodes a JSON string, or extracts usable text from JSON
// objects (via conventional keys) and arrays (first element).
type flexText string

func (f *flexText) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*f = flexText(text)
		return nil
	}
	if trimmed[0] == '[' {
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		for _, element := range list {
			var text string
			if err := json.Unmarshal(element, &text); err == nil && text != "" {
				*f = flexText(text)
				return nil
			}
		}
		return nil
	}
	if trimmed[0] == '{' {
		var object map[string]any
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		for _, key := range []string{"content", "text", "data", "value", "body", "stdin", "input"} {
			if raw, ok := object[key]; ok {
				switch typed := raw.(type) {
				case string:
					*f = flexText(typed)
					return nil
				default:
					encoded, err := json.Marshal(raw)
					if err == nil {
						*f = flexText(encoded)
						return nil
					}
				}
			}
		}
		// Structured values without a conventional text key collapse to
		// their identifying scalar when one exists.
		if extracted := identifyingString(object); extracted != "" {
			*f = flexText(extracted)
			return nil
		}
	}
	// Unknown shape: keep the literal JSON so downstream validation can
	// judge it instead of failing the whole agent turn here.
	*f = flexText(trimmed)
	return nil
}

// flexFixtures decodes fixtures given as an array of {path, content}, a
// single fixture object, or a path-to-content mapping.
type flexFixtures []ProposedFixture

func (f *flexFixtures) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		for _, element := range list {
			if hp, ok := decodeFixture(element); ok {
				*f = append(*f, hp)
			}
		}
		return nil
	case '{':
		var single ProposedFixture
		if err := json.Unmarshal(trimmed, &single); err == nil && single.Path != "" {
			*f = []ProposedFixture{single}
			return nil
		}
		var mapping map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &mapping); err != nil {
			return err
		}
		fixtures := make([]ProposedFixture, 0, len(mapping))
		for path, rawContent := range mapping {
			fixture := ProposedFixture{Path: path}
			var text string
			if err := json.Unmarshal(rawContent, &text); err == nil {
				fixture.Content = text
			} else {
				fixture.Content = string(rawContent)
			}
			fixtures = append(fixtures, fixture)
		}
		*f = fixtures
		return nil
	default:
		// Unusable shapes (bare strings, numbers) are dropped: fixtures are
		// optional and deterministic validation re-checks every materialized
		// workload.
		return nil
	}
}

// decodeFixture accepts a {path, content} object, a bare path string, or
// any object carrying an identifying path/name key.
func decodeFixture(raw json.RawMessage) (ProposedFixture, bool) {
	var fixture ProposedFixture
	if err := json.Unmarshal(raw, &fixture); err == nil && fixture.Path != "" {
		return fixture, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return ProposedFixture{Path: text}, true
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		for _, key := range []string{"path", "name", "file", "filename", "id"} {
			if value, ok := object[key].(string); ok && value != "" {
				fixture := ProposedFixture{Path: value}
				if content, ok := object["content"].(string); ok {
					fixture.Content = content
				}
				return fixture, true
			}
		}
	}
	return ProposedFixture{}, false
}

func decodeHotPath(raw json.RawMessage) (HotPath, bool) {
	var hp HotPath
	if err := json.Unmarshal(raw, &hp); err == nil && hp.Location != "" {
		return hp, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return HotPath{Location: text}, true
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		if extracted := identifyingString(object); extracted != "" {
			return HotPath{Location: extracted}, true
		}
	}
	return HotPath{}, false
}

func (a *AnalystResult) UnmarshalJSON(data []byte) error {
	type alias AnalystResult
	aux := struct {
		*alias
		HotPaths            flexHotPaths `json:"hot_paths"`
		LikelyCauses        flexStrings  `json:"likely_causes,omitempty"`
		CandidateHypotheses flexStrings  `json:"candidate_hypotheses"`
		AdditionalChecks    flexStrings  `json:"additional_checks,omitempty"`
	}{alias: (*alias)(a)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	a.HotPaths = aux.HotPaths
	a.LikelyCauses = aux.LikelyCauses
	a.CandidateHypotheses = aux.CandidateHypotheses
	a.AdditionalChecks = aux.AdditionalChecks
	return nil
}

func (o *OptimizerResult) UnmarshalJSON(data []byte) error {
	type alias OptimizerResult
	aux := struct {
		*alias
		Risks          flexStrings `json:"risks,omitempty"`
		ValidationPlan flexStrings `json:"validation_plan,omitempty"`
	}{alias: (*alias)(o)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	o.Risks = aux.Risks
	o.ValidationPlan = aux.ValidationPlan
	return nil
}

func (rv *ReviewerResult) UnmarshalJSON(data []byte) error {
	type alias ReviewerResult
	aux := struct {
		*alias
		Proceed        flexBool    `json:"proceed"`
		Concerns       flexStrings `json:"concerns,omitempty"`
		RequiredChecks flexStrings `json:"required_checks,omitempty"`
	}{alias: (*alias)(rv)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	rv.Proceed = bool(aux.Proceed)
	rv.Concerns = aux.Concerns
	rv.RequiredChecks = aux.RequiredChecks
	return nil
}

// flexHotPaths decodes hot paths given as an array of HotPath objects, a
// single HotPath object, an object with grouped arrays (measured/suspected),
// or an array of plain location strings.
type flexHotPaths []HotPath

func (f *flexHotPaths) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		for _, element := range list {
			if hp, ok := decodeHotPath(element); ok {
				*f = append(*f, hp)
			}
		}
		return nil
	case '{':
		var grouped map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &grouped); err != nil {
			return err
		}
		if hp, ok := decodeHotPath(trimmed); ok {
			*f = append(*f, hp)
			return nil
		}
		for _, key := range []string{"measured", "suspected", "suspicions", "paths", "hot_paths", "entries", "items"} {
			raw, ok := grouped[key]
			if !ok {
				continue
			}
			var list []json.RawMessage
			if err := json.Unmarshal(raw, &list); err != nil {
				continue
			}
			for _, element := range list {
				if hp, ok := decodeHotPath(element); ok {
					*f = append(*f, hp)
				}
			}
		}
		return nil
	default:
		return nil
	}
}
