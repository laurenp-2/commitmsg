package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildContextFitsBudgetAndKeepsExcludedFilesInSummary(t *testing.T) {
	small := readDiffFixture(t, "small.diff")
	files := []FileChange{
		{
			Path:    "internal/greet/greet.go",
			Added:   6,
			Deleted: 2,
			Diff:    small,
		},
		{
			Path:    "go.sum",
			Added:   200,
			Deleted: 0,
			Diff:    "diff --git a/go.sum b/go.sum\n+generated-lock-sentinel\n",
		},
	}

	const budget = 5_000
	context := BuildContext(files, budget)

	if runeCount(context) > budget {
		t.Fatalf("context is %d characters; want at most %d", runeCount(context), budget)
	}
	if !strings.Contains(context, "- internal/greet/greet.go +6/-2") {
		t.Fatalf("source file missing from summary:\n%s", context)
	}
	if !strings.Contains(context, "- go.sum +200/-0 (excluded)") {
		t.Fatalf("excluded file missing from summary:\n%s", context)
	}
	if !strings.Contains(context, "func Message(name string, excited bool) string") {
		t.Fatalf("included diff was omitted:\n%s", context)
	}
	if strings.Contains(context, "generated-lock-sentinel") {
		t.Fatalf("excluded diff was included:\n%s", context)
	}
}

func TestBuildContextTruncatesAcrossFilesAndIsDeterministic(t *testing.T) {
	manyHunks := readDiffFixture(t, "many-hunks.diff")
	files := []FileChange{
		{Path: "internal/parser/first.go", Added: 10, Deleted: 1, Diff: strings.ReplaceAll(manyHunks, "parser.go", "first.go")},
		{Path: "internal/parser/second.go", Added: 10, Deleted: 1, Diff: strings.ReplaceAll(manyHunks, "parser.go", "second.go")},
		{Path: "internal/parser/third.go", Added: 10, Deleted: 1, Diff: strings.ReplaceAll(manyHunks, "parser.go", "third.go")},
	}

	const budget = 2_300
	first := BuildContext(files, budget)
	second := BuildContext(files, budget)

	if first != second {
		t.Fatalf("BuildContext is not deterministic\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if runeCount(first) > budget {
		t.Fatalf("context is %d characters; want at most %d", runeCount(first), budget)
	}
	if !strings.Contains(first, "... [") || !strings.Contains(first, "more hunks truncated]") {
		t.Fatalf("expected hunk truncation marker:\n%s", first)
	}
	for _, file := range files {
		if !strings.Contains(first, "- "+file.Path+" +10/-1") {
			t.Fatalf("%q missing from summary:\n%s", file.Path, first)
		}
	}
}

func TestTruncatedFileBlockKeepsWholeHunks(t *testing.T) {
	diff := readDiffFixture(t, "many-hunks.diff")
	file := FileChange{Path: "internal/parser/parser.go", Added: 10, Deleted: 1, Diff: diff}
	block := truncatedFileBlock(file, 500)
	_, hunks := splitDiff(diff)

	if !strings.Contains(block, "... [") {
		t.Fatalf("expected a truncation marker:\n%s", block)
	}
	for _, hunk := range hunks {
		hunkHeader := strings.SplitN(hunk, "\n", 2)[0]
		if strings.Contains(block, hunkHeader) && !strings.Contains(block, hunk) {
			t.Fatalf("hunk was cut mid-block:\n%s", block)
		}
	}
}

func TestBuildContextCompactsGiantFiles(t *testing.T) {
	giant := readDiffFixture(t, "giant.diff")
	file := FileChange{
		Path:    "web/assets/dashboard.js",
		Added:   25,
		Deleted: 2,
		Diff:    giant,
	}

	const budget = 600
	context := BuildContext([]FileChange{file}, budget)

	if !isGiantDiff(file, budget) {
		t.Fatal("fixture should exceed 40% of the total budget")
	}
	if !strings.Contains(context, "diff --git a/web/assets/dashboard.js b/web/assets/dashboard.js") {
		t.Fatalf("giant diff header missing:\n%s", context)
	}
	if !strings.Contains(context, "stat: +25/-2") {
		t.Fatalf("giant diff stat missing:\n%s", context)
	}
	if !strings.Contains(context, "[truncated: 27 lines changed]") {
		t.Fatalf("giant diff marker missing:\n%s", context)
	}
	if strings.Contains(context, "visibleItems") {
		t.Fatalf("giant diff contents should not be included:\n%s", context)
	}
}

func TestContextAllocationsDoNotWasteBudgetOnGiantFiles(t *testing.T) {
	giant := contextFile{
		file:  FileChange{Path: "generated/giant.go", Added: 10_000, Diff: readDiffFixture(t, "giant.diff")},
		giant: true,
	}
	normal := contextFile{
		file: FileChange{Path: "internal/meaningful.go", Added: 12, Diff: readDiffFixture(t, "many-hunks.diff")},
	}
	files := []contextFile{giant, normal}

	old := proportionalAllocations(files, 900)
	got := contextAllocations(files, 900)
	if got[0] != runeCount(compactFileBlock(giant.file)) {
		t.Fatalf("giant allocation = %d, want compact size %d", got[0], runeCount(compactFileBlock(giant.file)))
	}
	if got[1] <= old[1] {
		t.Fatalf("normal allocation = %d, want more than wasted-allocation result %d", got[1], old[1])
	}
}

func TestBuildContextWithOnlyExcludedFilesReturnsSummary(t *testing.T) {
	files := []FileChange{
		{Path: "go.sum", Added: 20, Deleted: 1, Diff: "diff --git a/go.sum b/go.sum\n+lock-content\n"},
		{Path: "web/app.min.js", Added: 100, Deleted: 50, Diff: "diff --git a/web/app.min.js b/web/app.min.js\n+minified-content\n"},
		{Path: "assets/logo.png", IsBinary: true, Diff: "binary-content"},
	}

	context := BuildContext(files, 1_000)
	if strings.Contains(context, "Diff:") {
		t.Fatalf("expected no diff section for only excluded files:\n%s", context)
	}
	for _, file := range files {
		if !IsExcluded(file) {
			t.Fatalf("%q should be excluded", file.Path)
		}
		if !strings.Contains(context, file.Path) {
			t.Fatalf("%q missing from summary:\n%s", file.Path, context)
		}
	}
}

func TestIsExcludedMatchesBasenamesAndGeneratedPatterns(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "vendor/package-lock.json", want: true},
		{path: "frontend/yarn.lock", want: true},
		{path: "dist/site.min.css", want: true},
		{path: "dist/site.min.js", want: true},
		{path: "dist/site.js.map", want: true},
		{path: "cmd/commitmsg/main.go", want: false},
		{path: "docs/lockfile.md", want: false},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := IsExcluded(FileChange{Path: test.path}); got != test.want {
				t.Fatalf("IsExcluded(%q) = %t; want %t", test.path, got, test.want)
			}
		})
	}
}

func readDiffFixture(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return string(contents)
}
