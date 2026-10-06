package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// Replay is the scripted adapter: it returns the transcripts it was given, one
// per call in order, and keeps returning the last once they run out. It records
// every request, so a test can assert what discovery asked the sampler to run.
type Replay struct {
	mu          sync.Mutex
	transcripts []Transcript
	requests    []SampleTarget
}

// NewReplay scripts a sampler that replays the transcripts in order.
func NewReplay(transcripts ...Transcript) *Replay {
	return &Replay{transcripts: transcripts}
}

func (r *Replay) Sample(_ context.Context, req SampleTarget) (Transcript, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if len(r.transcripts) == 0 {
		return Transcript{}, errors.New("replay sampler has no transcripts")
	}
	return r.transcripts[min(len(r.requests)-1, len(r.transcripts)-1)], nil
}

// Requests returns the requests received so far, oldest first.
func (r *Replay) Requests() []SampleTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SampleTarget(nil), r.requests...)
}
