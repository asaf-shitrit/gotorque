package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/campaign"
	"example.com/gotorque/internal/manifest"
	"example.com/gotorque/internal/orchestrator"
	"example.com/gotorque/internal/version"
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
	roleSet, adkConfig, err := configureOptimizeAgents(ctx, out, f)
	if err != nil {
		return err
	}
	if f.resume != "" {
		return resumeOptimize(ctx, out, f, roleSet, adkConfig)
	}
	return createAndRunOptimize(ctx, out, f, roleSet, adkConfig)
}

func configureOptimizeAgents(ctx context.Context, out io.Writer, f optimizeFlags) (*agents.Set, *orchestrator.Config, error) {
	// A resumed campaign takes its manifest from persisted state, and
	// --resume rejects an explicit --manifest, so requiring one here made
	// "optimize --resume <dir> --adk" impossible to satisfy in either
	// direction. attachResumeADK configures the roles from that state.
	if f.resume != "" {
		return nil, nil, nil
	}
	if f.runADK {
		if f.manifestPath == "" {
			return nil, nil, errors.New("--manifest is required with --adk")
		}
		return configureADK(ctx, out, f.manifestPath)
	}
	if !f.runADKStub {
		return nil, nil, nil
	}
	return deterministicAgents() //nolint:contextcheck // builds five static stub agents from literals: no I/O, nothing to cancel
}

// deterministicAgents builds the stub role set used by --adk-stub, shared by
// fresh and resumed campaigns.
func deterministicAgents() (*agents.Set, *orchestrator.Config, error) {
	configured, err := agents.NewDeterministicSet()
	if err != nil {
		return nil, nil, err
	}
	return &configured, &orchestrator.Config{MaxCandidates: 1, MaxConsecutiveFailures: 1, DeterministicTimeout: 20 * time.Minute, AgentTimeout: 2 * time.Minute, MaxConcurrency: 1}, nil
}

func resumeOptimize(ctx context.Context, out io.Writer, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) (err error) {
	if f.repo != "" || f.manifestPath != "" || f.campaignDir != "" {
		return errors.New("--resume cannot be combined with --repo, --manifest, or --campaign-dir")
	}
	engine, err := campaign.Resume(f.resume, out)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	if err := attachResumeADK(ctx, out, engine, f, roleSet, adkConfig); err != nil {
		return err
	}
	if err := engine.Run(ctx); err != nil {
		return err
	}
	return printCampaignComplete(out, engine)
}

func attachResumeADK(ctx context.Context, out io.Writer, engine *campaign.Engine, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) error {
	if engine.State().ADKMode != "" && !f.runADK && !f.runADKStub {
		return fmt.Errorf("campaign %s was started with model agents; pass --adk or --adk-stub to resume model-driven work", f.resume)
	}
	if f.runADK {
		configured, config, err := configureADK(ctx, out, engine.State().ManifestPath)
		if err != nil {
			return err
		}
		engine.SetADK(configured, config)
		return nil
	}
	if f.runADKStub {
		if roleSet == nil {
			configured, config, err := deterministicAgents() //nolint:contextcheck // static stub set: no I/O, nothing to cancel
			if err != nil {
				return err
			}
			roleSet, adkConfig = configured, config
		}
		engine.SetADK(roleSet, adkConfig)
	}
	return nil
}

func createAndRunOptimize(ctx context.Context, out io.Writer, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) (err error) {
	if f.repo == "" || f.manifestPath == "" {
		return errors.New("--repo and --manifest are required unless --resume is used")
	}
	engine, err := campaign.Create(ctx, campaign.Options{Repository: f.repo, ManifestPath: f.manifestPath, CampaignDir: f.campaignDir, Progress: out, ADKAgents: roleSet, ADKConfig: adkConfig})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	if err := engine.Run(ctx); err != nil {
		return err
	}
	return printCampaignComplete(out, engine)
}

func printCampaignComplete(out io.Writer, engine *campaign.Engine) error {
	_, err := fmt.Fprintf(out, "campaign %s complete: %s\n", engine.State().ID, engine.State().Directory)
	return err
}

func configureADK(ctx context.Context, out io.Writer, manifestPath string) (*agents.Set, *orchestrator.Config, error) {
	if manifestPath == "" {
		return nil, nil, errors.New("--manifest is required with --adk")
	}
	provider := agents.NewOpenAIProviderFromEnvironment()
	// Role calls are the slowest and least observable part of a campaign;
	// without per-attempt lines the run prints nothing between starting the
	// workflow and the first role that completes.
	provider.Observer = agents.LogCalls(out)
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
	// Zero leaves the historical behavior, where an inconclusive verdict
	// counts toward stop_after_failures instead of a bound of its own. A
	// manifest that wants its patch budget spent on unresolved candidates
	// sets stop_after_inconclusive.
	config.MaxConsecutiveInconclusive = m.Campaign.StopAfterInconclusive
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
