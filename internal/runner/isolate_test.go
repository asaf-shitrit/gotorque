package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// resetBwrapProbes clears the process-wide capability probes so a test can
// control whether this environment appears to support bubblewrap.
func resetBwrapProbes() {
	bwrapProbeOnce = sync.Once{}
	bwrapProbeOK = false
	bwrapNetNamespaceOnce = sync.Once{}
	bwrapNetNamespaceOK = false
}

// writeFakeBwrap puts a shell script named bwrap on PATH. exitCode and
// netExitCode select the exit status of the isolation probe and the network
// namespace probe respectively. Every invocation appends its arguments to
// logPath so tests can assert which probes ran.
func writeFakeBwrap(t *testing.T, logPath, exitCode, netExitCode string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "bwrap")
	content := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = \"--unshare-net\" ]; then exit " + netExitCode + "; fi\n" +
		"exit " + exitCode + "\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil { //nolint:gosec // fake bwrap must be executable to stand in for the real binary
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return script
}

func TestIsolatedCommandForDarwin(t *testing.T) {
	tests := []struct {
		name        string
		root        string
		denyNetwork bool
		wantProfile string
	}{
		{
			name:        "writes are restricted to the sandbox root",
			root:        "/sandbox/root",
			wantProfile: `(version 1)(allow default)(deny file-write*)(allow file-write* (subpath "/sandbox/root"))`,
		},
		{
			name:        "network denial is appended when requested",
			root:        "/sandbox/root",
			denyNetwork: true,
			wantProfile: `(version 1)(allow default)(deny file-write*)(allow file-write* (subpath "/sandbox/root"))(deny network*)`,
		},
		{
			name:        "quotes in the sandbox root are escaped",
			root:        `/tmp/od"d`,
			wantProfile: `(version 1)(allow default)(deny file-write*)(allow file-write* (subpath "/tmp/od\"d"))`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--input", "sample"}
			gotPath, gotArgs, err := isolatedCommandFor(context.Background(), "darwin", tt.root, filepath.Join(tt.root, "work"), tt.denyNetwork, "/bins/app", args)
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != "/usr/bin/sandbox-exec" {
				t.Fatalf("path = %q, want /usr/bin/sandbox-exec", gotPath)
			}
			want := append([]string{"-p", tt.wantProfile, "/bins/app"}, args...)
			if !reflect.DeepEqual(gotArgs, want) {
				t.Fatalf("args = %#v, want %#v", gotArgs, want)
			}
			if !reflect.DeepEqual(args, []string{"--input", "sample"}) {
				t.Fatalf("caller arguments were mutated: %#v", args)
			}
		})
	}
}

func TestIsolatedCommandForLinuxUsesBubblewrap(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	binary := "/bins/app"
	callerArgs := []string{"--input", "sample"}

	t.Run("filesystem-only when network is allowed", func(t *testing.T) {
		assertFilesystemOnlyIsolation(t, root, workDir, binary, callerArgs)
	})
	t.Run("adds a network namespace when network is denied", func(t *testing.T) {
		assertDeniedNetworkAddsNamespace(t, root, workDir, binary, callerArgs)
	})
	t.Run("degrades to filesystem-only when the network probe fails", func(t *testing.T) {
		assertFailedNetworkProbeDegrades(t, root, workDir, binary, callerArgs)
	})
	t.Run("degrades to unwrapped when the isolation probe fails", func(t *testing.T) {
		assertFailedIsolationProbeDegrades(t, root, workDir, binary, callerArgs)
	})
	t.Run("fails when bubblewrap is missing", func(t *testing.T) {
		assertMissingBubblewrapFails(t, root, workDir, binary, callerArgs)
	})
}

func assertFilesystemOnlyIsolation(t *testing.T, root, workDir, binary string, callerArgs []string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "probe.log")
	bwrap := writeFakeBwrap(t, logPath, "0", "0")
	resetBwrapProbes()

	gotPath, gotArgs, err := isolatedCommandFor(context.Background(), "linux", root, workDir, false, binary, callerArgs)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != bwrap {
		t.Fatalf("path = %q, want %q", gotPath, bwrap)
	}
	want := append([]string{"--die-with-parent", "--ro-bind", "/", "/", "--bind", root, root, "--chdir", workDir, binary}, callerArgs...)
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
	// The network namespace probe must not run when network is allowed.
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logged), "\n") != 1 || !strings.Contains(string(logged), "--die-with-parent") {
		t.Fatalf("probe log = %q, want only the isolation probe", logged)
	}
}

func assertDeniedNetworkAddsNamespace(t *testing.T, root, workDir, binary string, callerArgs []string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "probe.log")
	bwrap := writeFakeBwrap(t, logPath, "0", "0")
	resetBwrapProbes()

	gotPath, gotArgs, err := isolatedCommandFor(context.Background(), "linux", root, workDir, true, binary, callerArgs)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != bwrap {
		t.Fatalf("path = %q, want %q", gotPath, bwrap)
	}
	want := append([]string{"--die-with-parent", "--unshare-net", "--ro-bind", "/", "/", "--bind", root, root, "--chdir", workDir, binary}, callerArgs...)
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "--unshare-net") {
		t.Fatalf("probe log = %q, want a network namespace probe", logged)
	}
}

func assertFailedNetworkProbeDegrades(t *testing.T, root, workDir, binary string, callerArgs []string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "probe.log")
	bwrap := writeFakeBwrap(t, logPath, "0", "1")
	resetBwrapProbes()

	gotPath, gotArgs, err := isolatedCommandFor(context.Background(), "linux", root, workDir, true, binary, callerArgs)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != bwrap {
		t.Fatalf("path = %q, want %q", gotPath, bwrap)
	}
	want := append([]string{"--die-with-parent", "--ro-bind", "/", "/", "--bind", root, root, "--chdir", workDir, binary}, callerArgs...)
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v (no network namespace expected)", gotArgs, want)
	}
}

func assertFailedIsolationProbeDegrades(t *testing.T, root, workDir, binary string, callerArgs []string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "probe.log")
	writeFakeBwrap(t, logPath, "1", "1")
	resetBwrapProbes()

	gotPath, gotArgs, err := isolatedCommandFor(context.Background(), "linux", root, workDir, true, binary, callerArgs)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != binary || !reflect.DeepEqual(gotArgs, callerArgs) {
		t.Fatalf("got (%q, %#v), want the unwrapped (%q, %#v)", gotPath, gotArgs, binary, callerArgs)
	}
}

func assertMissingBubblewrapFails(t *testing.T, root, workDir, binary string, callerArgs []string) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	_, _, err := isolatedCommandFor(context.Background(), "linux", root, workDir, true, binary, callerArgs)
	if err == nil || !strings.Contains(err.Error(), "bubblewrap") {
		t.Fatalf("err = %v, want a missing bubblewrap error", err)
	}
}

func TestIsolatedCommandForUnsupportedPlatform(t *testing.T) {
	_, _, err := isolatedCommandFor(context.Background(), "plan9", "/root", "/root/work", true, "/bins/app", nil)
	if err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("err = %v, want an unsupported platform error naming plan9", err)
	}
}

func TestIsolatedCommandDelegatesToHostPlatform(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	logPath := filepath.Join(t.TempDir(), "probe.log")
	writeFakeBwrap(t, logPath, "0", "0")
	resetBwrapProbes()

	gotPath, gotArgs, gotErr := isolatedCommand(context.Background(), root, workDir, true, "/bins/app", []string{"--input", "sample"})
	wantPath, wantArgs, wantErr := isolatedCommandFor(context.Background(), runtime.GOOS, root, workDir, true, "/bins/app", []string{"--input", "sample"})
	if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
		t.Fatalf("err = %v, want %v", gotErr, wantErr)
	}
	if gotPath != wantPath || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("got (%q, %#v), want (%q, %#v)", gotPath, gotArgs, wantPath, wantArgs)
	}
}
