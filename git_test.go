package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	input := []byte("12\t3\tinternal/with spaces/file name.go\x00-\t-\tassets/logo.bin\x001\t0\tplain.go\x00")

	got, err := parseNumstat(input)
	if err != nil {
		t.Fatalf("parseNumstat() error = %v", err)
	}

	want := []FileChange{
		{Path: "internal/with spaces/file name.go", Added: 12, Deleted: 3},
		{Path: "assets/logo.bin", IsBinary: true},
		{Path: "plain.go", Added: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNumstat() = %#v, want %#v", got, want)
	}
}

func TestParseNumstatRenameUsesNewPath(t *testing.T) {
	// git diff --numstat -z writes a blank path, followed by old and new paths,
	// for rename/copy entries.
	input := []byte("4\t2\t\x00old name.go\x00new name.go\x00")

	got, err := parseNumstat(input)
	if err != nil {
		t.Fatalf("parseNumstat() error = %v", err)
	}

	want := []FileChange{{Path: "new name.go", Added: 4, Deleted: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNumstat() = %#v, want %#v", got, want)
	}
}

func TestHooksDirectoryUsesRelativeCoreHooksPath(t *testing.T) {
	repoRoot := t.TempDir()
	runGitForTest(t, repoRoot, "init", "--quiet")
	runGitForTest(t, repoRoot, "config", "core.hooksPath", ".githooks")

	got, err := HooksDirectory(repoRoot)
	if err != nil {
		t.Fatalf("HooksDirectory() error = %v", err)
	}

	want := filepath.Join(repoRoot, ".githooks")
	if got != want {
		t.Errorf("HooksDirectory() = %q, want %q", got, want)
	}
}

func TestCurrentBranchReturnsEmptyForDetachedHead(t *testing.T) {
	repoRoot := t.TempDir()
	runGitForTest(t, repoRoot, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repoRoot, "file.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGitForTest(t, repoRoot, "add", "file.txt")
	runGitForTest(t, repoRoot, "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")
	runGitForTest(t, repoRoot, "checkout", "--quiet", "--detach")

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get current directory: %v", err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("change directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	branch, err := CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch() error = %v", err)
	}
	if branch != "" {
		t.Fatalf("CurrentBranch() = %q, want empty for detached HEAD", branch)
	}
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
