package profile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBenchmarkPackagesRanksByBenchmarkCount(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Root declares two benchmarks, ./eval one, ./cmd/tool none.
	write("query_test.go", "package q\n\nfunc BenchmarkRun(b *testing.B) {}\nfunc BenchmarkParse(b *testing.B) {}\n")
	write("eval/eval_test.go", "package eval\n\nfunc BenchmarkEval(b *testing.B) {}\n")
	write("cmd/tool/main_test.go", "package main\n\nfunc TestMain(t *testing.T) {}\n")
	write("vendor/dep/dep_test.go", "package dep\n\nfunc BenchmarkVendored(b *testing.B) {}\n")

	got := BenchmarkPackages(root)
	want := []string{".", "./eval"}
	if !slices.Equal(got, want) {
		t.Errorf("BenchmarkPackages() = %v, want %v", got, want)
	}
}

func TestBenchmarkPackagesEmptyWithoutBenchmarks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main_test.go"), []byte("package main\n\nfunc TestX(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := BenchmarkPackages(root); len(got) != 0 {
		t.Errorf("BenchmarkPackages() = %v, want empty", got)
	}
}
