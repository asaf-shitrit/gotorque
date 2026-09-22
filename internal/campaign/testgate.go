package campaign

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Passed lists passing tests as `package::Test`, subtests included. A
	// failure list cannot show a test that no longer runs, so the gate also
	// needs to know which tests the unpatched revision passed.
	Passed []string
	// Skipped lists tests that reported a skip, so a lost pass can be named as
	// skipped rather than as never run.
	Skipped []string
}

// testTally accumulates per-test results while a `go test -json` stream is
// decoded. A package-level event carries an empty Test and only matters when
// it fails.
type testTally struct {
	failed, passed, skipped map[string]bool
	packages                map[string]string
}

func (t *testTally) record(event testEvent) {
	if event.Package == "" {
		return
	}
	if event.Test == "" {
		if event.Action == "fail" {
			t.packages[event.Package] = event.FailedBuild
		}
		return
	}
	key := event.Package + "::" + event.Test
	switch event.Action {
	case "fail":
		t.failed[key] = true
	case "pass":
		t.passed[key] = true
	case "skip":
		t.skipped[key] = true
	}
}

// parseTestFailures extracts per-test results from `go test -json` output. ok
// is false when the output carried no JSON event at all, which is what a go
// command that cannot even start prints.
func parseTestFailures(output string) (testOutcome, bool) {
	tally := testTally{failed: map[string]bool{}, passed: map[string]bool{}, skipped: map[string]bool{}, packages: map[string]string{}}
	seen := false
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			// io.EOF ends a complete stream; anything else ends an unreadable
			// one, whose unread tests then count as not passed.
			break
		}
		seen = true
		tally.record(event)
	}
	outcome := testOutcome{Tests: sortedKeys(tally.failed), Passed: sortedKeys(tally.passed), Skipped: sortedKeys(tally.skipped)}
	for name, failedBuild := range tally.packages {
		// A package that fails while naming no failing test gives the gate
		// nothing to attribute: either the test binary never built, or it died
		// before reporting a test.
		if failedBuild != "" || !hasTestFailure(tally.failed, name) {
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

// baselinePassesStep marks a baseline test run that recorded the passing
// tests as well as the failing ones. It is separate from "baseline_tests"
// because campaigns persisted before passes were recorded have that step set.
const baselinePassesStep = "baseline_test_passes"

// runBaselineTestStep runs the target's own test suite on the unpatched
// revision and records which tests already fail and which pass.
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
//
// A campaign persisted before passes were recorded has "baseline_tests" set
// and no pass set, and the step runs again for it rather than letting the gate
// treat the missing set as "nothing to keep passing". That reading would leave
// the campaign unable to see a skipped or vanished test for the rest of its
// life, since nothing else ever records the set; the re-run costs one suite
// run, the same price the first one was judged worth. Both sets are replaced
// together, so they come from one run in the environment that the remaining
// candidates will be tested in.
func (e *Engine) runBaselineTestStep(ctx context.Context) error {
	if e.state.CompletedSteps["baseline_tests"] && e.state.CompletedSteps[baselinePassesStep] {
		return nil
	}
	outcome, err := e.runBaselineSuite(ctx)
	if err != nil {
		return err
	}
	e.state.BaselineTestFailures = outcome.Tests
	e.state.BaselineTestPasses = outcome.Passed
	e.state.CompletedSteps["baseline_tests"] = true
	e.state.CompletedSteps[baselinePassesStep] = true
	return e.saveEvent("baseline_tests_completed", baselineTestMessage(outcome), map[string]any{"failures": outcome.Tests, "passes": len(outcome.Passed)})
}

// runBaselineSuite runs the suite on the unpatched revision and refuses an
// outcome the gate could not attribute a candidate against.
func (e *Engine) runBaselineSuite(ctx context.Context) (testOutcome, error) {
	result, err := e.toolchain.Test(ctx, toolchain.TestRequest{Repository: e.state.Repository, JSON: true, Env: []string{"GOTOOLCHAIN=local"}})
	// A failing suite is a non-zero exit, which the toolchain reports as an
	// error alongside the result. The exit status is therefore not the signal
	// here: the parsed JSON is, and it is only unavailable when the go command
	// never got as far as reporting an event.
	outcome, ok := parseTestFailures(string(result.Stdout))
	if !ok {
		if err != nil {
			return testOutcome{}, fmt.Errorf("run baseline test suite: %w", err)
		}
		return testOutcome{}, fmt.Errorf("baseline test suite output carried no test events: %s", tail(string(result.Stderr), 400))
	}
	if len(outcome.Packages) > 0 {
		return testOutcome{}, fmt.Errorf("baseline test suite cannot run against the unpatched revision, so no candidate could be attributed: %s", describeTestFailures(outcome.Packages))
	}
	if result.ExitCode != 0 && len(outcome.Tests) == 0 {
		return testOutcome{}, fmt.Errorf("baseline test suite fails on the unpatched revision without naming a failing test: %s", tail(string(result.Stderr), 400))
	}
	return outcome, nil
}

func baselineTestMessage(outcome testOutcome) string {
	message := fmt.Sprintf("upstream test suite passes on the unpatched revision; %d passing test(s) must pass for every candidate", len(outcome.Passed))
	if len(outcome.Tests) > 0 {
		message = fmt.Sprintf("%d upstream test failure(s) predate every patch and are excluded from the behavior gate: %s; %d passing test(s) must pass for every candidate", len(outcome.Tests), describeTestFailures(outcome.Tests), len(outcome.Passed))
	}
	return message
}

// classifyTestOutcome decides whether a candidate's test run says anything
// against it. passed is true when every test the unpatched revision passed
// passed again and the only failures present are ones it already had.
//
// A clean exit is not enough on its own. A suite exits zero after a test was
// skipped, deleted, or never compiled into the binary, and comparing failure
// sets could not see any of those: a test that stops running cannot fail.
func (e *Engine) classifyTestOutcome(result toolchain.Result, testErr error) (reason string, passed bool) {
	// Failing tests arrive with a non-nil testErr, so the parsed outcome decides
	// whether the candidate is at fault; the error only matters when there is no
	// output to read. A clean exit with nothing to read still has to show the
	// baseline's passing tests, so it falls through to that check.
	outcome, ok := parseTestFailures(string(result.Stdout))
	if !ok && (testErr != nil || result.ExitCode != 0) {
		if testErr != nil {
			return testErr.Error(), false
		}
		return "test run failed without parseable output: " + tail(string(result.Stderr), 400), false
	}
	if len(outcome.Packages) > 0 {
		return "test build or setup failed in: " + describeTestFailures(outcome.Packages), false
	}
	if reason := e.newFailureReason(outcome.Tests); reason != "" {
		return reason, false
	}
	if lost := lostBaselinePasses(e.state.BaselineTestPasses, outcome); len(lost) > 0 {
		return "tests that passed on the unpatched revision did not pass: " + describeTestFailures(lost), false
	}
	return "", true
}

func (e *Engine) newFailureReason(failures []string) string {
	introduced := newTestFailures(e.state.BaselineTestFailures, failures)
	if len(introduced) == 0 {
		return ""
	}
	reason := "new failing tests: " + describeTestFailures(introduced)
	if len(e.state.BaselineTestFailures) > 0 {
		reason += fmt.Sprintf(" (ignoring %d failure(s) that predate the patch)", len(e.state.BaselineTestFailures))
	}
	return reason
}

// lostBaselinePasses returns the tests the unpatched revision passed that the
// candidate run did not pass, sorted and marked skipped or did not run. One
// that failed outright is named as a new failure before this is consulted.
func lostBaselinePasses(baseline []string, outcome testOutcome) []string {
	passed := stringSet(outcome.Passed)
	skipped := stringSet(outcome.Skipped)
	var lost []string
	for _, name := range baseline {
		switch {
		case passed[name]:
		case skipped[name]:
			lost = append(lost, name+" (skipped)")
		default:
			lost = append(lost, name+" (did not run)")
		}
	}
	sort.Strings(lost)
	return lost
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
