package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestHasFirstLineMessageIgnoresGitComments(t *testing.T) {
	if hasFirstLineMessage("# Git status\n\n# still a comment\n") {
		t.Fatal("comment-only message file should be eligible for a suggestion")
	}
	if !hasFirstLineMessage("# a comment\n\nfix: supplied by a template\n") {
		t.Fatal("an existing non-comment message should be preserved")
	}
}
