package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReportsExpectedLocalErrors(t *testing.T) {
	t.Run("outside repository", func(t *testing.T) {
		chdirForTest(t, t.TempDir())
		var stdout, stderr bytes.Buffer
		if code := run(nil, &stdout, &stderr); code != 1 {
			t.Fatalf("run() exit = %d, want 1", code)
		}
		if got, want := stderr.String(), "not a git repository\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("nothing staged", func(t *testing.T) {
		repo := setupGitRepository(t)
		chdirForTest(t, repo)
		var stdout, stderr bytes.Buffer
		if code := run(nil, &stdout, &stderr); code != 1 {
			t.Fatalf("run() exit = %d, want 1", code)
		}
		if got, want := stderr.String(), "no staged changes — stage something with git add\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
}

func TestRunGeneratesNumberedCandidates(t *testing.T) {
	repo := setupGitRepository(t)
	writeTestFile(t, filepath.Join(repo, "main.go"), "package demo\n\nfunc main() {}\n")
	runGitCommand(t, repo, "add", "main.go")
	chdirForTest(t, repo)

	generateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model:latest"}]}`))
		case "/api/generate":
			generateCalls++
			if generateCalls == 1 {
				_, _ = w.Write([]byte(`{"response":"feat: add a demo command"}`))
				return
			}
			_, _ = w.Write([]byte(`{"response":"test: cover the demo command"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--model", "test-model:latest", "-n", "2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() exit = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "1. feat: add a demo command\n2. test: cover the demo command\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if generateCalls != 2 {
		t.Fatalf("generate calls = %d, want 2", generateCalls)
	}
}

func TestFlagEnvironmentPrecedence(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "http://localhost:11434")
	t.Setenv("COMMITMSG_MODEL", "env-model")

	options, err := parseGenerateOptions([]string{"--host", "http://127.0.0.1:11435", "--model", "flag-model"})
	if err != nil {
		t.Fatalf("parseGenerateOptions() error = %v", err)
	}
	if options.host != "http://127.0.0.1:11435" || options.model != "flag-model" {
		t.Fatalf("flag values did not win: %#v", options)
	}
}

func TestParseGenerateOptionsRejectsRemoteHost(t *testing.T) {
	_, err := parseGenerateOptions([]string{"--host", "https://example.com"})
	if err == nil || err.Error() != "--host must point to a local Ollama server" {
		t.Fatalf("parseGenerateOptions() error = %v", err)
	}
}

func TestParseGenerateOptionsAcceptsLocalHostPort(t *testing.T) {
	options, err := parseGenerateOptions([]string{"--host", "127.0.0.1:11435"})
	if err != nil {
		t.Fatalf("parseGenerateOptions() error = %v", err)
	}
	if got, want := options.host, "http://127.0.0.1:11435"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
}

func TestParseGenerateOptionsAcceptsSchemeLessLocalHostFromEnvironment(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "localhost:11435")
	options, err := parseGenerateOptions(nil)
	if err != nil {
		t.Fatalf("parseGenerateOptions() error = %v", err)
	}
	if got, want := options.host, "http://localhost:11435"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
}

func TestRunRetriesDuplicateCandidates(t *testing.T) {
	repo := setupGitRepository(t)
	writeTestFile(t, filepath.Join(repo, "main.go"), "package demo\n")
	runGitCommand(t, repo, "add", "main.go")
	chdirForTest(t, repo)

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/generate":
			calls++
			if calls < 3 {
				_, _ = w.Write([]byte(`{"response":"feat: add local suggestions"}`))
				return
			}
			_, _ = w.Write([]byte(`{"response":"test: cover local suggestions"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--model", "test-model", "-n", "2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() exit = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "1. feat: add local suggestions\n2. test: cover local suggestions\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if calls != 3 {
		t.Fatalf("generation calls = %d, want 3", calls)
	}
}

func TestRunFailsInsteadOfPrintingDuplicateCandidates(t *testing.T) {
	repo := setupGitRepository(t)
	writeTestFile(t, filepath.Join(repo, "main.go"), "package demo\n")
	runGitCommand(t, repo, "add", "main.go")
	chdirForTest(t, repo)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/generate":
			_, _ = w.Write([]byte(`{"response":"feat: add local suggestions"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--model", "test-model", "-n", "2"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no duplicate candidates", stdout.String())
	}
	if got, want := stderr.String(), "model returned duplicate candidates — try a higher --temperature\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func setupGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitCommand(t, repo, "init", "--quiet")
	return repo
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func chdirForTest(t *testing.T, directory string) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatalf("change directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}
