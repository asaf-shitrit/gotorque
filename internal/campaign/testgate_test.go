package campaign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/toolchain"
)

// The fixtures are captured from real `go test -json` runs rather than written
// by hand: the sampler parser once matched only a report shape the tool never
// produced, and this gate decides whether a patch is accepted.
func TestParseTestFailuresReadsRealFailingSuite(t *testing.T) {
	output, err := os.ReadFile(filepath.Join("testdata", "gotest-json-failing-suite.txt"))
	require.NoError(t, err)
	outcome, ok := parseTestFailures(string(output))
	require.True(t, ok)
	require.Equal(t, []string{
		"github.com/itchyny/gojq/cli::TestCliRun",
		"github.com/itchyny/gojq/cli::TestCliRun/stream_option_with_unterminated_input",
	}, outcome.Tests)
	require.Empty(t, outcome.Packages, "a package that failed because its test failed is not a build failure")
}

func TestParseTestFailuresSeparatesSetupFailures(t *testing.T) {
	output, err := os.ReadFile(filepath.Join("testdata", "gotest-json-setup-failure.txt"))
	require.NoError(t, err)
	outcome, ok := parseTestFailures(string(output))
	require.True(t, ok)
	require.Empty(t, outcome.Tests)
	require.Equal(t, []string{"./..."}, outcome.Packages)
}

func TestParseTestFailuresRejectsNonJSON(t *testing.T) {
	_, ok := parseTestFailures("go: go.mod file not found in current directory or any parent directory\n")
	require.False(t, ok)
}

func TestNewTestFailuresSubtractsWhatTheBaselineAlreadyHad(t *testing.T) {
	baseline := []string{"pkg::TestOld", "pkg::TestAlsoOld"}
	require.Empty(t, newTestFailures(baseline, []string{"pkg::TestOld"}))
	require.Equal(t, []string{"pkg::TestNew"}, newTestFailures(baseline, []string{"pkg::TestNew", "pkg::TestOld", "pkg::TestAlsoOld"}))
}

func TestClassifyTestOutcomeOnlyChargesNewFailures(t *testing.T) {
	candidate := `{"Action":"fail","Package":"pkg","Test":"TestOld"}
{"Action":"fail","Package":"pkg"}
`
	// A failing suite reaches the gate as a toolchain error, so every case here
	// carries one: passing a nil error would test a path the campaign never takes.
	fixtureErr := errors.New("go test -mod=readonly -json ./...: exit status 1")
	engine := &Engine{state: State{BaselineTestFailures: []string{"pkg::TestOld"}}}
	reason, passed := engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(candidate), ExitCode: 1}, fixtureErr)
	require.True(t, passed, "reason = %q", reason)
	require.Empty(t, reason)

	withNewFailure := candidate + `{"Action":"fail","Package":"pkg","Test":"TestNew"}
`
	reason, passed = engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(withNewFailure), ExitCode: 1}, fixtureErr)
	require.False(t, passed)
	require.Contains(t, reason, "pkg::TestNew")
	require.Contains(t, reason, "ignoring 1 failure(s) that predate the patch")
}

func TestClassifyTestOutcomeNeverSubtractsBuildFailure(t *testing.T) {
	engine := &Engine{state: State{BaselineTestFailures: []string{"pkg::TestOld"}}}
	output := `{"Action":"fail","Package":"pkg","FailedBuild":"pkg"}
`
	reason, passed := engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(output), ExitCode: 1}, nil)
	require.False(t, passed)
	require.Contains(t, reason, "test build or setup failed in: pkg")
}

// A target whose own suite is already red must still let a candidate through
// when the candidate breaks nothing new, and must still refuse one that breaks
// a test the unpatched revision passed.
func TestBehaviorGateSubtractsPreExistingFailuresInARealRepository(t *testing.T) {
	repo := repositoryWithTestOutcome(t, "TestPreexisting")
	manifestPath := writeManifest(t, t.TempDir())
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: manifestPath,
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	require.NoError(t, engine.runBaselineTestStep(context.Background()))
	require.Equal(t, []string{"test.local/fixture::TestPreexisting"}, engine.State().BaselineTestFailures)

	var evidence orchestrator.CandidateEvidence
	require.True(t, engine.candidateTestsPassed(context.Background(), repo, &evidence), "evidence: %+v", evidence)
	require.Empty(t, evidence.Summary, "an untriggered gate must not label the candidate")

	broken := repositoryWithTestOutcome(t, "TestPreexisting", "TestIntroduced")
	evidence = orchestrator.CandidateEvidence{}
	require.False(t, engine.candidateTestsPassed(context.Background(), broken, &evidence))
	require.Contains(t, evidence.Summary, "test.local/fixture::TestIntroduced")
	require.Contains(t, evidence.Summary, "ignoring 1 failure(s) that predate the patch")
	require.False(t, evidence.SafetyChecksPassed)
}

// A suite that cannot run at all leaves nothing to attribute, so the campaign
// stops instead of reporting a gate that is quietly switched off.
func TestBaselineStepRefusesUnattributableFailure(t *testing.T) {
	repo := repositoryWithTestOutcome(t)
	manifestPath := writeManifest(t, t.TempDir())
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: manifestPath,
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	// Removing the module leaves the go command unable to run any test.
	require.NoError(t, os.Remove(filepath.Join(repo, "go.mod")))
	err = engine.runBaselineTestStep(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no candidate could be attributed")
}

// repositoryWithTestOutcome builds a clean repository whose test suite fails
// for exactly the named tests.
func repositoryWithTestOutcome(t *testing.T, failing ...string) string {
	t.Helper()
	repo := makeRepository(t)
	var body strings.Builder
	body.WriteString("package main\n\nimport \"testing\"\n")
	for _, name := range failing {
		body.WriteString("\nfunc " + name + "(t *testing.T) { t.Fatal(\"fails on the unpatched revision\") }\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main_test.go"), []byte(body.String()), 0o600))
	if len(failing) == 0 {
		// A test file with no test function still exercises the green path.
		require.NoError(t, os.WriteFile(filepath.Join(repo, "main_test.go"), []byte("package main\n"), 0o600))
	}
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "tests")
	return repo
}

// Captured from a real `go test -json ./...` run: a passing test, a skipped
// test, a table whose subtests pass and skip, and a package with no tests at
// all, whose package-level skip event names no test and must record nothing.
func TestParseTestFailuresRecordsPassesAndSkips(t *testing.T) {
	output, err := os.ReadFile(filepath.Join("testdata", "gotest-json-skips-and-subtests.txt"))
	require.NoError(t, err)
	outcome, ok := parseTestFailures(string(output))
	require.True(t, ok)
	require.Equal(t, []string{
		"test.local/fixture::TestKept",
		"test.local/fixture::TestTable",
		"test.local/fixture::TestTable/runs",
	}, outcome.Passed)
	require.Equal(t, []string{
		"test.local/fixture::TestSkipped",
		"test.local/fixture::TestTable/skips",
	}, outcome.Skipped)
	require.Empty(t, outcome.Tests)
	require.Empty(t, outcome.Packages)

	gojq, err := os.ReadFile(filepath.Join("testdata", "gotest-json-failing-suite.txt"))
	require.NoError(t, err)
	outcome, ok = parseTestFailures(string(gojq))
	require.True(t, ok)
	require.Len(t, outcome.Passed, 2, "subtest names are kept whole, escapes decoded")
	require.Equal(t, "github.com/itchyny/gojq::TestCompare/<nil>,<nil>", outcome.Passed[0])
}

// A suite exits zero after a test the baseline passed is skipped or no longer
// runs, so a clean exit alone must not pass the gate.
func TestClassifyTestOutcomeRejectsLostBaselinePasses(t *testing.T) {
	engine := &Engine{state: State{BaselineTestPasses: []string{"pkg::TestA", "pkg::TestB", "pkg::TestC/sub"}}}
	clean := `{"Action":"pass","Package":"pkg","Test":"TestA"}
{"Action":"pass","Package":"pkg","Test":"TestB"}
{"Action":"pass","Package":"pkg","Test":"TestC/sub"}
{"Action":"pass","Package":"pkg","Test":"TestC"}
{"Action":"pass","Package":"pkg"}
`
	reason, passed := engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(clean)}, nil)
	require.True(t, passed, "reason = %q", reason)

	lossy := `{"Action":"pass","Package":"pkg","Test":"TestA"}
{"Action":"skip","Package":"pkg","Test":"TestB"}
{"Action":"pass","Package":"pkg","Test":"TestC"}
{"Action":"pass","Package":"pkg"}
`
	reason, passed = engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(lossy)}, nil)
	require.False(t, passed)
	require.Equal(t, "tests that passed on the unpatched revision did not pass: pkg::TestB (skipped), pkg::TestC/sub (did not run)", reason)

	// A clean exit that printed nothing readable shows none of the passes.
	reason, passed = engine.classifyTestOutcome(toolchain.Result{Stdout: []byte("PASS\n")}, nil)
	require.False(t, passed)
	require.Contains(t, reason, "pkg::TestA (did not run)")

	// A new failure is named as one, not folded into the lost passes.
	failing := `{"Action":"pass","Package":"pkg","Test":"TestA"}
{"Action":"fail","Package":"pkg","Test":"TestB"}
{"Action":"fail","Package":"pkg"}
`
	reason, passed = engine.classifyTestOutcome(toolchain.Result{Stdout: []byte(failing), ExitCode: 1}, errors.New("exit status 1"))
	require.False(t, passed)
	require.Equal(t, "new failing tests: pkg::TestB", reason)

	// With no recorded passes there is nothing to lose, and an unreadable clean
	// run is not held against the candidate.
	empty := &Engine{}
	reason, passed = empty.classifyTestOutcome(toolchain.Result{Stdout: []byte("PASS\n")}, nil)
	require.True(t, passed, "reason = %q", reason)
}

// The end-to-end shape of the defect: a candidate that skips a test the
// unpatched revision passed, or drops it, used to pass the gate on a clean
// exit. Each now fails it with the test named.
func TestBehaviorGateRejectsATestThatStopsPassing(t *testing.T) {
	const kept = "package main\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {}\n\nfunc TestTable(t *testing.T) {\n\tt.Run(\"row\", func(t *testing.T) {})\n}\n"
	repo := repositoryWithTestFile(t, kept)
	manifestPath := writeManifest(t, t.TempDir())
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: manifestPath,
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	require.NoError(t, engine.runBaselineTestStep(context.Background()))
	require.Equal(t, []string{
		"test.local/fixture::TestKept",
		"test.local/fixture::TestTable",
		"test.local/fixture::TestTable/row",
	}, engine.State().BaselineTestPasses)

	var evidence orchestrator.CandidateEvidence
	require.True(t, engine.candidateTestsPassed(context.Background(), repo, &evidence), "evidence: %+v", evidence)

	cases := map[string]struct {
		tests string
		want  string
	}{
		"skipped": {
			strings.Replace(kept, "func TestKept(t *testing.T) {}", "func TestKept(t *testing.T) { t.Skip(\"fast path\") }", 1),
			"test.local/fixture::TestKept (skipped)",
		},
		"deleted": {
			"package main\n\nimport \"testing\"\n\nfunc TestTable(t *testing.T) {\n\tt.Run(\"row\", func(t *testing.T) {})\n}\n",
			"test.local/fixture::TestKept (did not run)",
		},
		"subtest dropped": {
			strings.Replace(kept, "\tt.Run(\"row\", func(t *testing.T) {})\n", "", 1),
			"test.local/fixture::TestTable/row (did not run)",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := repositoryWithTestFile(t, tc.tests)
			evidence := orchestrator.CandidateEvidence{}
			require.False(t, engine.candidateTestsPassed(context.Background(), candidate, &evidence))
			require.Contains(t, evidence.Summary, "upstream test suite failed: tests that passed on the unpatched revision did not pass")
			require.Contains(t, evidence.FailureDetail, tc.want)
			require.False(t, evidence.SafetyChecksPassed)
		})
	}
}

// Campaigns persisted before passes were recorded carry baseline_tests and no
// pass set. The step runs again for them instead of leaving the gate blind to
// lost passes for the rest of the campaign, and records both sets from that
// one run; afterwards it is done and does not run again.
func TestBaselineStepRecordsPassesForACampaignPersistedWithoutThem(t *testing.T) {
	repo := repositoryWithTestFile(t, "package main\n\nimport \"testing\"\n\nfunc TestKept(t *testing.T) {}\n")
	manifestPath := writeManifest(t, t.TempDir())
	engine, err := Create(context.Background(), Options{
		Repository: repo, ManifestPath: manifestPath,
		CampaignDir: filepath.Join(t.TempDir(), "campaign"), TestingUnsafeDisableIsolation: true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	engine.state.CompletedSteps["baseline_tests"] = true
	engine.state.BaselineTestFailures = []string{"test.local/fixture::TestGoneStale"}
	require.NoError(t, engine.runBaselineTestStep(context.Background()))
	require.Equal(t, []string{"test.local/fixture::TestKept"}, engine.state.BaselineTestPasses)
	require.Empty(t, engine.state.BaselineTestFailures, "both sets come from the re-run")
	require.True(t, engine.state.CompletedSteps[baselinePassesStep])

	persisted, err := engine.store.Load()
	require.NoError(t, err)
	require.Equal(t, []string{"test.local/fixture::TestKept"}, persisted.BaselineTestPasses)

	// Once recorded, the step is complete: a suite that would now fail to run
	// is not consulted again.
	require.NoError(t, os.Remove(filepath.Join(repo, "go.mod")))
	require.NoError(t, engine.runBaselineTestStep(context.Background()))
	require.Equal(t, []string{"test.local/fixture::TestKept"}, engine.state.BaselineTestPasses)
}

// repositoryWithTestFile builds a clean repository whose main_test.go holds
// exactly the given source.
func repositoryWithTestFile(t *testing.T, source string) string {
	t.Helper()
	repo := makeRepository(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main_test.go"), []byte(source), 0o600))
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "tests")
	return repo
}
