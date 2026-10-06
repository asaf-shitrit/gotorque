package campaign

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/toolchain"
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
	key := event.Package + "::" + stableTestName(event.Test)
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
		known[stableTestName(failure)] = true
	}
	var introduced []string
	for _, failure := range candidate {
		if !known[stableTestName(failure)] {
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
// A run in which no test passed at all — every package failed to build or
// set up — is refused outright: subtracting it would leave a campaign
// accepting patches with the behavior gate effectively switched off. A run in
// which some packages' tests could not build while others ran is kept, and
// those packages are recorded as BaselineUnbuildable and subtracted from each
// candidate's setup failures the way predating test failures are: their tests
// never ran on the unpatched revision, so they cannot speak for or against a
// patch. miller was refused whole before this for C benchmark sources under
// scripts/perf and two example commands, while hundreds of its tests ran.
// The target's own package cannot hide here: the baseline build that runs
// before this step fails first.
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
	outcome, err := e.baseSuite().run(ctx, 0)
	if err != nil {
		return err
	}
	e.state.BaselineTestFailures = outcome.Tests
	e.state.BaselineTestPasses = outcome.Passed
	e.state.BaselineUnbuildable = outcome.Packages
	e.state.CompletedSteps["baseline_tests"] = true
	e.state.CompletedSteps[baselinePassesStep] = true
	return e.saveEvent("baseline_tests_completed", baselineTestMessage(outcome), map[string]any{"failures": outcome.Tests, "passes": len(outcome.Passed)})
}

// runBaselineSuite runs the suite on the unpatched revision and refuses an
// outcome the gate could not attribute a candidate against.
//
// The suite runs in a disposable worktree at the base revision, the way every
// candidate's does, never in the canonical checkout: miller's tests rewrite
// tracked fixtures under test/input, and one run left seven of them truncated
// in the checkout the function_source transport reads its base from.
//
// count is passed to go test's -count: 0 leaves the test cache in play, and
// 1 forces the suite to run, which a re-check needs because the base
// revision builds the same test binary and would replay the first run.
func (b baseSuite) run(ctx context.Context, count int) (testOutcome, error) {
	dir, cleanup, err := b.worktree(ctx)
	if err != nil {
		return testOutcome{}, err
	}
	defer cleanup()
	result, err := b.toolchain.Test(ctx, toolchain.TestRequest{Repository: dir, JSON: true, Count: count, Env: []string{"GOTOOLCHAIN=local"}})
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
	if len(outcome.Packages) > 0 && len(outcome.Passed) == 0 {
		return testOutcome{}, fmt.Errorf("baseline test suite cannot run against the unpatched revision, so no candidate could be attributed: %s", describeTestFailures(outcome.Packages))
	}
	if result.ExitCode != 0 && len(outcome.Tests) == 0 && len(outcome.Packages) == 0 {
		return testOutcome{}, fmt.Errorf("baseline test suite fails on the unpatched revision without naming a failing test: %s", tail(string(result.Stderr), 400))
	}
	return outcome, nil
}

func baselineTestMessage(outcome testOutcome) string {
	message := fmt.Sprintf("upstream test suite passes on the unpatched revision; %d passing test(s) must pass for every candidate", len(outcome.Passed))
	if len(outcome.Tests) > 0 {
		message = fmt.Sprintf("%d upstream test failure(s) predate every patch and are excluded from the behavior gate: %s; %d passing test(s) must pass for every candidate", len(outcome.Tests), describeTestFailures(outcome.Tests), len(outcome.Passed))
	}
	if len(outcome.Packages) > 0 {
		message += fmt.Sprintf("; %d package(s) whose tests cannot build on the unpatched revision are excluded: %s", len(outcome.Packages), describeTestFailures(outcome.Packages))
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
func classifyTestOutcome(b testBaseline, result toolchain.Result, testErr error) (reason string, passed bool) {
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
	if broken := newTestFailures(b.unbuildable(), outcome.Packages); len(broken) > 0 {
		return "test build or setup failed in: " + describeTestFailures(broken), false
	}
	if reason := newFailureReason(b, outcome.Tests); reason != "" {
		return reason, false
	}
	if lost := lostBaselinePasses(b.passes(), outcome); len(lost) > 0 {
		return "tests that passed on the unpatched revision did not pass: " + describeTestFailures(lost), false
	}
	return "", true
}

// maxBaselineRechecks bounds how many times one campaign re-runs the
// unpatched suite to tell an unstable test from a lost one.
const maxBaselineRechecks = 2

// pruneUnstablePasses re-runs the unpatched suite when a candidate's only
// fault is baseline-passing tests that did not run at all, and stops
// requiring every test that the fresh run did not pass either. It reports
// whether the required set shrank, in which case the candidate is judged
// again against it.
//
// A test can generate its subtests from a random seed: cue's TestSortRandom
// seeds itself from rand.Uint64 and runs one subtest per permutation of
// random inputs, so TestSortRandom/0/2 exists on one run and not the next.
// Two cue candidates were rejected for "losing" such subtests. The re-run is
// of the unpatched revision in a fresh worktree, so nothing a patch does can
// decide which tests count as unstable, and a test the fresh run passes
// stays required: a candidate that stopped running it is still rejected.
func (ev *evaluator) pruneUnstablePasses(ctx context.Context, b testBaseline, result toolchain.Result) bool {
	if b.rechecks() >= maxBaselineRechecks {
		return false
	}
	outcome, ok := parseTestFailures(string(result.Stdout))
	if !ok || !onlyVanished(b, outcome) {
		return false
	}
	b.spendRecheck()
	fresh, err := ev.baseSuite().run(ctx, 1)
	if err != nil {
		_ = ev.journal.event("baseline_tests_rechecked", "re-running the unpatched suite failed, so every baseline pass stays required: "+err.Error(), nil)
		return false
	}
	stable, unstable := splitStable(b.passes(), fresh.Passed)
	if len(unstable) == 0 {
		_ = ev.journal.event("baseline_tests_rechecked", "every baseline pass passed again on the unpatched revision, so the candidate's missing tests are its own", nil)
		return false
	}
	b.requirePasses(stable)
	_ = ev.journal.event("baseline_tests_rechecked", fmt.Sprintf("%d baseline-passing test(s) did not pass on a second run of the unpatched revision and are no longer required: %s", len(unstable), describeTestFailures(unstable)), map[string]any{"unstable": unstable})
	return true
}

// onlyVanished reports whether the outcome's one fault is baseline-passing
// tests that did not run. A skip, a new failure or a broken package is a
// verdict a baseline re-run cannot change, so it is not worth the run.
func onlyVanished(b testBaseline, outcome testOutcome) bool {
	if len(newTestFailures(b.unbuildable(), outcome.Packages)) > 0 || newFailureReason(b, outcome.Tests) != "" {
		return false
	}
	lost := lostBaselinePasses(b.passes(), outcome)
	for _, name := range lost {
		if !strings.HasSuffix(name, " (did not run)") {
			return false
		}
	}
	return len(lost) > 0
}

// splitStable divides the required tests into those still required and
// those that are not. A subtest missing from the fresh run marks its whole
// top-level test's subtree as generated anew on every run: cue's
// TestSortRandom draws fresh permutations per run, so after the first
// re-check dropped 2452 of its subtests the next candidate still missed
// TestSortRandom/12/2, one neither baseline run had lost. Every subtest
// under such a test stops being required; the top-level test itself stays
// required whenever the fresh run passed it, and it fails if any of its
// subtests fails, so a failure still rejects.
func splitStable(required, fresh []string) (stable, unstable []string) {
	passed := stringSet(fresh)
	randomized := map[string]bool{}
	for _, name := range required {
		if !passed[name] {
			if top, sub := topLevelTest(name); sub {
				randomized[top] = true
			}
		}
	}
	for _, name := range required {
		top, sub := topLevelTest(name)
		if passed[name] && (!sub || !randomized[top]) {
			stable = append(stable, name)
		} else {
			unstable = append(unstable, name)
		}
	}
	return stable, unstable
}

// topLevelTest returns the `package::Test` a required name belongs to, and
// whether the name is a subtest of it.
func topLevelTest(name string) (string, bool) {
	pkg, test, ok := strings.Cut(name, "::")
	if !ok {
		return name, false
	}
	top, _, sub := strings.Cut(test, "/")
	return pkg + "::" + top, sub
}

func newFailureReason(b testBaseline, failures []string) string {
	introduced := newTestFailures(b.failures(), failures)
	if len(introduced) == 0 {
		return ""
	}
	reason := "new failing tests: " + describeTestFailures(introduced)
	if n := len(b.failures()); n > 0 {
		reason += fmt.Sprintf(" (ignoring %d failure(s) that predate the patch)", n)
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
		name = stableTestName(name)
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

// pointerAddress matches a printed pointer (0x7856190ebbc0): Go's %#v writes
// one for every pointer field, and table tests that name subtests after
// their input value carry them into test names.
var pointerAddress = regexp.MustCompile(`0x[0-9a-fA-F]{6,}`)

// stableTestName replaces pointer addresses in a test name with a
// placeholder. hcl's TestVariables names its subtests after a %#v of the
// expression under test, so every run names them differently, and on
// live1-hclfmt every candidate was rejected because baseline subtests "did
// not run": they had run, under new addresses. Names that differ only by
// address now compare equal, on both sides of the gate and for baselines
// persisted before this existed.
func stableTestName(name string) string {
	return pointerAddress.ReplaceAllString(name, "0x…")
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// baseSuite runs the target's test suite on the unpatched revision in a
// disposable worktree under the campaign directory: the campaign's baseline
// step records what it passes, and the test gate runs it again to tell an
// unstable test from a lost one.
type baseSuite struct {
	toolchain                 *toolchain.Toolchain
	dir, repository, revision string
}

// baselineWorktree checks out the campaign's base revision in a fresh
// worktree under the campaign directory and returns it with its cleanup. A
// worktree left behind by an interrupted run is removed first.
func (b baseSuite) worktree(ctx context.Context) (string, func(), error) {
	dir := filepath.Join(b.dir, "worktrees", "baseline-tests")
	_, _ = b.toolchain.RemoveWorktree(ctx, b.repository, dir)
	if err := os.RemoveAll(dir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", nil, err
	}
	if _, err := b.toolchain.CreateWorktree(ctx, b.repository, dir, b.revision); err != nil {
		return "", nil, fmt.Errorf("create baseline test worktree: %w", err)
	}
	cleanup := func() { _, _ = b.toolchain.RemoveWorktree(context.WithoutCancel(ctx), b.repository, dir) }
	return dir, cleanup, nil
}
