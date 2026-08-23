package domain

import "time"

type RunMode string

const (
	RunModeDiscovery   RunMode = "discovery"
	RunModeDiagnosis   RunMode = "diagnosis"
	RunModeMeasurement RunMode = "measurement"
	RunModeValidation  RunMode = "validation"
)

type WorkloadTier string

const (
	TierRepresentative WorkloadTier = "representative"
	TierPlausible      WorkloadTier = "plausible"
	TierStress         WorkloadTier = "stress"
)

type OptimizationPolicy string

const (
	PolicyIdiomatic   OptimizationPolicy = "idiomatic"
	PolicySpecialized OptimizationPolicy = "specialized"
	PolicyNative      OptimizationPolicy = "native"
)

type Decision string

const (
	DecisionAccepted     Decision = "accepted"
	DecisionRejected     Decision = "rejected"
	DecisionInconclusive Decision = "inconclusive"
)

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

type Command struct {
	Path       string            `json:"path"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
}

type Workload struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Tier        WorkloadTier  `json:"tier"`
	Weight      float64       `json:"weight"`
	Command     Command       `json:"command"`
	StdinPath   string        `json:"stdin_path,omitempty"`
	Timeout     time.Duration `json:"timeout"`
	Provenance  string        `json:"provenance"`
	Description string        `json:"description,omitempty"`
}

type Metric struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
}

type RunResult struct {
	ID           string            `json:"id"`
	BuildID      string            `json:"build_id"`
	WorkloadID   string            `json:"workload_id"`
	Mode         RunMode           `json:"mode"`
	StartedAt    time.Time         `json:"started_at"`
	Duration     time.Duration     `json:"duration"`
	ExitCode     int               `json:"exit_code"`
	StdoutDigest string            `json:"stdout_digest"`
	SortedLinesDigest string       `json:"sorted_lines_digest,omitempty"`
	StderrDigest string            `json:"stderr_digest"`
	Metrics      []Metric          `json:"metrics,omitempty"`
	Artifacts    map[string]string `json:"artifacts,omitempty"`
	Error        string            `json:"error,omitempty"`
}

type Candidate struct {
	ID           string    `json:"id"`
	BaseRevision string    `json:"base_revision"`
	Commit       string    `json:"commit,omitempty"`
	Hypothesis   string    `json:"hypothesis"`
	PatchPath    string    `json:"patch_path"`
	CreatedAt    time.Time `json:"created_at"`
}

type MetricComparison struct {
	Name             string  `json:"name"`
	Unit             string  `json:"unit"`
	Baseline         float64 `json:"baseline"`
	Candidate        float64 `json:"candidate"`
	DeltaPercent     float64 `json:"delta_percent"`
	Confidence       float64 `json:"confidence,omitempty"`
	StatisticallyFit bool    `json:"statistically_supported"`
}

type Evaluation struct {
	CandidateID     string             `json:"candidate_id"`
	Decision        Decision           `json:"decision"`
	BehaviorMatches bool               `json:"behavior_matches"`
	Comparisons     []MetricComparison `json:"comparisons"`
	Reasons         []string           `json:"reasons"`
}

type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    JobStatus `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ResultURI string    `json:"result_uri,omitempty"`
	Error     string    `json:"error,omitempty"`
}
