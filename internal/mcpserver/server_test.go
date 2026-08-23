package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/jobs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestServerExposesNarrowControlPlaneAndRunsAsyncOperation(t *testing.T) {
	backend := &stubBackend{}
	manager := jobs.NewMemoryManager(jobs.Options{})
	server, err := New(backend, manager)
	require.NoError(t, err)

	client := connect(t, server.MCP)
	defer client.Close()

	var names []string
	for tool, err := range client.Tools(context.Background(), nil) {
		require.NoError(t, err)
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	require.Equal(t, []string{
		"campaign_cancel", "campaign_start", "campaign_status", "candidate_create", "candidate_evaluate", "findings_get", "job_cancel", "job_status", "report_get", "repository_inspect", "workload_register", "workload_run",
	}, names)

	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "repository_inspect",
		Arguments: RepositoryInspectionInput{Repository: "/repo", BuildTarget: "./cmd/demo"},
	})
	require.NoError(t, err)
	job := decodeTool[domain.Job](t, result.StructuredContent)
	require.Equal(t, "repository-inspect", job.Kind)

	completed := waitForJob(t, manager, job.ID)
	require.Equal(t, domain.JobSucceeded, completed.Status)
	require.Equal(t, "repo://revision/inventory", completed.ResultURI)
	require.Equal(t, 1, backend.inspections)

	cancelled, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "campaign_cancel", Arguments: CampaignCancelInput{CampaignID: "campaign-1", Reason: "operator request"},
	})
	require.NoError(t, err)
	require.Equal(t, "cancelled", decodeTool[CampaignStatus](t, cancelled.StructuredContent).Status)
}

func TestServerRegistersWorkloadAndReadsOnlyApprovedResources(t *testing.T) {
	backend := &stubBackend{}
	server, err := New(backend, jobs.NewMemoryManager(jobs.Options{}))
	require.NoError(t, err)
	client := connect(t, server.MCP)
	defer client.Close()

	workload := domain.Workload{ID: "w1", Name: "example", Tier: domain.TierRepresentative, Weight: 1, Provenance: "manifest"}
	called, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "workload_register",
		Arguments: WorkloadRegistrationInput{CampaignID: "campaign-1", Workload: workload},
	})
	require.NoError(t, err)
	registered := decodeTool[RegisteredWorkload](t, called.StructuredContent)
	require.Equal(t, workload.ID, registered.Workload.ID)
	require.Equal(t, "campaign-1", backend.registered.CampaignID)

	resource, err := client.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "campaign://campaign-1/hot-paths"})
	require.NoError(t, err)
	require.Equal(t, `{"hotspots":[]}`, resource.Contents[0].Text)
	require.Equal(t, "campaign://campaign-1/hot-paths", backend.lastResource)

	_, err = client.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "file:///etc/passwd"})
	require.Error(t, err)
}

func TestApprovedResourceURI(t *testing.T) {
	for _, raw := range []string{
		"repo://rev/inventory", "repo://rev/coverage", "campaign://c/manifest", "campaign://c/hot-paths", "campaign://c/report",
		"run://r/summary", "candidate://c/diff", "candidate://c/comparison", "artifact://a/metadata",
	} {
		require.True(t, approvedResourceURI(raw), raw)
	}
	for _, raw := range []string{
		"file:///tmp/x", "repo://rev/other", "candidate://c/diff/extra", "campaign:///manifest", "run://r/summary?raw=1",
	} {
		require.False(t, approvedResourceURI(raw), raw)
	}
}

func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	left, right := mcp.NewInMemoryTransports()
	_, err := server.Connect(ctx, left, nil)
	require.NoError(t, err)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, right, nil)
	require.NoError(t, err)
	return session
}

func decodeTool[T any](t *testing.T, value any) T {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var result T
	require.NoError(t, json.Unmarshal(encoded, &result))
	return result
}

func waitForJob(t *testing.T, manager jobs.Manager, id string) domain.Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Get(id)
		require.NoError(t, err)
		if job.Status == domain.JobSucceeded || job.Status == domain.JobFailed {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return domain.Job{}
}

type stubBackend struct {
	inspections  int
	registered   WorkloadRegistrationInput
	lastResource string
}

func (b *stubBackend) StartCampaign(context.Context, CampaignStartInput) (OperationResult, error) {
	return OperationResult{ResultURI: "campaign://campaign-1/manifest"}, nil
}

func (b *stubBackend) GetCampaign(context.Context, CampaignStatusInput) (CampaignStatus, error) {
	return CampaignStatus{CampaignID: "campaign-1", Status: "running", ResourceURI: "campaign://campaign-1/manifest"}, nil
}

func (b *stubBackend) CancelCampaign(context.Context, CampaignCancelInput) (CampaignStatus, error) {
	return CampaignStatus{CampaignID: "campaign-1", Status: "cancelled", ResourceURI: "campaign://campaign-1/manifest"}, nil
}

func (b *stubBackend) InspectRepository(context.Context, RepositoryInspectionInput) (OperationResult, error) {
	b.inspections++
	return OperationResult{ResultURI: "repo://revision/inventory"}, nil
}

func (b *stubBackend) RegisterWorkload(_ context.Context, input WorkloadRegistrationInput) (RegisteredWorkload, error) {
	b.registered = input
	return RegisteredWorkload{CampaignID: input.CampaignID, Workload: input.Workload, ResourceURI: "campaign://campaign-1/manifest"}, nil
}

func (b *stubBackend) RunWorkload(context.Context, WorkloadRunInput) (OperationResult, error) {
	return OperationResult{ResultURI: "run://run-1/summary"}, nil
}

func (b *stubBackend) CreateCandidate(context.Context, CandidateCreateInput) (OperationResult, error) {
	return OperationResult{ResultURI: "candidate://candidate-1/diff"}, nil
}

func (b *stubBackend) EvaluateCandidate(context.Context, CandidateEvaluationInput) (OperationResult, error) {
	return OperationResult{ResultURI: "candidate://candidate-1/comparison"}, nil
}

func (b *stubBackend) GetFindings(context.Context, FindingsInput) (Findings, error) {
	return Findings{CampaignID: "campaign-1", ResourceURI: "campaign://campaign-1/hot-paths", Summary: "no hotspots"}, nil
}

func (b *stubBackend) GetReport(context.Context, ReportInput) (Report, error) {
	return Report{CampaignID: "campaign-1", ResourceURI: "campaign://campaign-1/report", Summary: "empty"}, nil
}

func (b *stubBackend) ReadResource(_ context.Context, uri string) (ResourceDocument, error) {
	b.lastResource = uri
	return ResourceDocument{MIMEType: "application/json", Text: `{"hotspots":[]}`}, nil
}
