package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstallAndUninstallHook(t *testing.T) {
	repo := setupGitRepository(t)
	chdirForTest(t, repo)

	if err := InstallHook(false); err != nil {
		t.Fatalf("InstallHook() error = %v", err)
	}
	path := filepath.Join(repo, ".git", "hooks", hookName)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}
	if got, want := string(contents), hookScript; got != want {
		t.Fatalf("hook contents = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed hook: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("hook mode = %o, want 755", info.Mode().Perm())
	}

	if err := UninstallHook(); err != nil {
		t.Fatalf("UninstallHook() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hook exists after uninstall; stat error = %v", err)
	}
}

func TestInstallDoesNotReplaceAnotherHookWithoutForce(t *testing.T) {
	repo := setupGitRepository(t)
	chdirForTest(t, repo)
	path := filepath.Join(repo, ".git", "hooks", hookName)
	writeTestFile(t, path, "#!/bin/sh\necho another hook\n")

	if err := InstallHook(false); !errors.Is(err, errHookExists) {
		t.Fatalf("InstallHook(false) error = %v, want errHookExists", err)
	}
	if err := InstallHook(true); err != nil {
		t.Fatalf("InstallHook(true) error = %v", err)
	}
}

func TestRunHookPrependsMessageAndPreservesGitComments(t *testing.T) {
	repo := setupGitRepository(t)
	writeTestFile(t, filepath.Join(repo, "main.go"), "package demo\n")
	runGitCommand(t, repo, "add", "main.go")
	chdirForTest(t, repo)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/generate":
			_, _ = w.Write([]byte(`{"response":"feat: add hook suggestions"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	t.Setenv("COMMITMSG_MODEL", "test-model")

	messagePath := filepath.Join(repo, "COMMIT_EDITMSG")
	original := "# Please enter the commit message for your changes.\n#\n# staged: main.go\n"
	writeTestFile(t, messagePath, original)
	var stderr bytes.Buffer
	if code := RunHook([]string{messagePath, ""}, &stderr); code != 0 {
		t.Fatalf("RunHook() exit = %d", code)
	}
	contents, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read commit-message file: %v", err)
	}
	if got, want := string(contents), "feat: add hook suggestions\n\n"+original; got != want {
		t.Fatalf("commit-message contents = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("hook wrote stderr without debug: %q", stderr.String())
	}
}

func TestRunHookLeavesSuppliedMessageUntouched(t *testing.T) {
	messagePath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	original := "fix: supplied by user\n\n# comments remain\n"
	writeTestFile(t, messagePath, original)
	var stderr bytes.Buffer
	if code := RunHook([]string{messagePath, "message"}, &stderr); code != 0 {
		t.Fatalf("RunHook() exit = %d", code)
	}
	contents, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read commit-message file: %v", err)
	}
	if got := string(contents); got != original {
		t.Fatalf("commit message changed: %q", got)
	}

	if code := RunHook([]string{messagePath, ""}, &stderr); code != 0 {
		t.Fatalf("RunHook() exit = %d", code)
	}
	contents, err = os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read commit-message file: %v", err)
	}
	if got := string(contents); got != original {
		t.Fatalf("existing message changed: %q", got)
	}
	if strings.Contains(stderr.String(), "commitmsg:") {
		t.Fatalf("hook wrote debug output without COMMITMSG_DEBUG: %q", stderr.String())
	}
}

func TestRunHookIgnoresUnrecognizedSource(t *testing.T) {
	messagePath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	original := "# status\n"
	writeTestFile(t, messagePath, original)
	t.Setenv("COMMITMSG_DEBUG", "1")

	var stderr bytes.Buffer
	if code := RunHook([]string{messagePath, "unexpected-source"}, &stderr); code != 0 {
		t.Fatalf("RunHook() exit = %d", code)
	}
	contents, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read commit-message file: %v", err)
	}
	if got := string(contents); got != original {
		t.Fatalf("commit message changed: %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("hook should not log for an ignored source: %q", stderr.String())
	}
}

func TestRunHookUsesOneOverallTimeout(t *testing.T) {
	repo := setupGitRepository(t)
	writeTestFile(t, filepath.Join(repo, "main.go"), "package demo\n")
	runGitCommand(t, repo, "add", "main.go")
	chdirForTest(t, repo)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			time.Sleep(120 * time.Millisecond)
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/generate":
			select {
			case <-time.After(time.Second):
				_, _ = w.Write([]byte(`{"response":"feat: should time out"}`))
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	t.Setenv("COMMITMSG_MODEL", "test-model")
	t.Setenv("COMMITMSG_TIMEOUT", "240ms")

	messagePath := filepath.Join(repo, "COMMIT_EDITMSG")
	original := "# Git status\n"
	writeTestFile(t, messagePath, original)
	started := time.Now()
	if code := RunHook([]string{messagePath, ""}, io.Discard); code != 0 {
		t.Fatalf("RunHook() exit = %d", code)
	}
	if elapsed := time.Since(started); elapsed > 330*time.Millisecond {
		t.Fatalf("RunHook() took %s; hook timeout should apply across health and generation", elapsed)
	}
	contents, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read commit-message file: %v", err)
	}
	if got := string(contents); got != original {
		t.Fatalf("timed-out hook changed message: %q", got)
	}
}

func TestInstalledHookGeneratesCommitMessageAndBypassesDashM(t *testing.T) {
	repo := setupGitRepository(t)
	runGitCommand(t, repo, "config", "user.name", "Commitmsg Test")
	runGitCommand(t, repo, "config", "user.email", "commitmsg-test@example.com")
	// Keep this test independent of a developer's global core.hooksPath.
	runGitCommand(t, repo, "config", "core.hooksPath", filepath.Join(repo, ".git", "hooks"))

	var modelChecks, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			modelChecks.Add(1)
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/generate":
			generations.Add(1)
			_, _ = w.Write([]byte(`{"response":"feat: generate an installed hook message"}`))
		default:
			t.Errorf("unexpected Ollama request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "commitmsg")
	buildTestCommitmsgBinary(t, binaryPath)
	env := testCommandEnvironment(map[string]string{
		"COMMITMSG_MODEL":   "test-model",
		"COMMITMSG_TIMEOUT": "5s",
		"GIT_EDITOR":        "true",
		"OLLAMA_HOST":       server.URL,
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	install := exec.Command(binaryPath, "install")
	install.Dir = repo
	install.Env = env
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("commitmsg install: %v\n%s", err, output)
	} else if got, want := strings.TrimSpace(string(output)), "installed prepare-commit-msg hook"; got != want {
		t.Fatalf("commitmsg install output = %q, want %q", got, want)
	}

	writeTestFile(t, filepath.Join(repo, "generated.go"), "package generated\n")
	runGitCommand(t, repo, "add", "generated.go")
	runGitCommandWithEnvironment(t, repo, env, "commit", "--no-gpg-sign")
	if got, want := strings.TrimSpace(string(runGitCommandWithEnvironment(t, repo, env, "log", "-1", "--format=%B"))), "feat: generate an installed hook message"; got != want {
		t.Fatalf("generated commit message = %q, want %q", got, want)
	}
	if got, want := modelChecks.Load(), int32(1); got != want {
		t.Fatalf("model checks after generated commit = %d, want %d", got, want)
	}
	if got, want := generations.Load(), int32(1); got != want {
		t.Fatalf("generations after generated commit = %d, want %d", got, want)
	}

	writeTestFile(t, filepath.Join(repo, "supplied.go"), "package supplied\n")
	runGitCommand(t, repo, "add", "supplied.go")
	runGitCommandWithEnvironment(t, repo, env, "commit", "--no-gpg-sign", "-m", "fix: preserve supplied message")
	if got, want := strings.TrimSpace(string(runGitCommandWithEnvironment(t, repo, env, "log", "-1", "--format=%B"))), "fix: preserve supplied message"; got != want {
		t.Fatalf("-m commit message = %q, want %q", got, want)
	}
	if got, want := modelChecks.Load(), int32(1); got != want {
		t.Fatalf("-m commit checked the model %d times, want %d", got, want)
	}
	if got, want := generations.Load(), int32(1); got != want {
		t.Fatalf("-m commit generated %d messages, want %d", got, want)
	}
}

func TestHasFirstLineMessageIgnoresGitComments(t *testing.T) {
	if hasFirstLineMessage("# Git status\n\n# still a comment\n") {
		t.Fatal("comment-only message file should be eligible for a suggestion")
	}
	if !hasFirstLineMessage("# a comment\n\nfix: supplied by a template\n") {
		t.Fatal("an existing non-comment message should be preserved")
	}
}

func buildTestCommitmsgBinary(t *testing.T, outputPath string) {
	t.Helper()
	sourceDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get source directory: %v", err)
	}
	command := exec.Command("go", "build", "-o", outputPath, ".")
	command.Dir = sourceDir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build commitmsg test binary: %v\n%s", err, output)
	}
}

func runGitCommandWithEnvironment(t *testing.T, dir string, env []string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func testCommandEnvironment(overrides map[string]string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			filtered = append(filtered, entry)
		}
	}
	for key, value := range overrides {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}
