package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	hookName       = "prepare-commit-msg"
	hookMarker     = "# installed by commitmsg"
	hookScript     = "#!/bin/sh\n" + hookMarker + "\ncommitmsg hook \"$1\" \"$2\" \"$3\"\n"
	hookTimeout    = 15 * time.Second
	defaultModel   = "qwen2.5-coder:7b"
	defaultHost    = "http://localhost:11434"
	defaultMaxDiff = 12000
)

// InstallHook installs the prepare-commit-msg hook in the current repository.
// Existing hooks written by another tool are left alone unless force is true.
func InstallHook(force bool) error {
	repoRoot, err := RepoRoot()
	if err != nil {
		return errNotGitRepository
	}

	hooksDir, err := HooksDirectory(repoRoot)
	if err != nil {
		return fmt.Errorf("find hooks directory: %w", err)
	}
	path := filepath.Join(hooksDir, hookName)

	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), hookMarker) && !force {
			return errHookExists
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing hook: %w", err)
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(hookScript), 0o755); err != nil {
		return fmt.Errorf("write hook: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("make hook executable: %w", err)
	}
	return nil
}

// UninstallHook removes the hook only when it contains our marker. A missing
// hook is already the desired state and is therefore a successful no-op.
func UninstallHook() error {
	repoRoot, err := RepoRoot()
	if err != nil {
		return errNotGitRepository
	}

	hooksDir, err := HooksDirectory(repoRoot)
	if err != nil {
		return fmt.Errorf("find hooks directory: %w", err)
	}
	path := filepath.Join(hooksDir, hookName)
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read hook: %w", err)
	}
	if !strings.Contains(string(contents), hookMarker) {
		return errHookNotOurs
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove hook: %w", err)
	}
	return nil
}

// RunHook implements the fail-open entrypoint used by prepare-commit-msg. It
// always returns zero: a local model must never stop a normal commit.
func RunHook(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "" {
		hookDebug(stderr, "missing commit-message path")
		return 0
	}

	source := ""
	if len(args) > 1 {
		source = args[1]
	}
	if source != "" && source != "template" {
		return 0
	}

	messagePath := args[0]
	original, err := os.ReadFile(messagePath)
	if err != nil {
		hookDebug(stderr, "could not read commit-message file")
		return 0
	}
	if hasFirstLineMessage(string(original)) {
		return 0
	}

	options, err := hookGenerateOptions()
	if err != nil {
		hookDebug(stderr, singleLine(err.Error()))
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	messages, err := generateMessagesWithContext(ctx, options, stderr)
	if err != nil || len(messages) == 0 {
		if err != nil {
			hookDebug(stderr, formatGenerationError(err, options))
		} else {
			hookDebug(stderr, "model returned an empty message")
		}
		return 0
	}

	updated := messages[0] + "\n\n" + string(original)
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(messagePath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(messagePath, []byte(updated), mode); err != nil {
		hookDebug(stderr, "could not write commit-message file")
	}
	return 0
}

func hasFirstLineMessage(contents string) bool {
	// Treat the first meaningful line as authoritative. Git's own status content
	// is commented, and another hook may put a comment above a real message.
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return true
	}
	return false
}

func hookGenerateOptions() (generateOptions, error) {
	timeout := hookTimeout
	if value := strings.TrimSpace(os.Getenv("COMMITMSG_TIMEOUT")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return generateOptions{}, fmt.Errorf("invalid timeout")
		}
		timeout = parsed
	}

	host := normalizeHost(envOrDefault("OLLAMA_HOST", defaultHost))
	if err := validateLocalOllamaHost(host); err != nil {
		return generateOptions{}, err
	}

	return generateOptions{
		model:       envOrDefault("COMMITMSG_MODEL", defaultModel),
		host:        host,
		maxChars:    defaultMaxDiff,
		temperature: 0.2,
		timeout:     timeout,
		candidates:  1,
	}, nil
}

func hookDebug(stderr io.Writer, message string) {
	if os.Getenv("COMMITMSG_DEBUG") == "1" {
		fmt.Fprintf(stderr, "commitmsg: %s\n", message)
	}
}
