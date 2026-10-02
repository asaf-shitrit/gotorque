// Package agents defines the optimizer, the campaign's one model role, and the
// result types every stage of the graph exchanges. The other stages are served
// by Jev and by code.
//
// The package does not construct a concrete hosted model. Model selection and
// credentials belong to the composition root and are injected through
// ModelProvider.
package agents

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/jev"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
)

// Role names one judgment stage of the campaign graph. Only the optimizer is a
// model; the analyst, reviewer and explorer are Jev questions ranked in code,
// and the coordinator is code. The names stay because degradation, usage and
// reports are recorded against them.
type Role string

const (
	RoleCoordinator Role = "coordinator"
	RoleExplorer    Role = "explorer"
	RoleAnalyst     Role = "analyst"
	RoleOptimizer   Role = "optimizer"
	RoleReviewer    Role = "reviewer"
)

// ModelProvider keeps model construction, credentials, and routing outside
// the role package.
type ModelProvider interface {
	OptimizerModel(ctx context.Context) (model.LLM, error)
}

// UsageReporter is an optional ModelProvider capability exposing the shared
// token-usage collector used by decorated models.
type UsageReporter interface {
	UsageReporter() *UsageCollector
}

// ModelProviderFunc adapts a function to ModelProvider.
type ModelProviderFunc func(context.Context) (model.LLM, error)

func (f ModelProviderFunc) OptimizerModel(ctx context.Context) (model.LLM, error) {
	return f(ctx)
}

// Set is what the campaign graph runs on: the optimizer agent, and the Jev
// evaluator that serves the analyst (cause classification), the reviewer
// (behaviour-hazard checks) and the explorer (which of the target's options
// select a processing mode). Tests may inject custom ADK agents and evaluators.
type Set struct {
	Optimizer adkagent.Agent

	// Usage reports cumulative per-role token usage collected by decorated
	// models during a run; nil when the provider does not track usage.
	Usage *UsageCollector

	Jev jev.Evaluator
}

// CoordinatorResult states the campaign's objective. Code writes it; the
// experiment itself is chosen after the analysis (planTarget).
type CoordinatorResult struct {
	Objective      string   `json:"objective"`
	NextExperiment string   `json:"next_experiment"`
	Rationale      []string `json:"rationale,omitempty"`
}

// ExplorerResult states how discovery's extra workloads were chosen: before
// discovery, code finds the target's boolean options and Jev judges which
// select a processing mode.
type ExplorerResult struct {
	Rationale []string `json:"rationale,omitempty"`
}

// HotPath is a measured hot function as the analyst reports it.
type HotPath struct {
	Location   string  `json:"location"`
	Impact     float64 `json:"impact"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
}

// AnalystResult is the Jev analyst's classification of discovery's hot
// functions, ranked in code.
type AnalystResult struct {
	HotPaths            []HotPath `json:"hot_paths"`
	LikelyCauses        []string  `json:"likely_causes,omitempty"`
	CandidateHypotheses []string  `json:"candidate_hypotheses"`
	AdditionalChecks    []string  `json:"additional_checks,omitempty"`
	// Targets are the ranked (function, cause) pairs the Jev analyst flagged,
	// in the order a campaign should attack them; code picks the optimizer's
	// target from them.
	Targets []Target `json:"targets,omitempty"`
}

// Target is one function and one cause the optimizer is told to address.
type Target struct {
	Location string  `json:"location"`
	Function string  `json:"function"`
	Cause    string  `json:"cause"`
	Remedy   string  `json:"remedy"`
	Z        float64 `json:"z"`
	// FixKind names the mechanism Jev chose within the cause, when one stood
	// out; Remedy is then that mechanism's, not the cause's generic one.
	FixKind string `json:"fix_kind,omitempty"`
	// Kind says how much code the target lets a patch change. The zero value
	// is one function, which is also what a target saved before kinds existed
	// reads as.
	Kind TargetKind `json:"kind,omitempty"`
	// Callers is set only on a TargetFunctionSet target (ADR 0027): the
	// callee's consuming callers, ranked, which the patch may change alongside
	// Function.
	Callers []FunctionRef `json:"callers,omitempty"`
	// Context is the declarations, from the target's own package, of the
	// functions, methods and types Function refers to, plus its receiver's
	// type (signatures without bodies), so the optimizer does not have to
	// guess the types around the code it rewrites.
	Context []string `json:"context,omitempty"`
}

// TargetKind names how much of the code a target lets a patch change.
type TargetKind string

const (
	// TargetFunction confines a patch to Target.Function.
	TargetFunction TargetKind = ""
	// TargetFunctionSet confines a patch to Target.Function plus
	// Target.Callers, for a remedy that spans a callee and its callers
	// (ADR 0027).
	TargetFunctionSet TargetKind = "function_set"
)

// IsFunctionSet reports whether the target spans a callee and its callers.
func (t Target) IsFunctionSet() bool { return t.Kind == TargetFunctionSet }

// FunctionRef names one function of a function-set target, in the
// path/receiver-qualified format internal/campaign/causes.go's funcName
// produces, with its repository-relative "path.go:line" location.
type FunctionRef struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

// OptimizerResult is one focused, reversible source candidate. Patch holds a
// unified diff; flexPatch documents the wire shapes accepted for it. The field
// stays a Go string and marshals back as one, so campaign state written by
// either transport reads the same on resume.
//
// FunctionSource and Imports are the other transport (ADR 0022): with a
// code-chosen target, the optimizer returns the target function's whole new
// declaration instead of hand-writing a diff, and leaves Patch empty.
// FunctionSource is the complete replacement declaration (doc comment
// optional); Imports names any import paths it needs that the file does not
// already have. Deterministic code (internal/campaign) finds the named
// function in the base revision, splices FunctionSource in, adds the missing
// imports, and turns the result into an ordinary unified diff before the rest
// of the pipeline sees it. Patch takes precedence when both are set, and
// remains the only transport when there is no target.
//
// FunctionSources is the throwaway_result transport (ADR 0027): with a
// function-set target (Target.IsFunctionSet), the optimizer returns one
// whole declaration per function it changes, callee and callers together,
// instead of one FunctionSource. Decoding is as lenient as every other list
// field here: a single string decodes as a one-element list. FunctionSource
// remains a fallback naming just the callee when FunctionSources is empty,
// for an optimizer that ignores the plural field.
type OptimizerResult struct {
	Hypothesis      string   `json:"hypothesis"`
	Patch           string   `json:"patch"`
	FunctionSource  string   `json:"function_source,omitempty"`
	FunctionSources []string `json:"function_sources,omitempty"`
	Imports         []string `json:"imports,omitempty"`
	ExpectedEffect  string   `json:"expected_effect"`
	Risks           []string `json:"risks,omitempty"`
	ValidationPlan  []string `json:"validation_plan,omitempty"`
}

// ReviewerResult is the Jev reviewer's behaviour-hazard checks on a candidate.
// Proceed is a recommendation only and cannot override deterministic policy.
type ReviewerResult struct {
	Proceed          bool     `json:"proceed"`
	BehaviorArgument string   `json:"behavior_argument"`
	Concerns         []string `json:"concerns,omitempty"`
	RequiredChecks   []string `json:"required_checks,omitempty"`
}

// These unmarshalers accept the shapes models actually emit (scalar
// where an array was declared, quoted booleans) so that downstream
// deterministic policy, not JSON shape checking, decides usability.

// flexText decodes a JSON string, or extracts usable text from JSON
// objects (via conventional keys) and arrays (first element).
type flexText string

func (f *flexText) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if emptyOrNull(trimmed) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*f = flexText(text)
		return nil
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*f = flexText(firstStringInList(list))
		return nil
	case '{':
		var object map[string]any
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		if text, ok := flexTextFromObject(object); ok {
			*f = text
			return nil
		}
	}
	// Unknown shape: keep the literal JSON so downstream validation can
	// judge it instead of failing the whole agent turn here.
	*f = flexText(trimmed)
	return nil
}

func firstStringInList(list []json.RawMessage) string {
	for _, element := range list {
		var text string
		if err := json.Unmarshal(element, &text); err == nil && text != "" {
			return text
		}
	}
	return ""
}

func flexTextFromObject(object map[string]any) (flexText, bool) {
	for _, key := range []string{"content", "text", "data", "value", "body", "stdin", "input"} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		if text, ok := raw.(string); ok {
			return flexText(text), true
		}
		encoded, err := json.Marshal(raw)
		if err == nil {
			return flexText(encoded), true
		}
	}
	// Structured values without a conventional text key collapse to
	// their identifying scalar when one exists.
	if extracted := identifyingString(object); extracted != "" {
		return flexText(extracted), true
	}
	return "", false
}

func (o *OptimizerResult) UnmarshalJSON(data []byte) error {
	type alias OptimizerResult
	aux := struct {
		*alias
		Patch           flexPatch   `json:"patch"`
		FunctionSource  flexText    `json:"function_source,omitempty"`
		FunctionSources flexStrings `json:"function_sources,omitempty"`
		Imports         flexStrings `json:"imports,omitempty"`
		Risks           flexStrings `json:"risks,omitempty"`
		ValidationPlan  flexStrings `json:"validation_plan,omitempty"`
	}{alias: (*alias)(o)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	o.Patch = string(aux.Patch)
	o.FunctionSource = string(aux.FunctionSource)
	o.FunctionSources = aux.FunctionSources
	o.Imports = aux.Imports
	o.Risks = aux.Risks
	o.ValidationPlan = aux.ValidationPlan
	return nil
}

// flexPatch decodes a unified diff sent either as an array of diff lines or as
// one JSON string.
//
// A diff is the worst payload a JSON string can carry: every line break, quote,
// and backslash of the source has to survive escaping, and one miss makes the
// entire role response unparseable, not just the patch. One array element per
// line removes the line breaks — by far the largest share of the escaping — and
// confines whatever escaping the model still gets wrong to the single line that
// contains it. The string form remains accepted for two reasons: proposals
// persisted by earlier runs replay through this decoder when a campaign
// directory resumes, and a model that ignores the instruction still produces a
// candidate the deterministic gates can judge.
type flexPatch string

func (p *flexPatch) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if emptyOrNull(trimmed) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*p = flexPatch(text)
		return nil
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*p = flexPatch(joinPatchLines(list))
		return nil
	}
	// Wrapper objects such as {"content": "..."} collapse through the shared
	// text extraction rather than failing the whole optimizer turn.
	var text flexText
	if err := text.UnmarshalJSON(trimmed); err != nil {
		return err
	}
	*p = flexPatch(text)
	return nil
}

// joinPatchLines reassembles an array-of-lines patch into canonical diff text:
// exactly one newline between lines and one at the end, which is the shape
// NormalizeUnifiedDiff and git apply expect. Elements that already carry their
// own line terminator are trimmed rather than doubled, because a duplicated
// newline reads downstream as a blank line inside a hunk. Non-string elements
// are kept as their compact JSON so a garbled line is rejected by diff
// validation instead of silently vanishing from the patch.
func joinPatchLines(list []json.RawMessage) string {
	lines := make([]string, 0, len(list))
	for _, element := range list {
		line, err := flexStringElement(element)
		if err != nil {
			continue
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
	joined := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if joined == "" {
		return ""
	}
	return joined + "\n"
}
