package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"example.com/gotorque/internal/domain"
)

// benchstatMaxOutputBytes bounds the raw benchstat output kept in the
// candidate record so one noisy run cannot bloat persisted state.
const benchstatMaxOutputBytes = 2048

// benchstatAlpha is the significance threshold applied to parsed p-values.
// It mirrors the ~p<0.05 conservatism of the internal Welch t-test fallback.
const benchstatAlpha = 0.05

// benchstatSummary carries what could conservatively be extracted from one
// benchstat run. Benchstat output format varies between the legacy golang.org/x/perf
// releases and the newer tables, so parsing extracts only what it recognizes.
type benchstatSummary struct {
	HasPValue       bool    // at least one "p=" token was recognized
	PValue          float64 // most conservative (largest) recognized p-value
	DeltaPercent    float64 // last recognized percentage delta token
	InformativeOnly bool    // delta seen but no p-value: informational, never support
}

var (
	benchstatPRe      = regexp.MustCompile(`p=([0-9]*\.?[0-9]+(?:[eE][+-]?[0-9]+)?)`)
	benchstatPctRe    = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?%`)
	benchstatStdErrRe = regexp.MustCompile(`±\s*[-+]?\d+(?:\.\d+)?%`)
)

// parseBenchstatOutput extracts comparison data from benchstat output. It
// returns ok=false when neither a p-value nor a delta percentage can be
// recognized, which sends the caller back to the internal t-test. When only
// delta percentages are present (legacy v0.x format), the summary is marked
// informative-only and must never grant statistical support by itself.
func parseBenchstatOutput(output string) (benchstatSummary, bool) {
	var summary benchstatSummary
	sawDelta := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if skipBenchstatMetaLine(line) {
			continue
		}
		parseBenchstatPValues(line, &summary)
		if parseBenchstatDelta(line, &summary) {
			sawDelta = true
		}
	}
	if !summary.HasPValue && !sawDelta {
		return benchstatSummary{}, false
	}
	summary.InformativeOnly = !summary.HasPValue
	return summary, true
}

func skipBenchstatMetaLine(line string) bool {
	return line == "" || strings.HasPrefix(line, "goos:") || strings.HasPrefix(line, "goarch:") ||
		strings.HasPrefix(line, "pkg:") || strings.HasPrefix(line, "cpu:")
}

func parseBenchstatPValues(line string, summary *benchstatSummary) {
	for _, match := range benchstatPRe.FindAllStringSubmatch(line, -1) {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil || value < 0 || value > 1 {
			continue
		}
		if !summary.HasPValue || value > summary.PValue {
			summary.PValue = value
		}
		summary.HasPValue = true
	}
}

func parseBenchstatDelta(line string, summary *benchstatSummary) bool {
	// Percentage tokens: skip ±-prefixed sample-noise markers so the
	// "vs base" delta column is what gets recorded.
	cleaned := benchstatStdErrRe.ReplaceAllString(line, "")
	matches := benchstatPctRe.FindAllString(cleaned, -1)
	if len(matches) == 0 {
		return false
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(matches[len(matches)-1], "%"), 64)
	if err != nil {
		return false
	}
	summary.DeltaPercent = value
	return true
}

// supported reports whether this summary alone justifies statistical
// support under policy. Only a parseable p-value below alpha counts;
// informative-only summaries defer to the caller's t-test result.
func (s benchstatSummary) supported() bool {
	return s.HasPValue && s.PValue < benchstatAlpha
}

// compareWallTimeMetric builds the wall_time_ns comparison for one workload.
// The Welch t-test result always exists; when the installed benchstat binary
// can compare the raw samples and yields a parseable p-value, that p-value
// refines the statistical-support signal. Any other outcome — binary absent,
// invocation failure, unparseable output — leaves the t-test path unchanged.
// The second return value is trimmed raw benchstat output for reporting, or
// empty when benchstat did not contribute.
// compareWallTimeMetric builds the wall_time_ns comparison for one workload,
// which is the manifest's seed id so the comparison, the benchstat sample files
// and the verdict all speak in the same name the operator writes in the
// target manifest. Pass an empty workload for the pooled reading.
func (e *Engine) compareWallTimeMetric(ctx context.Context, workload string, baselineRuns, candidateRuns []domain.RunResult) ([]domain.MetricComparison, string) {
	comparisons := compareMetric(workload, "wall_time_ns", "ns", baselineRuns, candidateRuns, wallTime)
	baseVals := collectMetric(baselineRuns, wallTime)
	candVals := collectMetric(candidateRuns, wallTime)
	output, summary, ok := e.runBenchstat(ctx, workload, baseVals, candVals)
	if !ok || len(comparisons) == 0 {
		return comparisons, ""
	}
	c := &comparisons[0]
	if summary.supported() {
		c.StatisticallyFit = true
	} else if !summary.InformativeOnly {
		// A parseable but insignificant p-value withdraws support that the
		// coarse t-test heuristic may have granted on these few samples.
		c.StatisticallyFit = false
	}
	return comparisons, output
}

// runBenchstat writes the wall-time sample sets to per-workload files in the
// campaign directory, runs benchstat over them, and parses its output. It is
// best-effort: any failure returns ok=false so callers keep the t-test path.
func (e *Engine) runBenchstat(ctx context.Context, workloadID string, baseVals, candVals []float64) (string, benchstatSummary, bool) {
	if !e.benchstatReady(baseVals, candVals) {
		return "", benchstatSummary{}, false
	}
	basePath, candPath, ok := writeBenchstatSamples(filepath.Join(e.dir, "benchstat"), workloadID, baseVals, candVals)
	if !ok {
		return "", benchstatSummary{}, false
	}
	result, err := e.toolchain.Benchstat(ctx, basePath, candPath)
	if err != nil || result.ExitCode != 0 {
		return "", benchstatSummary{}, false
	}
	output := truncateHead(strings.TrimSpace(string(result.Stdout)), benchstatMaxOutputBytes)
	summary, ok := parseBenchstatOutput(output)
	if !ok {
		return "", benchstatSummary{}, false
	}
	return output, summary, true
}

func (e *Engine) benchstatReady(baseVals, candVals []float64) bool {
	return e.toolchain != nil && e.toolchain.HasBenchstat() && len(baseVals) > 0 && len(candVals) > 0
}

func writeBenchstatSamples(dir, workloadID string, baseVals, candVals []float64) (basePath, candPath string, ok bool) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false
	}
	basePath = filepath.Join(dir, workloadID+".base.txt")
	candPath = filepath.Join(dir, workloadID+".cand.txt")
	if err := writeSampleFile(basePath, baseVals); err != nil {
		return "", "", false
	}
	if err := writeSampleFile(candPath, candVals); err != nil {
		return "", "", false
	}
	return basePath, candPath, true
}

// writeSampleFile stores one duration-per-line sample file ("1234ns"),
// the plain-text format benchstat parses natively.
// writeSampleFile renders one measurement per line in the Go benchmark format
// benchstat parses.
//
// The previous shape — one bare value per line — is not a benchmark report at
// all: benchstat skips every line, exits 0, and prints nothing, so this lane
// had never once contributed a p-value and every verdict fell back to a Welch
// t-test over means. On gojq two outlier samples (19.7ms and 16.7ms against a
// 9.9ms median) were enough to report a 6.8% pooled improvement that the
// distribution does not contain; benchstat's rank-based comparison answers
// ~ (p=0.648 n=7) for the same samples, which is the honest reading.
//
// Iterations are 1 because a sample is one whole run of the target binary
// rather than a batch inside one process: ns/op is then the run's wall time.
func writeSampleFile(path string, values []float64) error {
	var b strings.Builder
	for i, v := range values {
		fmt.Fprintf(&b, "BenchmarkWallTime-1\t%d\t%.0f ns/op\n", i+1, v)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func truncateHead(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
