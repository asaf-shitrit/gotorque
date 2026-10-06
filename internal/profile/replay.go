package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// transcriptFile is a Transcript on disk: the report is stored beside the
// metadata as its own file, so a recorded sampler report stays readable text.
type transcriptFile struct {
	Transcript
	ReportFile string `json:"report_file,omitempty"`
}

// LoadTranscript reads a recorded transcript: a JSON file holding the
// Transcript's fields, whose `report_file` (relative to the JSON file) supplies
// the Report. Tests replay transcripts recorded from real sampler runs.
func LoadTranscript(path string) (Transcript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Transcript{}, err
	}
	var file transcriptFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Transcript{}, fmt.Errorf("decode transcript %s: %w", path, err)
	}
	if file.ReportFile != "" {
		report, err := os.ReadFile(filepath.Join(filepath.Dir(path), file.ReportFile))
		if err != nil {
			return Transcript{}, fmt.Errorf("read transcript report: %w", err)
		}
		file.Report = string(report)
	}
	return file.Transcript, nil
}

// SamplerFunc adapts a function to a Sampler, for tests that script what a
// sampler returns.
type SamplerFunc func(ctx context.Context, req SampleTarget) (Transcript, error)

func (f SamplerFunc) Sample(ctx context.Context, req SampleTarget) (Transcript, error) {
	return f(ctx, req)
}
