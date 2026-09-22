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
	"example.com/gotorque/internal/jev"
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
	analyst, reviewer, explorer             string
}

// Role backends for --analyst and --reviewer. llm is the model role; jev
// replaces it with TypeSafe Jev judgments ranked in code.
const (
	analystLLM = "llm"
	analystJev = "jev"
)

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
	cmd.Flags().StringVar(&f.analyst, "analyst", analystLLM, "analyst backend: llm (the analyst model role) or jev (TypeSafe Jev cause classification; needs "+jev.EnvAPIKey+" with --adk)")
	cmd.Flags().StringVar(&f.explorer, "explorer", analystLLM, "explorer backend: llm (the explorer model role) or jev (the target's own options, judged by TypeSafe Jev and sampled in discovery; needs "+jev.EnvAPIKey+" with --adk)")
	cmd.Flags().StringVar(&f.reviewer, "reviewer", analystLLM, "reviewer backend: llm (the reviewer model role) or jev (TypeSafe Jev behaviour-hazard checks; needs "+jev.EnvAPIKey+" with --adk)")
	return cmd
}

func runOptimize(ctx context.Context, out io.Writer, f optimizeFlags) error {
	if f.runADK && f.runADKStub {
		return errors.New("--adk and --adk-stub are mutually exclusive")
	}
	if err := validateJevRoles(f); err != nil {
		return err
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
	roles, config, err := configureFreshAgents(ctx, out, f)
	if err != nil {
		return nil, nil, err
	}
	return roles, config, selectJev(ctx, out, roles, f)
}

func configureFreshAgents(ctx context.Context, out io.Writer, f optimizeFlags) (*agents.Set, *orchestrator.Config, error) {
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
	if err := requireSameJevRoles(engine.State(), f); err != nil {
		return err
	}
	roles, config, err := resumeRoles(ctx, out, engine, f, roleSet, adkConfig)
	if err != nil || roles == nil {
		return err
	}
	if err := selectJev(ctx, out, roles, f); err != nil {
		return err
	}
	engine.SetADK(roles, config)
	return nil
}

// resumeRoles rebuilds the role set a resumed campaign runs with, or nil when
// neither --adk nor --adk-stub was given.
func resumeRoles(ctx context.Context, out io.Writer, engine *campaign.Engine, f optimizeFlags, roleSet *agents.Set, adkConfig *orchestrator.Config) (*agents.Set, *orchestrator.Config, error) {
	if f.runADK {
		return configureADK(ctx, out, engine.State().ManifestPath)
	}
	if !f.runADKStub {
		return nil, nil, nil
	}
	if roleSet != nil {
		return roleSet, adkConfig, nil
	}
	return deterministicAgents() //nolint:contextcheck // static stub set: no I/O, nothing to cancel
}

// requireSameJevRoles applies the --adk rule to the Jev roles: a resumed
// campaign must not quietly change where its hypotheses or reviews come from
// halfway through.
func requireSameJevRoles(state campaign.State, f optimizeFlags) error {
	if state.Analyst == campaign.AnalystJev && f.analyst != analystJev {
		return fmt.Errorf("campaign %s was started with --analyst jev; pass it again to resume with the same analyst", f.resume)
	}
	if state.Reviewer == campaign.ReviewerJev && f.reviewer != analystJev {
		return fmt.Errorf("campaign %s was started with --reviewer jev; pass it again to resume with the same reviewer", f.resume)
	}
	if state.Explorer == campaign.ExplorerJev && f.explorer != analystJev {
		return fmt.Errorf("campaign %s was started with --explorer jev; pass it again to resume with the same explorer", f.resume)
	}
	return nil
}

func validateJevRoles(f optimizeFlags) error {
	for _, backend := range []struct{ flag, value string }{{"--analyst", f.analyst}, {"--reviewer", f.reviewer}, {"--explorer", f.explorer}} {
		if err := validateBackend(backend.flag, backend.value, f); err != nil {
			return err
		}
	}
	return nil
}

func validateBackend(flag, value string, f optimizeFlags) error {
	switch value {
	case "", analystLLM:
		return nil
	case analystJev:
		if !f.runADK && !f.runADKStub {
			return fmt.Errorf("%s jev needs --adk or --adk-stub: it replaces a role of the agent graph", flag)
		}
		return nil
	}
	return fmt.Errorf("unknown %s %q: want %s or %s", flag, value, analystLLM, analystJev)
}

// selectJev swaps the analyst, reviewer, and explorer roles for Jev when
// --analyst jev, --reviewer jev, or --explorer jev asks for it. Under --adk-stub
// it uses the no-network stub; under --adk it spends one request proving the key
// and billing work, because a gateway account without a card on file refuses
// every request and those roles are reached only after the baseline has been
// built.
func selectJev(ctx context.Context, out io.Writer, roles *agents.Set, f optimizeFlags) error {
	if roles == nil || (f.analyst != analystJev && f.reviewer != analystJev && f.explorer != analystJev) {
		return nil
	}
	evaluator, err := jevEvaluator(ctx, f)
	if err != nil {
		return err
	}
	lines, err := assignJev(roles, evaluator, f) //nolint:contextcheck // assigns fields and builds a static stub agent: no I/O, nothing to cancel
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(out, "%s (%s)\n", line, jev.Model); err != nil {
			return err
		}
	}
	return nil
}

// assignJev hands the evaluator to each role the flags move to Jev and names
// those roles for the progress output.
func assignJev(roles *agents.Set, evaluator jev.Evaluator, f optimizeFlags) ([]string, error) {
	var lines []string
	if f.analyst == analystJev {
		roles.CauseEvaluator = evaluator
		lines = append(lines, "analyst: Jev cause classification")
	}
	if f.reviewer == analystJev {
		roles.ReviewEvaluator = evaluator
		lines = append(lines, "reviewer: Jev behaviour-hazard checks")
	}
	if f.explorer == analystJev {
		explorer, err := agents.PlannedExplorer()
		if err != nil {
			return nil, err
		}
		roles.Explorer, roles.ExploreEvaluator = explorer, evaluator
		lines = append(lines, "explorer: the target's own processing modes, judged by Jev")
	}
	return lines, nil
}

func jevEvaluator(ctx context.Context, f optimizeFlags) (jev.Evaluator, error) {
	if f.runADKStub {
		return jev.Stub{}, nil
	}
	client := jev.NewClientFromEnvironment()
	if err := client.Preflight(ctx); err != nil {
		return nil, err
	}
	return client, nil
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
	config := orchestratorConfigFromManifest(m)
	return &roles, &config, nil
}

// orchestratorConfigFromManifest maps a loaded target manifest onto the ADK
// graph's node-level config. It is pure (no I/O) so the mapping can be unit
// tested without a manifest file or campaign directory.
//
// DeterministicTimeout deliberately does NOT come from
// minimum_command_timeout: that field is a per-command floor (see
// engine.go's use of it to size individual toolchain invocations), while
// DeterministicTimeout is the deadline the ADK scheduler applies to an
// entire deterministic graph node (evaluate_candidate, run_discovery) via
// context.WithTimeout. evaluate_candidate alone runs a build, `go test`, 25
// A/B pairs per workload, and a PGO lane; every shipped manifest sets
// minimum_command_timeout to 30s, which is nowhere near enough for that
// node and would fail the run with a bare "context deadline exceeded" the
// moment a heavier target or a cold build cache pushed evaluation past it.
// DefaultConfig's 20-minute deadline is kept instead: it already sits inside
// (and is bounded by) the campaign-wide max_duration deadline, so a stuck
// node still cannot run away with the whole campaign.
func orchestratorConfigFromManifest(m manifest.Manifest) orchestrator.Config {
	config := orchestrator.DefaultConfig()
	config.MaxCandidates = m.Campaign.MaxCandidatePatches
	config.MaxConsecutiveFailures = m.Campaign.StopAfterFailures
	// Zero leaves the historical behavior, where an inconclusive verdict
	// counts toward stop_after_failures instead of a bound of its own. A
	// manifest that wants its patch budget spent on unresolved candidates
	// sets stop_after_inconclusive.
	config.MaxConsecutiveInconclusive = m.Campaign.StopAfterInconclusive
	return config
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
