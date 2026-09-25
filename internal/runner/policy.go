package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
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
// memory rlimit, applied by a shell before it execs into path so the limit
// is inherited by any local-isolation wrapper (sandbox-exec, bwrap) between
// the shell and the target: rlimits survive exec, and none of those
// wrappers resets them. It returns notes describing anything it could not
// enforce, in the same style as the local-isolation degradation notes: a
// requested limit that cannot be honored on this platform is recorded,
// never silently dropped.
//
// sandbox.max_processes is never enforced here (see processLimitNote): on
// Linux RLIMIT_NPROC is scoped per user account rather than per process
// tree and counts threads, so `ulimit -u` bounds the whole account's thread
// count, not this run's process tree. Verified in a Debian container as an
// unprivileged user: `bash -c 'ulimit -u 1; exec ./gobin'` crashes every Go
// binary with `runtime: failed to create new OS thread ... fatal error:
// newosproc`, because the Go runtime's own worker threads count against the
// same limit as every other process on the account.
//
// When no clause is produced -- which is always true on a platform other
// than Linux, since memory is the only rlimit this function ever wraps --
// path and args are returned unchanged: callers that attach to a live PID
// immediately after starting it (the macOS sampler) depend on that, because
// a shell wrapper delays the target's exec past the moment the sampler
// attaches.
func WrapWithResourceLimits(ctx context.Context, limits ResourceLimits, path string, args []string) (string, []string, []string) {
	procNote := processLimitNote(limits.MaxProcesses)
	memClause, memNote := memoryLimitClause(ctx, runtime.GOOS, limits.MaxMemoryBytes)
	notes := appendNonEmpty(nil, procNote, memNote)
	if memClause == "" {
		return path, args, notes
	}
	script := memClause + `; exec "$0" "$@"`
	wrapped := append([]string{"-c", script, path}, args...)
	return resourceLimitShell, wrapped, notes
}

// ProfilingResourceLimitNotes describes what a manifest's resource limits
// ask for without wrapping a command in a memory rlimit. The Linux sampler
// (perf record) must attach directly to the target's own process rather than
// running it under a shell wrapper's rlimit, so a requested memory limit is
// recorded here as not enforced while profiling instead of silently applied
// or silently dropped.
func ProfilingResourceLimitNotes(limits ResourceLimits) []string {
	notes := appendNonEmpty(nil, processLimitNote(limits.MaxProcesses))
	if limits.MaxMemoryBytes > 0 {
		notes = append(notes, fmt.Sprintf("sandbox.max_memory_bytes=%d requested but not enforced while profiling: perf record runs the target unwrapped, so a candidate's peak memory during a Linux profiling sample is not bounded", limits.MaxMemoryBytes))
	}
	return notes
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

// resourceLimitExitCode is what the shell built by memoryLimitClause exits
// with when `ulimit -S -v` itself fails, before the target ever execs. A
// run that exits with this code never ran the workload at all, so callers
// must not confuse it with a workload exit code; see
// isResourceLimitFailure.
const resourceLimitExitCode = 125

// processLimitNote explains why sandbox.max_processes is never translated
// into an rlimit, on any platform: RLIMIT_NPROC is scoped per user account,
// not per process tree, and on Linux it counts threads rather than
// processes. Bounding it to the manifest's small values (every manifest in
// targets/ sets 1) crashes the Go runtime itself, which starts more than
// one OS thread before main runs -- verified in a Debian container as an
// unprivileged user: `bash -c 'ulimit -u 1; exec ./gobin'` fails every time
// with `runtime: failed to create new OS thread ... fatal error:
// newosproc`. There is no unprivileged mechanism that bounds one run's
// process tree without also risking the harness's own process, so the
// request is recorded and left unenforced instead.
func processLimitNote(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("sandbox.max_processes=%d requested but not enforced: RLIMIT_NPROC is scoped per user account (not per process tree) and on Linux it counts threads, so bounding it can crash any multi-threaded process -- including the Go runtime itself -- without bounding this run's process tree; not enforced on any platform", n)
}

// memoryLimitClause builds the shell clause that bounds a Linux run's
// address space, and fails loudly rather than silently running unbounded:
// a `2>/dev/null`-suppressed `ulimit -v` that fails leaves the target
// running with no limit at all and no sign anything went wrong. `-S` sets
// the soft limit only, so the shell that applies it (not just the exec'd
// target) can still raise it back up to the hard limit if something needs
// to; `|| exit resourceLimitExitCode` turns a failed ulimit into a distinct,
// recognizable exit code instead of a silently-unbounded run.
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
	return fmt.Sprintf("ulimit -S -v %d || exit %d", kib, resourceLimitExitCode),
		fmt.Sprintf("sandbox.max_memory_bytes=%d enforced via the soft RLIMIT_AS (ulimit -S -v)", bytes)
}

// memoryLimitSupportedFn is a package variable (not a plain function) so
// tests can force the unsupported path without depending on the real
// host's rlimit behavior, the same technique already used for the bwrap
// capability probes below.
var memoryLimitSupportedFn = probeMemoryLimit

var (
	memLimitProbeOnce sync.Once
	memLimitProbeOK   bool
)

// probeMemoryLimit runs its capability check against its own short-lived
// context.Background() derivative rather than the caller's ctx: this probe
// is cached once for the process (sync.Once), so if it ran under the first
// caller's ctx, that caller cancelling or timing out its context while the
// probe command is still starting would poison the cached result for every
// later caller, on an unrelated request.
func probeMemoryLimit(context.Context) bool {
	memLimitProbeOnce.Do(func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, resourceLimitShell, "-c", "ulimit -S -v 1048576 2>/dev/null")
		memLimitProbeOK = cmd.Run() == nil
	})
	return memLimitProbeOK
}

// resetResourceLimitProbes clears the process-wide capability cache;
// exported for tests in this package only via internal visibility.
func resetResourceLimitProbes() {
	memLimitProbeOnce = sync.Once{}
	memLimitProbeOK = false
}

// IsResourceLimitFailure reports whether a run's command path and exit code
// indicate that WrapWithResourceLimits's memory-rlimit shell failed before
// the workload ever started, rather than the workload itself exiting with
// this code. Runner.Run uses it to turn that failure into a clear error
// instead of reporting it as an ordinary (and coincidentally identical)
// workload exit code.
func IsResourceLimitFailure(commandPath string, exitCode int) bool {
	return commandPath == resourceLimitShell && exitCode == resourceLimitExitCode
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
