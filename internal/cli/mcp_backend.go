package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/mcpserver"
	"example.com/gotorque/internal/toolchain"
)

type engineBackend struct {
	root      string
	mu        sync.Mutex
	cancel    map[string]context.CancelFunc
	active    map[string]*campaign.Engine
	inventory *toolchain.Toolchain // nil until the first inspection; injectable for tests
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

// repositoryInventory is the JSON document addressed by a
// repo://{inspection_id}/inventory resource.
type repositoryInventory struct {
	Hash     string   `json:"hash"`
	Packages []string `json:"packages"`
	Commands []string `json:"commands"`
}

// InspectRepository inventories a repository without starting a campaign. It
// runs the allowlisted `go list` wrapper, persists the resulting package and
// command lists under the harness root, and returns a compact result whose
// ResultURI addresses the full inventory document.
func (b *engineBackend) InspectRepository(ctx context.Context, input mcpserver.RepositoryInspectionInput) (mcpserver.OperationResult, error) {
	repository := input.Repository
	if repository == "" || !filepath.IsAbs(repository) {
		return mcpserver.OperationResult{}, fmt.Errorf("repository %q must be an absolute path", repository)
	}
	info, err := os.Stat(repository)
	if err != nil {
		return mcpserver.OperationResult{}, err
	}
	if !info.IsDir() {
		return mcpserver.OperationResult{}, fmt.Errorf("repository %q is not a directory", repository)
	}
	listing, err := b.listingToolchain().GoList(ctx, repository)
	if err != nil {
		return mcpserver.OperationResult{}, fmt.Errorf("inventory Go packages: %w", err)
	}
	packages, commands, err := parseGoList(listing.Stdout)
	if err != nil {
		return mcpserver.OperationResult{}, err
	}
	hash, err := b.storeInventory(repositoryInventory{Packages: packages, Commands: commands})
	if err != nil {
		return mcpserver.OperationResult{}, err
	}
	return mcpserver.OperationResult{
		ResultURI: "repo://" + hash + "/inventory",
		Summary:   fmt.Sprintf("discovered %d packages and %d command entry points", len(packages), len(commands)),
	}, nil
}

// listingToolchain lazily builds the toolchain used for standalone
// inspections. Tests may pre-set b.inventory with a fake executor.
func (b *engineBackend) listingToolchain() *toolchain.Toolchain {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inventory == nil {
		b.inventory = toolchain.New(toolchain.Options{})
	}
	return b.inventory
}

// parseGoList decodes the streamed `go list -json` documents into sorted
// package import paths and command (package main) entry points.
func parseGoList(stdout []byte) ([]string, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var packages, commands []string
	for {
		var item struct{ ImportPath, Name string }
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("decode go list: %w", err)
		}
		packages = append(packages, item.ImportPath)
		if item.Name == "main" {
			commands = append(commands, item.ImportPath)
		}
	}
	sort.Strings(packages)
	sort.Strings(commands)
	return packages, commands, nil
}

// storeInventory writes the inventory document below the harness root keyed
// by a content hash and returns the hash used in the repo:// resource URI.
func (b *engineBackend) storeInventory(inventory repositoryInventory) (string, error) {
	digest := sha256.Sum256([]byte(strings.Join(inventory.Packages, "\n") + "\x00" + strings.Join(inventory.Commands, "\n")))
	inventory.Hash = hex.EncodeToString(digest[:8])
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(b.root, "inspection", inventory.Hash)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "inventory.json"), data, 0o600); err != nil {
		return "", err
	}
	return inventory.Hash, nil
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
	switch parsed.Scheme {
	case "campaign":
		return b.readCampaignResource(parsed)
	case "repo":
		return b.readRepositoryResource(parsed)
	default:
		return mcpserver.ResourceDocument{}, errors.New("resource is not owned by campaign engine")
	}
}

func (b *engineBackend) readCampaignResource(parsed *url.URL) (mcpserver.ResourceDocument, error) {
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

func (b *engineBackend) readRepositoryResource(parsed *url.URL) (mcpserver.ResourceDocument, error) {
	if kind := strings.TrimPrefix(parsed.Path, "/"); kind != "inventory" {
		return mcpserver.ResourceDocument{}, fmt.Errorf("unknown repository resource %q", kind)
	}
	data, err := os.ReadFile(filepath.Join(b.root, "inspection", parsed.Host, "inventory.json"))
	if os.IsNotExist(err) {
		return mcpserver.ResourceDocument{}, fmt.Errorf("repository inspection %q not found", parsed.Host)
	}
	if err != nil {
		return mcpserver.ResourceDocument{}, err
	}
	return mcpserver.ResourceDocument{MIMEType: "application/json", Text: string(data)}, nil
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
