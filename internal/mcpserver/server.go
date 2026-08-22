// Package mcpserver exposes the bounded harness control plane over MCP.
//
// It intentionally delegates all repository, worktree, measurement, and
// evidence-store behavior to Backend. In particular it does not run a shell,
// make optimization decisions, or orchestrate ADK agents.
package mcpserver

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"example.com/go-agent-optimizer/internal/domain"
	"example.com/go-agent-optimizer/internal/jobs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "go-agent-optimizer"
	serverVersion = "0.1.0"
)

// Backend supplies deterministic harness operations. Implementations own
// sandboxing, Git worktrees, artifact storage, policy evaluation, and the ADK
// workflow hand-off. All potentially long methods are submitted as jobs by
// this package; only workload registration, findings, reports, and resource
// reads are synchronous.
type Backend interface {
	StartCampaign(context.Context, CampaignStartInput) (OperationResult, error)
	GetCampaign(context.Context, CampaignStatusInput) (CampaignStatus, error)
	CancelCampaign(context.Context, CampaignCancelInput) (CampaignStatus, error)
	InspectRepository(context.Context, RepositoryInspectionInput) (OperationResult, error)
	RegisterWorkload(context.Context, WorkloadRegistrationInput) (RegisteredWorkload, error)
	RunWorkload(context.Context, WorkloadRunInput) (OperationResult, error)
	CreateCandidate(context.Context, CandidateCreateInput) (OperationResult, error)
	EvaluateCandidate(context.Context, CandidateEvaluationInput) (OperationResult, error)
	GetFindings(context.Context, FindingsInput) (Findings, error)
	GetReport(context.Context, ReportInput) (Report, error)
	ReadResource(context.Context, string) (ResourceDocument, error)
}

// OperationResult is deliberately small. Detailed results belong in the
// immutable resource addressed by ResultURI.
type OperationResult struct {
	ResultURI   string   `json:"result_uri"`
	Summary     string   `json:"summary,omitempty"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
}

type CampaignStartInput struct {
	Repository  string   `json:"repository" jsonschema:"repository root available to the harness"`
	BuildTarget string   `json:"build_target" jsonschema:"Go package or build target"`
	Command     []string `json:"command,omitempty" jsonschema:"CLI command or subcommand selected for this campaign"`
	ManifestURI string   `json:"manifest_uri,omitempty" jsonschema:"target manifest resource URI"`
}

type CampaignStatusInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"campaign ID"`
}

// CampaignStatus is intentionally a compact control-plane snapshot. The
// detailed campaign state is available through its ResourceURI.
type CampaignStatus struct {
	CampaignID  string `json:"campaign_id"`
	Status      string `json:"status"`
	ResourceURI string `json:"resource_uri,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type CampaignCancelInput struct {
	CampaignID string `json:"campaign_id"`
	Reason     string `json:"reason,omitempty" jsonschema:"operator-visible cancellation reason"`
}

type RepositoryInspectionInput struct {
	CampaignID  string `json:"campaign_id,omitempty" jsonschema:"optional existing campaign ID"`
	Repository  string `json:"repository" jsonschema:"repository root available to the harness"`
	BuildTarget string `json:"build_target" jsonschema:"Go package or build target"`
}

type WorkloadRegistrationInput struct {
	CampaignID string          `json:"campaign_id" jsonschema:"campaign that owns the workload"`
	Workload   domain.Workload `json:"workload" jsonschema:"replayable workload definition"`
}

type RegisteredWorkload struct {
	CampaignID  string          `json:"campaign_id"`
	Workload    domain.Workload `json:"workload"`
	ResourceURI string          `json:"resource_uri,omitempty"`
}

type WorkloadRunInput struct {
	CampaignID string         `json:"campaign_id"`
	BuildID    string         `json:"build_id"`
	WorkloadID string         `json:"workload_id"`
	Mode       domain.RunMode `json:"mode"`
	Limits     RunLimits      `json:"limits,omitempty"`
}

// RunLimits are declarative limits interpreted by the backend. They are not
// command-line arguments and cannot be used as a generic shell escape hatch.
type RunLimits struct {
	TimeoutMS      int64 `json:"timeout_ms,omitempty"`
	MemoryBytes    int64 `json:"memory_bytes,omitempty"`
	CPUMillis      int64 `json:"cpu_millis,omitempty"`
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
}

type CandidateCreateInput struct {
	CampaignID   string `json:"campaign_id"`
	BaseRevision string `json:"base_revision"`
	Hypothesis   string `json:"hypothesis"`
	PatchPath    string `json:"patch_path" jsonschema:"validated patch artifact path managed by the harness"`
}

type CandidateEvaluationInput struct {
	CampaignID        string `json:"campaign_id"`
	CandidateID       string `json:"candidate_id"`
	ValidationProfile string `json:"validation_profile,omitempty" jsonschema:"named manifest validation profile"`
	HoldoutBudgetMS   int64  `json:"holdout_budget_ms,omitempty"`
}

type FindingsInput struct {
	CampaignID string `json:"campaign_id"`
	Limit      int    `json:"limit,omitempty"`
}

type Findings struct {
	CampaignID  string `json:"campaign_id"`
	ResourceURI string `json:"resource_uri"`
	Summary     string `json:"summary"`
}

type ReportInput struct {
	CampaignID string `json:"campaign_id"`
}

type Report struct {
	CampaignID  string `json:"campaign_id"`
	ResourceURI string `json:"resource_uri"`
	Summary     string `json:"summary"`
}

// ResourceDocument is returned only for approved harness URI families. Raw
// large profiling data stays behind explicit artifact resources owned by the
// backend.
type ResourceDocument struct {
	MIMEType string
	Text     string
	Blob     []byte
}

// Server wraps the SDK server to keep its injected dependencies available to
// embedding code and tests.
type Server struct {
	MCP     *mcp.Server
	backend Backend
	jobs    jobs.Manager
}

// New builds the complete MCP surface. The supplied backend must enforce the
// target manifest's filesystem, network, and process permissions.
func New(backend Backend, manager jobs.Manager) (*Server, error) {
	if backend == nil {
		return nil, errors.New("MCP backend is required")
	}
	if manager == nil {
		return nil, errors.New("job manager is required")
	}
	s := &Server{backend: backend, jobs: manager}
	s.MCP = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, &mcp.ServerOptions{
		Instructions: "Control plane for bounded Go CLI optimization campaigns. Long operations return a job ID; inspect results through resources. No shell access is exposed.",
	})
	s.registerTools()
	s.registerResources()
	return s, nil
}

func (s *Server) registerTools() {
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "campaign_start", Description: "Start a bounded optimization campaign asynchronously."}, s.startCampaign)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "campaign_status", Description: "Get a compact status snapshot for a campaign."}, s.campaignStatus)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "campaign_cancel", Description: "Request cooperative cancellation of a campaign."}, s.cancelCampaign)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "job_status", Description: "Get a deterministic snapshot of an asynchronous job."}, s.jobStatus)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "job_cancel", Description: "Cancel a queued or running asynchronous job."}, s.cancelJob)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "repository_inspect", Description: "Inspect a Go repository asynchronously; returns a job ID."}, s.inspectRepository)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "workload_register", Description: "Register a replayable workload in a campaign."}, s.registerWorkload)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "workload_run", Description: "Run a registered workload asynchronously; returns a job ID."}, s.runWorkload)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "findings_get", Description: "Read compact measured hotspot findings for a campaign."}, s.getFindings)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "candidate_create", Description: "Create an isolated candidate from a validated patch asynchronously."}, s.createCandidate)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "candidate_evaluate", Description: "Run deterministic validation and policy evaluation asynchronously."}, s.evaluateCandidate)
	mcp.AddTool(s.MCP, &mcp.Tool{Name: "report_get", Description: "Read the compact campaign report."}, s.getReport)
}

func (s *Server) registerResources() {
	resources := []*mcp.ResourceTemplate{
		{Name: "repository-inventory", URITemplate: "repo://{revision}/inventory", MIMEType: "application/json"},
		{Name: "repository-coverage", URITemplate: "repo://{revision}/coverage", MIMEType: "application/json"},
		{Name: "campaign-manifest", URITemplate: "campaign://{campaign_id}/manifest", MIMEType: "application/json"},
		{Name: "campaign-hot-paths", URITemplate: "campaign://{campaign_id}/hot-paths", MIMEType: "application/json"},
		{Name: "campaign-report", URITemplate: "campaign://{campaign_id}/report", MIMEType: "application/json"},
		{Name: "run-summary", URITemplate: "run://{run_id}/summary", MIMEType: "application/json"},
		{Name: "candidate-diff", URITemplate: "candidate://{candidate_id}/diff", MIMEType: "text/x-diff"},
		{Name: "candidate-comparison", URITemplate: "candidate://{candidate_id}/comparison", MIMEType: "application/json"},
		{Name: "artifact-metadata", URITemplate: "artifact://{artifact_id}/metadata", MIMEType: "application/json"},
	}
	for _, resource := range resources {
		s.MCP.AddResourceTemplate(resource, s.readResource)
	}
}

func (s *Server) startCampaign(_ context.Context, _ *mcp.CallToolRequest, input CampaignStartInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.submit("campaign-start", func(ctx context.Context) (jobs.Result, error) {
		result, err := s.backend.StartCampaign(ctx, input)
		return jobs.Result{ResultURI: result.ResultURI}, err
	})
	return nil, job, err
}

func (s *Server) campaignStatus(ctx context.Context, _ *mcp.CallToolRequest, input CampaignStatusInput) (*mcp.CallToolResult, CampaignStatus, error) {
	status, err := s.backend.GetCampaign(ctx, input)
	return nil, status, err
}

func (s *Server) cancelCampaign(ctx context.Context, _ *mcp.CallToolRequest, input CampaignCancelInput) (*mcp.CallToolResult, CampaignStatus, error) {
	status, err := s.backend.CancelCampaign(ctx, input)
	return nil, status, err
}

func (s *Server) jobStatus(_ context.Context, _ *mcp.CallToolRequest, input jobInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.jobs.Get(input.JobID)
	return nil, job, err
}

func (s *Server) cancelJob(_ context.Context, _ *mcp.CallToolRequest, input jobInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.jobs.Cancel(input.JobID)
	return nil, job, err
}

type jobInput struct {
	JobID string `json:"job_id" jsonschema:"asynchronous job ID"`
}

func (s *Server) inspectRepository(_ context.Context, _ *mcp.CallToolRequest, input RepositoryInspectionInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.submit("repository-inspect", func(ctx context.Context) (jobs.Result, error) {
		result, err := s.backend.InspectRepository(ctx, input)
		return jobs.Result{ResultURI: result.ResultURI}, err
	})
	return nil, job, err
}

func (s *Server) registerWorkload(ctx context.Context, _ *mcp.CallToolRequest, input WorkloadRegistrationInput) (*mcp.CallToolResult, RegisteredWorkload, error) {
	result, err := s.backend.RegisterWorkload(ctx, input)
	return nil, result, err
}

func (s *Server) runWorkload(_ context.Context, _ *mcp.CallToolRequest, input WorkloadRunInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.submit("workload-run", func(ctx context.Context) (jobs.Result, error) {
		result, err := s.backend.RunWorkload(ctx, input)
		return jobs.Result{ResultURI: result.ResultURI}, err
	})
	return nil, job, err
}

func (s *Server) getFindings(ctx context.Context, _ *mcp.CallToolRequest, input FindingsInput) (*mcp.CallToolResult, Findings, error) {
	result, err := s.backend.GetFindings(ctx, input)
	return nil, result, err
}

func (s *Server) createCandidate(_ context.Context, _ *mcp.CallToolRequest, input CandidateCreateInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.submit("candidate-create", func(ctx context.Context) (jobs.Result, error) {
		result, err := s.backend.CreateCandidate(ctx, input)
		return jobs.Result{ResultURI: result.ResultURI}, err
	})
	return nil, job, err
}

func (s *Server) evaluateCandidate(_ context.Context, _ *mcp.CallToolRequest, input CandidateEvaluationInput) (*mcp.CallToolResult, domain.Job, error) {
	job, err := s.submit("candidate-evaluate", func(ctx context.Context) (jobs.Result, error) {
		result, err := s.backend.EvaluateCandidate(ctx, input)
		return jobs.Result{ResultURI: result.ResultURI}, err
	})
	return nil, job, err
}

func (s *Server) getReport(ctx context.Context, _ *mcp.CallToolRequest, input ReportInput) (*mcp.CallToolResult, Report, error) {
	result, err := s.backend.GetReport(ctx, input)
	return nil, result, err
}

func (s *Server) submit(kind string, task jobs.Task) (domain.Job, error) {
	return s.jobs.Submit(kind, task)
}

func (s *Server) readResource(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if !approvedResourceURI(request.Params.URI) {
		return nil, mcp.ResourceNotFoundError(request.Params.URI)
	}
	document, err := s.backend.ReadResource(ctx, request.Params.URI)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI: request.Params.URI, MIMEType: document.MIMEType, Text: document.Text, Blob: document.Blob,
	}}}, nil
}

func approvedResourceURI(raw string) bool {
	uri, err := url.Parse(raw)
	if err != nil || uri.Host == "" || uri.RawQuery != "" || uri.Fragment != "" {
		return false
	}
	segments := strings.Split(strings.Trim(uri.Path, "/"), "/")
	if len(segments) != 1 || segments[0] == "" {
		return false
	}
	switch uri.Scheme {
	case "repo":
		return segments[0] == "inventory" || segments[0] == "coverage"
	case "campaign":
		return segments[0] == "manifest" || segments[0] == "hot-paths" || segments[0] == "report"
	case "run":
		return segments[0] == "summary"
	case "candidate":
		return segments[0] == "diff" || segments[0] == "comparison"
	case "artifact":
		return segments[0] == "metadata"
	default:
		return false
	}
}
