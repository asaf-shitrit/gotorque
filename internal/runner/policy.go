package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// EnvironmentPolicy lists which environment variable names a workload
// process may receive. Names in Allow or Passthrough are copied from
// gotorque's own process environment when present, and Allow additionally
// gates which keys of RunRequest.AdditionalEnv (values gotorque supplies
// itself, such as build configuration) are kept. Every other variable in the
// harness's own environment -- including model provider credentials such as
// OPENROUTER_API_KEY or AI_GATEWAY_API_KEY -- never reaches the target
// process. An empty policy grants nothing beyond the sandbox's fixed base
// (HOME/TMPDIR) and gotorque's own infrastructure variables.
type EnvironmentPolicy struct {
	Allow       []string
	Passthrough []string
}

// ResourceLimits is the manifest's max_processes/max_memory_bytes translated
// into rlimits. Zero means "no limit requested".
type ResourceLimits struct {
	MaxProcesses   int
	MaxMemoryBytes int64
}

// SandboxPolicy is the target manifest's sandbox block translated into the
// primitives the runner enforces. It is set once per Runner -- one manifest
// governs a whole campaign -- so a baseline and a candidate run under
// identical policy, which keeps an A/B comparison fair.
type SandboxPolicy struct {
	// NetworkAllowed is sandbox.network == "allow". Deny is the default.
	NetworkAllowed bool
	// WriteScope is sandbox.filesystem.write: "temp_only", "manifest_paths",
	// or "any". Only "any" changes enforcement today; the other two both
	// restrict writes to the run's sandbox directory (see writeScopeNotes).
	WriteScope string
	// ReadScope is sandbox.filesystem.read, recorded for isolation notes;
	// see readScopeNotes for what is and is not enforced.
	ReadScope   string
	Environment EnvironmentPolicy
	Limits      ResourceLimits
}

func (p SandboxPolicy) filesystemUnrestricted() bool {
	return p.WriteScope == "any"
}

// infrastructureEnv lists variables the runner itself sets for every run
// regardless of manifest policy, because they configure the Go toolchain
// gotorque controls rather than anything a target manifest is meant to gate.
// None of them is a secret.
var infrastructureEnv = map[string]bool{"GOTOOLCHAIN": true}

// BuildEnv assembles a workload's environment from a fixed base (typically
// the sandbox's HOME/TMPDIR block), the manifest's allowed/passthrough
// variables copied from gotorque's own process environment, and any
// gotorque-supplied additional variables the policy allows. It is exported
// so internal/profile's direct-sampling path can apply the same policy the
// sandboxed runner does.
func BuildEnv(policy EnvironmentPolicy, base []string, additional map[string]string) []string {
	// base's own keys (HOME/TMPDIR/TMP/TEMP, pointed at the sandbox
	// directory) win over a same-named passthrough variable: a manifest
	// that lists HOME in environment.allow -- as targets/gron's does -- is
	// declaring HOME non-secret, not asking to replace the sandbox's own
	// home directory with gotorque's.
	reserved := envKeys(base)
	env := append([]string{}, base...)
	for _, name := range passthroughNames(policy) {
		if reserved[name] {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, mapEnvironment(filterAdditionalEnv(policy, additional))...)
}

func envKeys(env []string) map[string]bool {
	keys := make(map[string]bool, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			keys[kv[:i]] = true
		}
	}
	return keys
}

func passthroughNames(policy EnvironmentPolicy) []string {
	seen := map[string]bool{}
	var names []string
	for _, name := range append(append([]string{}, policy.Allow...), policy.Passthrough...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func filterAdditionalEnv(policy EnvironmentPolicy, additional map[string]string) map[string]string {
	if len(additional) == 0 {
		return nil
	}
	allow := toSet(policy.Allow)
	out := make(map[string]string, len(additional))
	for k, v := range additional {
		if infrastructureEnv[k] || allow[k] {
			out[k] = v
		}
	}
	return out
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// WrapWithResourceLimits wraps a command so it runs under the requested
// rlimits, applied by a shell before it execs into path so the limits are
// inherited by any local-isolation wrapper (sandbox-exec, bwrap) between the
// shell and the target: rlimits survive exec, and none of those wrappers
// resets them. It returns notes describing anything it could not enforce, in
// the same style as the local-isolation degradation notes: a requested limit
// that cannot be honored on this platform is recorded, never silently
// dropped.
func WrapWithResourceLimits(ctx context.Context, limits ResourceLimits, path string, args []string) (string, []string, []string) {
	procClause, procNote := processLimitClause(ctx, limits.MaxProcesses)
	memClause, memNote := memoryLimitClause(ctx, runtime.GOOS, limits.MaxMemoryBytes)
	notes := appendNonEmpty(nil, procNote, memNote)
	clauses := appendNonEmpty(nil, procClause, memClause)
	if len(clauses) == 0 {
		return path, args, notes
	}
	script := strings.Join(clauses, "; ") + `; exec "$0" "$@"`
	wrapped := append([]string{"-c", script, path}, args...)
	return resourceLimitShell, wrapped, notes
}

func appendNonEmpty(dst []string, values ...string) []string {
	for _, v := range values {
		if v != "" {
			dst = append(dst, v)
		}
	}
	return dst
}

// resourceLimitShell must be bash, not /bin/sh: ulimit -v (address space) is
// a bash extension that POSIX/dash sh does not implement, and it is the only
// portable way to bound process memory without a container runtime.
const resourceLimitShell = "/bin/bash"

func processLimitClause(ctx context.Context, n int) (clause, note string) {
	if n <= 0 {
		return "", ""
	}
	if !processLimitSupportedFn(ctx) {
		return "", fmt.Sprintf("sandbox.max_processes=%d requested but RLIMIT_NPROC could not be set in this environment; not enforced for this run", n)
	}
	return fmt.Sprintf("ulimit -u %d 2>/dev/null", n),
		fmt.Sprintf("sandbox.max_processes=%d enforced via RLIMIT_NPROC (ulimit -u), which Unix scopes per user account rather than per process tree; concurrent work under the same account shares this bound", n)
}

func memoryLimitClause(ctx context.Context, goos string, bytes int64) (clause, note string) {
	if bytes <= 0 {
		return "", ""
	}
	if goos != "linux" {
		return "", fmt.Sprintf("sandbox.max_memory_bytes=%d requested but no virtual-memory rlimit is enforceable for an unprivileged process on %s; not enforced for this run", bytes, goos)
	}
	if !memoryLimitSupportedFn(ctx) {
		return "", fmt.Sprintf("sandbox.max_memory_bytes=%d requested but RLIMIT_AS could not be set in this environment; not enforced for this run", bytes)
	}
	kib := bytes / 1024
	if kib < 1 {
		kib = 1
	}
	return fmt.Sprintf("ulimit -v %d 2>/dev/null", kib),
		fmt.Sprintf("sandbox.max_memory_bytes=%d enforced via RLIMIT_AS (ulimit -v)", bytes)
}

// processLimitSupportedFn/memoryLimitSupportedFn are package variables (not
// plain functions) so tests can force the unsupported path without
// depending on the real host's rlimit behavior, the same technique already
// used for the bwrap capability probes below.
var (
	processLimitSupportedFn = probeProcessLimit
	memoryLimitSupportedFn  = probeMemoryLimit
)

var (
	procLimitProbeOnce sync.Once
	procLimitProbeOK   bool
)

func probeProcessLimit(ctx context.Context) bool {
	procLimitProbeOnce.Do(func() {
		cmd := exec.CommandContext(ctx, resourceLimitShell, "-c", "ulimit -u 256 2>/dev/null")
		procLimitProbeOK = cmd.Run() == nil
	})
	return procLimitProbeOK
}

var (
	memLimitProbeOnce sync.Once
	memLimitProbeOK   bool
)

func probeMemoryLimit(ctx context.Context) bool {
	memLimitProbeOnce.Do(func() {
		cmd := exec.CommandContext(ctx, resourceLimitShell, "-c", "ulimit -v 1048576 2>/dev/null")
		memLimitProbeOK = cmd.Run() == nil
	})
	return memLimitProbeOK
}

// resetResourceLimitProbes clears the process-wide capability caches;
// exported for tests in this package only via internal visibility.
func resetResourceLimitProbes() {
	procLimitProbeOnce = sync.Once{}
	procLimitProbeOK = false
	memLimitProbeOnce = sync.Once{}
	memLimitProbeOK = false
}

// localIsolationNotes explains when local isolation (bubblewrap on Linux)
// could not fully honor a requested restriction, mirroring the probe-based
// degradation isolatedCommandFor already performs silently. macOS's
// sandbox-exec has no equivalent degrade path: it enforces what its profile
// says or NewSandbox's caller sees an exec error, so there is nothing to
// report here for darwin.
func localIsolationNotes(ctx context.Context, goos string, denyNetwork, writeRestricted bool) []string {
	if goos != "linux" || (!denyNetwork && !writeRestricted) {
		return nil
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return degradedNotes(denyNetwork, writeRestricted, "bubblewrap is not installed; this run was not isolated")
	}
	if !bwrapIsolationSupported(ctx, bwrap, "", "") {
		return degradedNotes(denyNetwork, writeRestricted, "bubblewrap could not set up its sandbox in this environment; this run was not isolated")
	}
	if denyNetwork && !bwrapNetNamespaceSupported(ctx, bwrap) {
		return []string{"sandbox.network=deny requested but bubblewrap could not create a network namespace in this environment; this run kept filesystem isolation but network access was not blocked"}
	}
	return nil
}

func degradedNotes(denyNetwork, writeRestricted bool, reason string) []string {
	var notes []string
	if denyNetwork {
		notes = append(notes, "sandbox.network=deny requested but "+reason)
	}
	if writeRestricted {
		notes = append(notes, "sandbox.filesystem.write restriction requested but "+reason)
	}
	return notes
}

// readScopeNotes and writeScopeNotes make visible the gap between what a
// manifest's filesystem scope asks for and what gotorque currently
// enforces: neither scope carries a path list in the manifest schema today,
// so "manifest_paths" and "none" cannot be told apart from the broader
// access gotorque actually grants.
func readScopeNotes(policy SandboxPolicy) []string {
	switch policy.ReadScope {
	case "manifest_paths":
		return []string{"sandbox.filesystem.read=manifest_paths requested but gotorque does not read a manifest-declared path list; broad read access was granted instead, the same as repo_and_assets"}
	case "none":
		return []string{"sandbox.filesystem.read=none requested but gotorque cannot deny all reads without breaking process and library loading; broad read access was granted instead"}
	default:
		return nil
	}
}

func writeScopeNotes(policy SandboxPolicy) []string {
	if policy.WriteScope == "manifest_paths" {
		return []string{"sandbox.filesystem.write=manifest_paths requested but gotorque does not read a manifest-declared path list; writes were restricted to the run's temporary sandbox directory instead, the same as temp_only"}
	}
	return nil
}
