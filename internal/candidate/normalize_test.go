package candidate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeUnifiedDiff(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name: "already valid diff unchanged",
			in: `--- a/main.go
+++ b/main.go
@@ -1,2 +1,3 @@ package main
 context
-removed
+added
+new line
`,
			wantChanged: false,
			want: `--- a/main.go
+++ b/main.go
@@ -1,2 +1,3 @@ package main
 context
-removed
+added
+new line
`,
		},
		{
			name: "declared counts too high corrected",
			in: `--- a/f.txt
+++ b/f.txt
@@ -1,10 +1,20 @@
 one
-two
+TWO
`,
			wantChanged: true,
			want: `--- a/f.txt
+++ b/f.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
`,
		},
		{
			name: "trailing garbage truncated",
			in: `--- a/f.txt
+++ b/f.txt
@@ -1,1 +1,3 @@ func
 one
+two
+three
some random garbage that is not a diff line
`,
			wantChanged: true,
			want: `--- a/f.txt
+++ b/f.txt
@@ -1,1 +1,3 @@ func
 one
+two
+three
`,
		},
		{
			name: "empty hunk dropped",
			in: `--- a/f.txt
+++ b/f.txt
@@ -1,5 +1,5 @@
garbage only, no valid body lines
--- a/g.txt
+++ b/g.txt
@@ -1,1 +1,1 @@
-keep
+KEEP
`,
			wantChanged: true,
			want: `--- a/g.txt
+++ b/g.txt
@@ -1,1 +1,1 @@
-keep
+KEEP
`,
		},
		{
			name: "multiple file sections",
			in: `--- a/a.txt
+++ b/a.txt
@@ -1,99 +1,99 @@
-a1
+A1
--- a/b.txt
+++ b/b.txt
@@ -1,1 +1,50 @@
+b1
`,
			wantChanged: true,
			want: `--- a/a.txt
+++ b/a.txt
@@ -1,1 +1,1 @@
-a1
+A1
--- a/b.txt
+++ b/b.txt
@@ -1,0 +1,1 @@
+b1
`,
		},
		{
			name: "no newline marker preserved and uncounted",
			in: `--- a/f.txt
+++ b/f.txt
@@ -1,2 +1,2 @@
 ctx
-old
\ No newline at end of file
+new
\ No newline at end of file
`,
			wantChanged: false,
			want: `--- a/f.txt
+++ b/f.txt
@@ -1,2 +1,2 @@
 ctx
-old
\ No newline at end of file
+new
\ No newline at end of file
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := NormalizeUnifiedDiff(tt.in)
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if got != tt.want {
				t.Fatalf("normalized diff mismatch:\ngot:\n%q\nwant:\n%q", got, tt.want)
			}
			if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
				t.Fatalf("output must end with exactly one newline, got %q", got)
			}
		})
	}
}

func TestNormalizeUnifiedDiffNoHunks(t *testing.T) {
	in := "just some text\nwithout any hunk headers\n"
	got, changed := NormalizeUnifiedDiff(in)
	if changed || got != in {
		t.Fatalf("expected unchanged input, got %q changed=%v", got, changed)
	}
}

// End-to-end sanity: normalize a count-corrupted diff and confirm git apply
// --check accepts it against a small real git repo.
func TestNormalizeThenGitApplyCheck(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")

	corrupt := "--- a/f.txt\n+++ b/f.txt\n@@ -1,9 +1,9 @@\n alpha\n-beta\n+BETA\n gamma\n"
	normalized, changed := NormalizeUnifiedDiff(corrupt)
	if !changed {
		t.Fatal("expected normalization to change corrupted diff")
	}
	patchPath := filepath.Join(t.TempDir(), "fix.diff")
	if err := os.WriteFile(patchPath, []byte(normalized), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", "--check", patchPath)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply --check failed on normalized diff: %v: %s", err, out)
	}

	// Sanity: the original corrupt diff must be rejected.
	badPath := filepath.Join(t.TempDir(), "bad.diff")
	if err := os.WriteFile(badPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "apply", "--check", badPath)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected git apply --check to reject corrupt diff, output: %s", out)
	}
}
