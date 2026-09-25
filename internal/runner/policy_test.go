package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"example.com/gotorque/internal/domain"
)

// TestBuildEnvDropsUnlistedSecrets is the required regression: a workload's
// environment must not carry a variable the manifest does not name, even
// when that variable is set on gotorque's own process, which is where model
// provider credentials such as OPENROUTER_API_KEY and AI_GATEWAY_API_KEY
// live.
func TestBuildEnvDropsUnlistedSecrets(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-live-secret")
	t.Setenv("AI_GATEWAY_API_KEY", "gw-live-secret")
	t.Setenv("LANG", "en_US.UTF-8")

	policy := EnvironmentPolicy{Allow: []string{"LANG"}}
	env := BuildEnv(policy, []string{"HOME=/sandbox/home"}, map[string]string{"GOTOOLCHAIN": "local"})

	for _, secret := range []string{"OPENROUTER_API_KEY", "AI_GATEWAY_API_KEY"} {
		for _, kv := range env {
			if strings.HasPrefix(kv, secret+"=") {
				t.Fatalf("workload env leaked %s: %v", secret, env)
			}
		}
	}
	if !containsPrefix(env, "LANG=en_US.UTF-8") {
		t.Fatalf("expected allowed LANG to pass through, got %v", env)
	}
	if !containsPrefix(env, "GOTOOLCHAIN=local") {
		t.Fatalf("expected infrastructure GOTOOLCHAIN to always be set, got %v", env)
	}
	if !containsPrefix(env, "HOME=/sandbox/home") {
		t.Fatalf("expected sandbox base HOME to survive, got %v", env)
	}
}

// TestBuildEnvPassthroughIsSeparateFromAllow proves a name listed only under
// Passthrough still reaches the workload from gotorque's own environment,
// and that an additional variable gotorque supplies is dropped unless Allow
// names it -- Allow and Passthrough gate different things (see
// EnvironmentPolicy's doc comment) but both draw from the parent process.
func TestBuildEnvPassthroughIsSeparateFromAllow(t *testing.T) {
	t.Setenv("TARGET_LOCALE", "fr_FR")
	policy := EnvironmentPolicy{Passthrough: []string{"TARGET_LOCALE"}}
	env := BuildEnv(policy, nil, map[string]string{"EXTRA": "value"})
	if !containsPrefix(env, "TARGET_LOCALE=fr_FR") {
		t.Fatalf("expected passthrough variable, got %v", env)
	}
	if containsPrefix(env, "EXTRA=") {
		t.Fatalf("expected EXTRA dropped: not infrastructure, not in Allow: got %v", env)
	}
}

// TestBuildEnvBaseWinsOverPassthrough guards the gron manifest's shape
// (environment.allow: ["LANG", "HOME"]): declaring HOME non-secret must not
// replace the sandbox's own HOME (pointed at a fresh temp directory) with
// gotorque's own process HOME.
func TestBuildEnvBaseWinsOverPassthrough(t *testing.T) {
	t.Setenv("HOME", "/Users/real-developer")
	env := BuildEnv(EnvironmentPolicy{Allow: []string{"HOME"}}, []string{"HOME=/sandbox/home"}, nil)
	homes := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			homes++
			if kv != "HOME=/sandbox/home" {
				t.Fatalf("HOME was overridden by passthrough: %q", kv)
			}
		}
	}
	if homes != 1 {
		t.Fatalf("expected exactly one HOME entry, got %d in %v", homes, env)
	}
}

// TestBuildEnvOmitsUnsetPassthrough covers the case where a manifest names a
// passthrough variable gotorque's own process never had set: it must be
// left out, not synthesized as an empty string.
func TestBuildEnvOmitsUnsetPassthrough(t *testing.T) {
	name := "GOTORQUE_TEST_UNSET_VARIABLE_XYZ"
	os.Unsetenv(name) //nolint:errcheck // best-effort; the point is that it is unset
	env := BuildEnv(EnvironmentPolicy{Allow: []string{name}}, nil, nil)
	if containsPrefix(env, name+"=") {
		t.Fatalf("expected unset variable omitted, got %v", env)
	}
}

func containsPrefix(env []string, prefix string) bool {
	for _, kv := range env {
		if kv == prefix || strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// TestRunHonorsManifestNetworkPolicy is the fix for the audited bug: network
// denial used to be driven only by whether local isolation was available,
// never by sandbox.network. With local isolation on, a "deny" policy must
// still produce a deny-network sandbox-exec profile, and "allow" must not.
func TestRunHonorsManifestNetworkPolicy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("exercises the darwin sandbox-exec profile construction directly")
	}
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(binary, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, policy SandboxPolicy) string {
		t.Helper()
		fake := &fakeExecutor{}
		r, err := New(Options{Executor: fake, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes"), LocalIsolation: true, Sandbox: policy})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), RunRequest{
			Build: Build{ID: "b", BinaryPath: binary}, Workload: domain.Workload{ID: "w"}, Mode: domain.RunModeMeasurement,
		}); err != nil {
			t.Fatal(err)
		}
		if len(fake.calls) != 1 {
			t.Fatalf("expected one call, got %d", len(fake.calls))
		}
		return strings.Join(fake.calls[0].Args, " ")
	}

	denyArgs := run(t, SandboxPolicy{NetworkAllowed: false, WriteScope: "temp_only"})
	if !strings.Contains(denyArgs, "(deny network*)") {
		t.Fatalf("network=deny must produce a deny-network profile, got %q", denyArgs)
	}
	allowArgs := run(t, SandboxPolicy{NetworkAllowed: true, WriteScope: "temp_only"})
	if strings.Contains(allowArgs, "(deny network*)") {
		t.Fatalf("network=allow must not deny network, got %q", allowArgs)
	}
}

// TestRunTestingBypassGrantsBothWithoutAPolicy covers the LocalIsolation:
// false escape hatch (TestingUnsafeDisableIsolation): with no enforcement
// mechanism available at all, both network and filesystem stay granted
// regardless of the manifest's policy, exactly as before this change, so
// tests that rely on it keep working unmodified.
func TestRunTestingBypassGrantsBothWithoutAPolicy(t *testing.T) {
	root := t.TempDir()
	store, err := NewArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(binary, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{}
	r, err := New(Options{Executor: fake, Artifacts: store, SandboxRoot: filepath.Join(root, "sandboxes"), Sandbox: SandboxPolicy{NetworkAllowed: false, WriteScope: "temp_only"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), RunRequest{
		Build: Build{ID: "b", BinaryPath: binary}, Workload: domain.Workload{ID: "w"}, Mode: domain.RunModeMeasurement,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWrapWithResourceLimitsAddsUlimitClauses(t *testing.T) {
	resetResourceLimitProbes()
	t.Cleanup(resetResourceLimitProbes)
	processLimitSupportedFn = func(context.Context) bool { return true }
	memoryLimitSupportedFn = func(context.Context) bool { return true }
	t.Cleanup(func() {
		processLimitSupportedFn = probeProcessLimit
		memoryLimitSupportedFn = probeMemoryLimit
	})

	path, args, notes := WrapWithResourceLimits(context.Background(), ResourceLimits{MaxProcesses: 4, MaxMemoryBytes: 2 << 20}, "/bin/app", []string{"--flag"})
	if path != resourceLimitShell {
		t.Fatalf("path = %q, want %q", path, resourceLimitShell)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ulimit -u 4") {
		t.Fatalf("expected process ulimit clause, got %q", joined)
	}
	// The memory rlimit is only attempted on Linux (see memoryLimitClause);
	// on other platforms the runner still notes the request but never wraps
	// a ulimit -v clause for it.
	if runtime.GOOS == "linux" && !strings.Contains(joined, "ulimit -v") {
		t.Fatalf("expected memory ulimit clause on linux, got %q", joined)
	}
	if !strings.Contains(joined, `exec "$0" "$@"`) {
		t.Fatalf("expected exec handoff, got %q", joined)
	}
	if !strings.HasSuffix(joined, "/bin/app --flag") {
		t.Fatalf("expected original path/args preserved at the end, got %q", joined)
	}
	if len(notes) != 2 {
		t.Fatalf("expected a note per limit (enforced or not), got %v", notes)
	}
}

func TestWrapWithResourceLimitsNoopWithoutLimits(t *testing.T) {
	path, args, notes := WrapWithResourceLimits(context.Background(), ResourceLimits{}, "/bin/app", []string{"--flag"})
	if path != "/bin/app" || len(args) != 1 || args[0] != "--flag" {
		t.Fatalf("expected passthrough with no limits requested, got (%q, %v)", path, args)
	}
	if len(notes) != 0 {
		t.Fatalf("expected no notes with no limits requested, got %v", notes)
	}
}

func TestWrapWithResourceLimitsNotesWhenUnsupported(t *testing.T) {
	resetResourceLimitProbes()
	t.Cleanup(resetResourceLimitProbes)
	processLimitSupportedFn = func(context.Context) bool { return false }
	memoryLimitSupportedFn = func(context.Context) bool { return false }
	t.Cleanup(func() {
		processLimitSupportedFn = probeProcessLimit
		memoryLimitSupportedFn = probeMemoryLimit
	})

	path, _, notes := WrapWithResourceLimits(context.Background(), ResourceLimits{MaxProcesses: 4, MaxMemoryBytes: 2 << 20}, "/bin/app", []string{"--flag"})
	if path != "/bin/app" {
		t.Fatalf("expected no shell wrapping when nothing could be enforced, got %q", path)
	}
	if len(notes) != 2 {
		t.Fatalf("expected a not-enforced note per requested limit, got %v", notes)
	}
	for _, note := range notes {
		if !strings.Contains(note, "not enforced") {
			t.Fatalf("expected an explicit not-enforced note, got %q", note)
		}
	}
}

func TestMemoryLimitNotEnforceableOffLinux(t *testing.T) {
	_, note := memoryLimitClause(context.Background(), "darwin", 1<<20)
	if !strings.Contains(note, "darwin") || !strings.Contains(note, "not enforced") {
		t.Fatalf("expected a degraded note naming darwin, got %q", note)
	}
}

func TestReadAndWriteScopeNotesNameTheGap(t *testing.T) {
	notes := readScopeNotes(SandboxPolicy{ReadScope: "manifest_paths"})
	if len(notes) != 1 || !strings.Contains(notes[0], "manifest_paths") {
		t.Fatalf("expected a manifest_paths note, got %v", notes)
	}
	if got := readScopeNotes(SandboxPolicy{ReadScope: "repo_and_assets"}); len(got) != 0 {
		t.Fatalf("expected no note for the default read scope, got %v", got)
	}
	if got := writeScopeNotes(SandboxPolicy{WriteScope: "manifest_paths"}); len(got) != 1 {
		t.Fatalf("expected a manifest_paths write note, got %v", got)
	}
	if got := writeScopeNotes(SandboxPolicy{WriteScope: "temp_only"}); len(got) != 0 {
		t.Fatalf("expected no note for temp_only, got %v", got)
	}
}

// TestLocalIsolationNotesLinuxNetworkDegrade reuses the fake-bwrap harness
// from isolate_test.go to force the network-namespace probe to fail, then
// checks the note surfaces the degradation instead of silently running the
// workload with network access.
func TestLocalIsolationNotesLinuxNetworkDegrade(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "probe.log")
	writeFakeBwrap(t, logPath, "0", "1") // isolation probe ok, net-namespace probe fails
	resetBwrapProbes()

	// Warm the probes exactly as Run() would, by calling isolatedCommandFor
	// first (localIsolationNotes assumes this ordering; see its doc comment).
	if _, _, err := isolatedCommandFor(context.Background(), "linux", t.TempDir(), t.TempDir(), true, "/bin/app", nil); err != nil {
		t.Fatal(err)
	}
	notes := localIsolationNotes(context.Background(), "linux", true, false)
	if len(notes) != 1 || !strings.Contains(notes[0], "network namespace") {
		t.Fatalf("expected a network-namespace degradation note, got %v", notes)
	}
}

func TestLocalIsolationNotesDarwinIsAlwaysEmpty(t *testing.T) {
	if notes := localIsolationNotes(context.Background(), "darwin", true, true); notes != nil {
		t.Fatalf("darwin has no probe-based degradation path, got %v", notes)
	}
}
