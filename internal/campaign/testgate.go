package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"example.com/gotorque/internal/toolchain"
)

// testEvent is one decoded line of `go test -json` output. Only the fields the
// behavior gate needs are declared; the rest of the event is ignored.
type testEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	FailedBuild string `json:"FailedBuild"`
}

// testOutcome is the parsed result of one `go test -json` run.
type testOutcome struct {
	// Tests lists failing tests as `package::Test`, subtests included. A test
	// failure is attributable to a revision, so a candidate is only charged
	// for the ones the unpatched revision did not already have.
	Tests []string
	// Packages lists packages whose tests never ran: a test binary that failed
	// to build, a setup failure such as a directory outside the module, or a
	// TestMain that exited without a failing test to point at. Nothing here is
	// attributable, so it must not be silently subtracted as a pre-existing
	// failure.
	Packages []string
}

// parseTestFailures extracts the failures from `go test -json` output. ok is
// false when the output carried no JSON event at all, which is what a go
// command that cannot even start prints.
func parseTestFailures(output string) (testOutcome, bool) {
	tests := map[string]bool{}
	packages := map[string]string{}
	seen := false
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event testEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		seen = true
		if event.Action != "fail" || event.Package == "" {
			continue
		}
		if event.Test != "" {
			tests[event.Package+"::"+event.Test] = true
			continue
		}
		packages[event.Package] = event.FailedBuild
	}
	outcome := testOutcome{Tests: sortedKeys(tests)}
	for name, failedBuild := range packages {
		// A package that fails while naming no failing test gives the gate
		// nothing to attribute: either the test binary never built, or it died
		// before reporting a test.
		if failedBuild != "" || !hasTestFailure(tests, name) {
			outcome.Packages = append(outcome.Packages, name)
		}
	}
	sort.Strings(outcome.Packages)
	return outcome, seen
}

func hasTestFailure(tests map[string]bool, pkg string) bool {
	for key := range tests {
		if strings.HasPrefix(key, pkg+"::") {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// newTestFailures returns the failures the candidate introduced, sorted. A
// failure the unpatched revision already had is not the candidate's doing and
// cannot be evidence against it.
func newTestFailures(baseline, candidate []string) []string {
	known := make(map[string]bool, len(baseline))
	for _, failure := range baseline {
		known[failure] = true
	}
	var introduced []string
	for _, failure := range candidate {
		if !known[failure] {
			introduced = append(introduced, failure)
		}
	}
	sort.Strings(introduced)
	return introduced
}

// describeTestFailures renders a failure list for a summary or report line,
// bounded so a target with hundreds of failures cannot dominate the record.
func describeTestFailures(failures []string) string {
	const limit = 8
	shown := failures
	suffix := ""
	if len(shown) > limit {
		shown, suffix = shown[:limit], fmt.Sprintf(" (+%d more)", len(failures)-limit)
	}
	return strings.Join(shown, ", ") + suffix
}

// runBaselineTestStep runs the target's own test suite on the unpatched
// revision and records which tests already fail.
//
// The gate exists to attribute breakage to a patch, and it cannot do that when
// the unpatched tree is already red. On Go 1.27 that was the whole gojq
// campaign: its tests assert an encoding/json error string a later Go release
// changed, so every candidate was rejected for a failure no patch could have
// caused. One suite run up front costs seconds and turns that environment
// drift into a recorded set the gate can subtract.
//
// A run that never reached a test — build or setup failure — is refused
// outright: subtracting it would leave a campaign accepting patches with the
// behavior gate effectively switched off.
func (e *Engine) runBaselineTestStep(ctx context.Context) error {
	if e.state.CompletedSteps["baseline_tests"] {
		return nil
	}
	result, err := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: e.state.Repository, JSON: true, Env: []string{"GOTOOLCHAIN=local"}})
	// A failing suite is a non-zero exit, which the toolchain reports as an
	// error alongside the result. The exit status is therefore not the signal
	// here: the parsed JSON is, and it is only unavailable when the go command
	// never got as far as reporting an event.
	outcome, ok := parseTestFailures(string(result.Stdout))
	if !ok {
		if err != nil {
			return fmt.Errorf("run baseline test suite: %w", err)
		}
		return fmt.Errorf("baseline test suite output carried no test events: %s", tail(string(result.Stderr), 400))
	}
	if len(outcome.Packages) > 0 {
		return fmt.Errorf("baseline test suite cannot run against the unpatched revision, so no candidate could be attributed: %s", describeTestFailures(outcome.Packages))
	}
	if result.ExitCode != 0 && len(outcome.Tests) == 0 {
		return fmt.Errorf("baseline test suite fails on the unpatched revision without naming a failing test: %s", tail(string(result.Stderr), 400))
	}
	e.state.BaselineTestFailures = outcome.Tests
	e.state.CompletedSteps["baseline_tests"] = true
	message := "upstream test suite passes on the unpatched revision"
	if len(outcome.Tests) > 0 {
		message = fmt.Sprintf("%d upstream test failure(s) predate every patch and are excluded from the behavior gate: %s", len(outcome.Tests), describeTestFailures(outcome.Tests))
	}
	return e.saveEvent("baseline_tests_completed", message, map[string]any{"failures": outcome.Tests})
}

// classifyTestOutcome decides whether a failing test run says anything about
// the candidate. passed is true when the only failures present are ones the
// unpatched revision already had.
func (e *Engine) classifyTestOutcome(result toolchain.Result, testErr error) (reason string, passed bool) {
	// Failing tests arrive with a non-nil testErr, so the parsed outcome decides
	// whether the candidate is at fault; the error only matters when there is no
	// output to read.
	outcome, ok := parseTestFailures(string(result.Stdout))
	if !ok {
		if testErr != nil {
			return testErr.Error(), false
		}
		return "test run failed without parseable output: " + tail(string(result.Stderr), 400), false
	}
	if len(outcome.Packages) > 0 {
		return "test build or setup failed in: " + describeTestFailures(outcome.Packages), false
	}
	introduced := newTestFailures(e.state.BaselineTestFailures, outcome.Tests)
	if len(introduced) == 0 {
		return "", true
	}
	reason = "new failing tests: " + describeTestFailures(introduced)
	if len(e.state.BaselineTestFailures) > 0 {
		reason += fmt.Sprintf(" (ignoring %d failure(s) that predate the patch)", len(e.state.BaselineTestFailures))
	}
	return reason, false
}
