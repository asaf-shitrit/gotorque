package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/mcpserver"
)

type engineBackend struct {
	root   string
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
	active map[string]*campaign.Engine
}

func newEngineBackend(root string) *engineBackend {
	return &engineBackend{root: root, cancel: map[string]context.CancelFunc{}, active: map[string]*campaign.Engine{}}
}

func (b *engineBackend) StartCampaign(ctx context.Context, input mcpserver.CampaignStartInput) (mcpserver.OperationResult, error) {
	manifestPath, err := localManifestPath(input.ManifestURI)
	if err != nil {
		return mcpserver.OperationResult{}, err
	}
	dir, err := os.MkdirTemp(b.root, "campaign-")
	if err != nil {
		return mcpserver.OperationResult{}, err
	}
	engine, err := campaign.Create(ctx, campaign.Options{Repository: input.Repository, ManifestPath: manifestPath, CampaignDir: dir})
	if err != nil {
		_ = os.RemoveAll(dir)
		return mcpserver.OperationResult{}, err
	}
	defer engine.Close()
	runCtx, cancel := context.WithCancel(ctx)
	id := engine.State().ID
	b.mu.Lock()
	b.cancel[id] = cancel
	b.active[id] = engine
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.cancel, id); delete(b.active, id); b.mu.Unlock(); cancel() }()
	if err := engine.Run(runCtx); err != nil {
		return mcpserver.OperationResult{}, err
	}
	return mcpserver.OperationResult{ResultURI: "campaign://" + id + "/report", Summary: engine.State().StopReason}, nil
}

func (b *engineBackend) GetCampaign(_ context.Context, input mcpserver.CampaignStatusInput) (mcpserver.CampaignStatus, error) {
	b.mu.Lock()
	active := b.active[input.CampaignID]
	b.mu.Unlock()
	if active != nil {
		return mcpserver.CampaignStatus{CampaignID: input.CampaignID, Status: "running", ResourceURI: "campaign://" + input.CampaignID + "/report"}, nil
	}
	state, _, err := b.find(input.CampaignID)
	if err != nil {
		return mcpserver.CampaignStatus{}, err
	}
	return mcpserver.CampaignStatus{CampaignID: state.ID, Status: string(state.Status), ResourceURI: "campaign://" + state.ID + "/report", Summary: state.StopReason}, nil
}

func (b *engineBackend) CancelCampaign(_ context.Context, input mcpserver.CampaignCancelInput) (mcpserver.CampaignStatus, error) {
	b.mu.Lock()
	cancel := b.cancel[input.CampaignID]
	b.mu.Unlock()
	if cancel == nil {
		return mcpserver.CampaignStatus{}, fmt.Errorf("campaign %q is not running", input.CampaignID)
	}
	cancel()
	return mcpserver.CampaignStatus{CampaignID: input.CampaignID, Status: "cancelling"}, nil
}

func (b *engineBackend) InspectRepository(context.Context, mcpserver.RepositoryInspectionInput) (mcpserver.OperationResult, error) {
	return mcpserver.OperationResult{}, errors.New("repository inspection requires campaign_start in this engine version")
}
func (b *engineBackend) RegisterWorkload(context.Context, mcpserver.WorkloadRegistrationInput) (mcpserver.RegisteredWorkload, error) {
	return mcpserver.RegisteredWorkload{}, errors.New("workload registration requires an active campaign")
}
func (b *engineBackend) RunWorkload(context.Context, mcpserver.WorkloadRunInput) (mcpserver.OperationResult, error) {
	return mcpserver.OperationResult{}, errors.New("standalone workload execution is not implemented")
}
func (b *engineBackend) CreateCandidate(context.Context, mcpserver.CandidateCreateInput) (mcpserver.OperationResult, error) {
	return mcpserver.OperationResult{}, errors.New("candidate stage is not implemented")
}
func (b *engineBackend) EvaluateCandidate(context.Context, mcpserver.CandidateEvaluationInput) (mcpserver.OperationResult, error) {
	return mcpserver.OperationResult{}, errors.New("candidate stage is not implemented")
}

func (b *engineBackend) GetFindings(_ context.Context, input mcpserver.FindingsInput) (mcpserver.Findings, error) {
	state, _, err := b.find(input.CampaignID)
	if err != nil {
		return mcpserver.Findings{}, err
	}
	return mcpserver.Findings{CampaignID: state.ID, ResourceURI: "campaign://" + state.ID + "/hot-paths", Summary: fmt.Sprintf("%d packages and %d command entry points discovered", len(state.Inventory.Packages), len(state.Inventory.Commands))}, nil
}

func (b *engineBackend) GetReport(_ context.Context, input mcpserver.ReportInput) (mcpserver.Report, error) {
	state, _, err := b.find(input.CampaignID)
	if err != nil {
		return mcpserver.Report{}, err
	}
	return mcpserver.Report{CampaignID: state.ID, ResourceURI: "campaign://" + state.ID + "/report", Summary: state.StopReason}, nil
}

func (b *engineBackend) ReadResource(_ context.Context, uri string) (mcpserver.ResourceDocument, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return mcpserver.ResourceDocument{}, err
	}
	if parsed.Scheme != "campaign" {
		return mcpserver.ResourceDocument{}, errors.New("resource is not owned by campaign engine")
	}
	id := parsed.Host
	kind := strings.TrimPrefix(parsed.Path, "/")
	state, _, err := b.find(id)
	if err != nil {
		return mcpserver.ResourceDocument{}, err
	}
	switch kind {
	case "report":
		return mcpserver.ResourceDocument{MIMEType: "text/markdown", Text: campaign.RenderMarkdown(state)}, nil
	case "manifest":
		data, _ := json.MarshalIndent(state.Manifest, "", "  ")
		return mcpserver.ResourceDocument{MIMEType: "application/json", Text: string(data)}, nil
	case "hot-paths":
		data, _ := json.Marshal(state.Inventory)
		return mcpserver.ResourceDocument{MIMEType: "application/json", Text: string(data)}, nil
	default:
		return mcpserver.ResourceDocument{}, fmt.Errorf("unknown campaign resource %q", kind)
	}
}

func (b *engineBackend) find(id string) (campaign.State, string, error) {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return campaign.State{}, "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(b.root, entry.Name())
		state, loadErr := campaign.LoadReport(dir)
		if loadErr == nil && state.ID == id {
			return state, dir, nil
		}
	}
	return campaign.State{}, "", fmt.Errorf("campaign %q not found", id)
}

func localManifestPath(value string) (string, error) {
	if value == "" {
		return "", errors.New("manifest_uri is required")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" {
		return value, nil
	}
	if parsed.Scheme != "file" {
		return "", errors.New("manifest_uri must be a local path or file URI")
	}
	return parsed.Path, nil
}
