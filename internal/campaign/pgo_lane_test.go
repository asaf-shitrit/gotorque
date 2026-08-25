package campaign

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/toolchain"
)

// pgoLaneTestEngine builds an Engine sufficient for runPgoLane's skip paths
// and for record plumbing tests: a real bbolt-backed store in a temp campaign
// directory, no live runner (never reached on skip paths).
func pgoLaneTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, DatabaseName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Engine{
		dir:       dir,
		store:     store,
		progress:  io.Discard,
		now:       func() time.Time { return time.Now().UTC() },
		toolchain: toolchain.New(toolchain.Options{}),
	}
}

func TestPgoLaneSkipsWithoutPprofProfile(t *testing.T) {
	e := pgoLaneTestEngine(t)
	evidence := orchestrator.CandidateEvidence{}

	e.runPgoLane(context.Background(), &evidence, t.TempDir(), "cand1")

	if len(evidence.PgoComparisons) != 0 {
		t.Fatalf("expected no comparisons, got %+v", evidence.PgoComparisons)
	}
	if !strings.Contains(evidence.PgoNote, "PGO lane skipped") || !strings.Contains(evidence.PgoNote, "pprof") {
		t.Fatalf("note should explain the pprof-format requirement, got %q", evidence.PgoNote)
	}
	events, err := e.store.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "pgo_lane_skipped" && strings.Contains(ev.Message, "pprof") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a pgo_lane_skipped event, got %+v", events)
	}
}

func TestPgoLaneSkipsWhenProfileFileMissingOnDisk(t *testing.T) {
	e := pgoLaneTestEngine(t)
	e.state.PGOProfilePath = filepath.Join(e.dir, "profiles", "bench-cpu.pb.gz")
	evidence := orchestrator.CandidateEvidence{}

	e.runPgoLane(context.Background(), &evidence, t.TempDir(), "cand2")

	if len(evidence.PgoComparisons) != 0 {
		t.Fatalf("expected no comparisons, got %+v", evidence.PgoComparisons)
	}
	if !strings.Contains(evidence.PgoNote, "missing or empty") {
		t.Fatalf("note should mention the missing profile file, got %q", evidence.PgoNote)
	}
}

// A zero-length file is not a usable pprof profile either.
func TestPgoLaneSkipsWhenProfileFileEmpty(t *testing.T) {
	e := pgoLaneTestEngine(t)
	path := filepath.Join(e.dir, "bench-cpu.pb.gz")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.state.PGOProfilePath = path
	evidence := orchestrator.CandidateEvidence{}

	e.runPgoLane(context.Background(), &evidence, t.TempDir(), "cand3")

	if !strings.Contains(evidence.PgoNote, "missing or empty") {
		t.Fatalf("note should mention the empty profile file, got %q", evidence.PgoNote)
	}
}

func TestPgoEvidenceFlowsIntoCandidateRecord(t *testing.T) {
	e := pgoLaneTestEngine(t)
	services := adkServices{engine: e}
	pgo := []domain.MetricComparison{{
		Name: "wl/wall_time_ns", Unit: "ns",
		Baseline: 100, Candidate: 90, DeltaPercent: -10, StatisticallyFit: true,
	}}
	input := orchestrator.PolicyInput{
		Evidence: orchestrator.CandidateEvidence{
			Candidate:              domain.Candidate{ID: "cand4"},
			BehaviorMatches:        true,
			SafetyChecksPassed:     true,
			RepresentativeEvidence: true,
			PgoComparisons:         pgo,
			PgoNote:                "informational PGO comparison; never changes accept/reject decisions",
		},
	}
	if _, err := services.Evaluate(context.Background(), input); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(e.state.CandidateRecords) != 1 {
		t.Fatalf("records = %d, want 1", len(e.state.CandidateRecords))
	}
	record := e.state.CandidateRecords[0]
	if len(record.PgoComparisons) != 1 || record.PgoComparisons[0].Name != "wl/wall_time_ns" {
		t.Fatalf("PgoComparisons did not flow into the record: %+v", record.PgoComparisons)
	}
	if record.PgoNote != input.Evidence.PgoNote {
		t.Fatalf("PgoNote = %q, want %q", record.PgoNote, input.Evidence.PgoNote)
	}
	// The PGO lane must not influence the policy verdict inputs.
	if record.Decision == "" {
		t.Fatal("record should still carry a normal policy decision")
	}
}

func TestReportRendersPgoLaneSection(t *testing.T) {
	state := State{
		ID: "c1",
		CandidateRecords: []CandidateRecord{{
			Attempt: 1, CandidateID: "abc", Decision: domain.DecisionAccepted,
			PgoComparisons: []domain.MetricComparison{{
				Name: "wl/wall_time_ns", Unit: "ns",
				Baseline: 100, Candidate: 90, DeltaPercent: -10, StatisticallyFit: true,
			}},
			PgoNote: "informational PGO comparison over 1 representative workload(s)",
		}},
	}
	rendered := RenderMarkdown(state)
	for _, want := range []string{
		"PGO lane",
		"never changes accept/reject decisions",
		"| `wl/wall_time_ns` | 100 | 90 | -10.00% | yes |",
		"informational PGO comparison over 1 representative workload(s)",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("report missing %q:\n%s", want, rendered)
		}
	}
}

func TestReportRendersSkippedPgoLaneNote(t *testing.T) {
	state := State{
		CandidateRecords: []CandidateRecord{{
			Attempt: 1, CandidateID: "abc", Decision: domain.DecisionAccepted,
			PgoNote: "PGO lane skipped: no pprof-format CPU profile was collected during discovery",
		}},
	}
	rendered := RenderMarkdown(state)
	if !strings.Contains(rendered, "PGO lane") || !strings.Contains(rendered, "PGO lane skipped") {
		t.Fatalf("skipped lane note should render under the PGO lane heading:\n%s", rendered)
	}
}

func TestReportOmitsEmptyPgoLaneSection(t *testing.T) {
	state := State{
		CandidateRecords: []CandidateRecord{{Attempt: 1, CandidateID: "abc"}},
	}
	if rendered := RenderMarkdown(state); strings.Contains(rendered, "PGO lane") {
		t.Fatalf("PGO lane section rendered without data:\n%s", rendered)
	}
}
