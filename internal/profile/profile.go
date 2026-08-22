// Package profile turns authoritative Go pprof and trace output into compact
// structured evidence for agents. Raw artifacts remain available separately.
package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"example.com/go-agent-optimizer/internal/runner"
	"example.com/go-agent-optimizer/internal/toolchain"
)

type Function struct {
	Flat              string  `json:"flat"`
	FlatPercent       float64 `json:"flat_percent"`
	Cumulative        string  `json:"cumulative"`
	CumulativePercent float64 `json:"cumulative_percent"`
	Name              string  `json:"name"`
}

type Summary struct {
	SourcePath string     `json:"source_path"`
	Total      string     `json:"total,omitempty"`
	Functions  []Function `json:"functions"`
	RawReport  string     `json:"raw_report_artifact"`
}

type TraceSummary struct {
	Kind       string  `json:"kind"`
	Profile    Summary `json:"profile"`
	RawProfile string  `json:"raw_profile_artifact"`
}

type Collector struct {
	Toolchain *toolchain.Toolchain
	Artifacts *runner.ArtifactStore
	TempRoot  string
}

func (c Collector) SummarizePprof(ctx context.Context, profilePath string, nodeCount int) (Summary, error) {
	if c.Toolchain == nil || c.Artifacts == nil {
		return Summary{}, errors.New("toolchain and artifact store are required")
	}
	if !filepath.IsAbs(profilePath) {
		return Summary{}, errors.New("profile path must be absolute")
	}
	result, err := c.Toolchain.PprofTop(ctx, profilePath, nodeCount)
	if err != nil {
		return Summary{}, err
	}
	_, reportPath, err := c.Artifacts.Put("pprof-top.txt", result.Stdout)
	if err != nil {
		return Summary{}, err
	}
	summary := parseTop(string(result.Stdout))
	summary.SourcePath = profilePath
	summary.RawReport = reportPath
	return summary, nil
}

// SummarizeTrace uses go tool trace to extract an authoritative pprof stream,
// stores it, then asks go tool pprof for the same normalized top report used
// by ordinary CPU/heap profiles.
func (c Collector) SummarizeTrace(ctx context.Context, tracePath, kind string, nodeCount int) (TraceSummary, error) {
	if c.Toolchain == nil || c.Artifacts == nil {
		return TraceSummary{}, errors.New("toolchain and artifact store are required")
	}
	if !filepath.IsAbs(tracePath) {
		return TraceSummary{}, errors.New("trace path must be absolute")
	}
	root := c.TempRoot
	if root == "" {
		root = os.TempDir()
	}
	if !filepath.IsAbs(root) {
		return TraceSummary{}, errors.New("temporary root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return TraceSummary{}, err
	}
	converted, err := c.Toolchain.TracePprof(ctx, tracePath, kind)
	if err != nil {
		return TraceSummary{}, err
	}
	_, rawPath, err := c.Artifacts.Put("trace-"+kind+".pb.gz", converted.Stdout)
	if err != nil {
		return TraceSummary{}, err
	}
	temp, err := os.CreateTemp(root, "trace-profile-*.pb.gz")
	if err != nil {
		return TraceSummary{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(converted.Stdout); err != nil {
		_ = temp.Close()
		return TraceSummary{}, err
	}
	if err := temp.Close(); err != nil {
		return TraceSummary{}, err
	}
	summary, err := c.SummarizePprof(ctx, tempPath, nodeCount)
	if err != nil {
		return TraceSummary{}, fmt.Errorf("summarize %s trace: %w", kind, err)
	}
	return TraceSummary{Kind: kind, Profile: summary, RawProfile: rawPath}, nil
}

var totalLine = regexp.MustCompile(`(?m)^Showing nodes accounting for .+? of (.+?)(?:,|$)`)

func parseTop(report string) Summary {
	summary := Summary{}
	if match := totalLine.FindStringSubmatch(report); len(match) == 2 {
		summary.Total = strings.TrimSpace(match[1])
	}
	lines := strings.Split(report, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		// pprof's stable text table is: flat flat% sum% cum cum% name.
		if len(fields) < 6 || !strings.HasSuffix(fields[1], "%") || !strings.HasSuffix(fields[2], "%") || !strings.HasSuffix(fields[4], "%") {
			continue
		}
		flatPercent, okFlat := parsePercent(fields[1])
		cumulativePercent, okCum := parsePercent(fields[4])
		if !okFlat || !okCum {
			continue
		}
		summary.Functions = append(summary.Functions, Function{
			Flat: fields[0], FlatPercent: flatPercent, Cumulative: fields[3], CumulativePercent: cumulativePercent,
			Name: strings.Join(fields[5:], " "),
		})
	}
	return summary
}

func parsePercent(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
	return parsed, err == nil
}
