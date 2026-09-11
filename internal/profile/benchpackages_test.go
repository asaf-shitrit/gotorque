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

func TestBenchmarkPackagesBreaksTiesLexicographically(t *testing.T) {
	root := t.TempDir()
	// Six one-benchmark packages tie. Map iteration order is random, so an
	// unordered tie-break would almost never reproduce the sorted list.
	files := map[string]string{"most/m_test.go": "package most\n\nfunc BenchmarkA(b *testing.B) {}\nfunc BenchmarkB(b *testing.B) {}\n"}
	for _, pkg := range []string{"f", "b", "d", "a", "e", "c"} {
		files[pkg+"/x_test.go"] = "package " + pkg + "\n\nfunc BenchmarkX(b *testing.B) {}\n"
	}
	writeTree(t, root, files)

	got := BenchmarkPackages(root)
	want := []string{"./most", "./a", "./b", "./c", "./d", "./e", "./f"}
	if !slices.Equal(got, want) {
		t.Errorf("BenchmarkPackages() = %v, want %v", got, want)
	}
}

func TestBenchmarkPackagesSkipsNonTestAndUnreadableFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"counted/counted_test.go": "package counted\n\nfunc BenchmarkReal(b *testing.B) {}\n",
		// Benchmark-shaped declarations outside _test.go files are not benchmarks.
		"lib/bench.go": "package lib\n\nfunc BenchmarkFake(b *testing.B) {}\n",
	})
	// A dangling symlink named like a test file fails to read and is skipped.
	if err := os.MkdirAll(filepath.Join(root, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken", "broken_test.go")); err != nil {
		t.Fatal(err)
	}

	got := BenchmarkPackages(root)
	want := []string{"./counted"}
	if !slices.Equal(got, want) {
		t.Errorf("BenchmarkPackages() = %v, want %v", got, want)
	}
}

func TestBenchmarkPackagesMissingRoot(t *testing.T) {
	if got := BenchmarkPackages(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Errorf("BenchmarkPackages() = %v, want empty", got)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
