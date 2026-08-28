package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/jobs"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/mcpserver"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

type Dependencies struct {
	Stdout io.Writer
	Stderr io.Writer
}

func New(deps Dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:           "gotorque",
		Short:         "Find and validate behavior-preserving Go CLI optimizations",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(deps.Stdout)
	root.SetErr(deps.Stderr)
	root.AddCommand(newManifestCommand(deps.Stdout))
	root.AddCommand(newOptimizeCommand(deps.Stdout))
	root.AddCommand(newReportCommand(deps.Stdout))
	root.AddCommand(newMCPCommand())
	root.AddCommand(newVersionCommand(deps.Stdout))
	return root
}

type optimizeFlags struct {
	repo, manifestPath, campaignDir, resume string
	runADK, runADKStub                      bool
}

func newOptimizeCommand(out io.Writer) *cobra.Command {
	var f optimizeFlags
	cmd := &cobra.Command{
		Use:   "optimize",
		Short: "Run or resume an in-process optimization campaign",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOptimize(cmd.Context(), out, f)
		},
	}
	cmd.Flags().StringVar(&f.repo, "repo", "", "absolute or resolvable local Git repository")
	cmd.Flags().StringVar(&f.manifestPath, "manifest", "", "target manifest path")
	cmd.Flags().StringVar(&f.campaignDir, "campaign-dir", "", "campaign storage directory")
	cmd.Flags().StringVar(&f.resume, "resume", "", "resume an existing campaign directory")
	cmd.Flags().BoolVar(&f.runADK, "adk", false, "run the full ADK graph using the OpenAI-compatible endpoint")
	cmd.Flags().BoolVar(&f.runADKStub, "adk-stub", false, "run the full ADK graph with deterministic stub agents")
	return cmd
}

func runOptimize(ctx context.Context, out io.Writer, f optimizeFlags) error {
	if f.runADK && f.runADKStub {
		return errors.New("--adk and --adk-stub are mutually exclusive")
	}
	roleSet, adkConfig, err := configureOptimizeAgents(ctx, f)
	if err != nil {
		return err
	}
	if f.resume != "" {
		return resumeOptimize(ctx, out, f, roleSet, adkConfig)
	}
	return createAndRunOptimize(ctx, out, f, roleSet, adkConfig)
}

func configureOptimizeAgents(ctx context.Context, f optimizeFlags) (*agents.Set, *orchestrator.Config, error) {
	if f.runADK {
		if f.manifestPath == "" {
			return nil, nil, errors.New("--manifest is required with --adk")
		}
		return configureADK(ctx, f.manifestPath)
	}
	if !f.runADKStub {
		return nil, nil, nil
	}
	configured, err := agents.NewDeterministicSet()
	if err != nil {
		return nil, nil, err
	}
	return &configured, &orchestrator.Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: 20 * time.Minute, AgentTimeout: 2 * time.Minute, MaxConcurrency: 1}, nil
}

func resumeOptimize(ctx context.Context, out io.Writer, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) error {
	if f.repo != "" || f.manifestPath != "" || f.campaignDir != "" {
		return fmt.Errorf("--resume cannot be combined with --repo, --manifest, or --campaign-dir")
	}
	engine, err := campaign.Resume(f.resume, out)
	if err != nil {
		return err
	}
	defer engine.Close()
	if err := attachResumeADK(ctx, engine, f, roleSet, adkConfig); err != nil {
		return err
	}
	if err := engine.Run(ctx); err != nil {
		return err
	}
	return printCampaignComplete(out, engine)
}

func attachResumeADK(ctx context.Context, engine *campaign.Engine, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) error {
	if engine.State().ADKMode != "" && !f.runADK && !f.runADKStub {
		return fmt.Errorf("campaign %s was started with model agents; pass --adk or --adk-stub to resume model-driven work", f.resume)
	}
	if f.runADK {
		configured, config, err := configureADK(ctx, engine.State().ManifestPath)
		if err != nil {
			return err
		}
		engine.SetADK(configured, config)
		return nil
	}
	if f.runADKStub {
		engine.SetADK(roleSet, adkConfig)
	}
	return nil
}

func createAndRunOptimize(ctx context.Context, out io.Writer, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) error {
	if f.repo == "" || f.manifestPath == "" {
		return fmt.Errorf("--repo and --manifest are required unless --resume is used")
	}
	engine, err := campaign.Create(ctx, campaign.Options{Repository: f.repo, ManifestPath: f.manifestPath, CampaignDir: f.campaignDir, Progress: out, ADKAgents: roleSet, ADKConfig: adkConfig})
	if err != nil {
		return err
	}
	defer engine.Close()
	if err := engine.Run(ctx); err != nil {
		return err
	}
	return printCampaignComplete(out, engine)
}

func printCampaignComplete(out io.Writer, engine *campaign.Engine) error {
	_, err := fmt.Fprintf(out, "campaign %s complete: %s\n", engine.State().ID, engine.State().Directory)
	return err
}

func configureADK(ctx context.Context, manifestPath string) (*agents.Set, *orchestrator.Config, error) {
	if manifestPath == "" {
		return nil, nil, errors.New("--manifest is required with --adk")
	}
	provider := agents.NewOpenAIProviderFromEnvironment()
	if err := provider.ValidateConnectivity(ctx); err != nil {
		return nil, nil, err
	}
	roles, err := agents.NewSet(ctx, provider)
	if err != nil {
		return nil, nil, err
	}
	m, err := manifest.LoadFile(manifestPath)
	if err != nil {
		return nil, nil, err
	}
	config := orchestrator.DefaultConfig()
	config.MaxCandidates = m.Campaign.MaxCandidatePatches
	config.MaxConsecutiveFailures = m.Campaign.StopAfterFailures
	config.DeterministicTimeout = m.Campaign.MinimumCommandTimeout.Duration()
	return &roles, &config, nil
}

func newReportCommand(out io.Writer) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "report CAMPAIGN_DIR",
		Short: "Render a persisted campaign report",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			state, err := campaign.LoadReport(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(out).Encode(state)
			}
			_, err = io.WriteString(out, campaign.RenderMarkdown(state))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newMCPCommand() *cobra.Command {
	var stateRoot string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the campaign engine over stdio MCP",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if stateRoot == "" {
				return fmt.Errorf("--state-root is required")
			}
			if !filepath.IsAbs(stateRoot) {
				return fmt.Errorf("--state-root must be absolute")
			}
			if err := os.MkdirAll(stateRoot, 0o700); err != nil {
				return err
			}
			server, err := mcpserver.New(newEngineBackend(stateRoot), jobs.NewMemoryManager(jobs.Options{}))
			if err != nil {
				return err
			}
			return server.MCP.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	serve.Flags().StringVar(&stateRoot, "state-root", "", "absolute campaign state root")
	cmd := &cobra.Command{Use: "mcp", Short: "MCP control surface"}
	cmd.AddCommand(serve)
	return cmd
}

func newManifestCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "manifest", Short: "Inspect target manifests"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate PATH",
		Short: "Validate and normalize a target manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			loaded, err := manifest.LoadFile(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "valid target manifest %q (%d seed workloads)\n", loaded.Name, len(loaded.Workloads.Seeds))
			return err
		},
	})
	return cmd
}

func newVersionCommand(out io.Writer) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			info := version.Current()
			if asJSON {
				return json.NewEncoder(out).Encode(info)
			}
			_, err := fmt.Fprintf(out, "gotorque %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}
