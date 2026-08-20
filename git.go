package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// FileChange describes one staged file. Diff is populated by the diff selection
// layer after it calls StagedDiffForFile.
type FileChange struct {
	Path     string
	Added    int
	Deleted  int
	IsBinary bool
	Diff     string
}

// RepoRoot returns the absolute path to the current repository's work tree.
func RepoRoot() (string, error) {
	output, err := runGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find git repository root: %w", err)
	}

	root := trimGitLine(string(output))
	if root == "" {
		return "", fmt.Errorf("find git repository root: git returned an empty path")
	}
	return root, nil
}

// StagedFiles returns the files in the staged diff along with their line stats.
func StagedFiles() ([]FileChange, error) {
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}

	output, err := runGit(root, "diff", "--cached", "--numstat", "-z")
	if err != nil {
		return nil, fmt.Errorf("read staged file stats: %w", err)
	}

	files, err := parseNumstat(output)
	if err != nil {
		return nil, fmt.Errorf("parse staged file stats: %w", err)
	}
	return files, nil
}

// StagedDiffForFile returns the uncoloured staged patch for path.
func StagedDiffForFile(path string) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}

	output, err := runGit(root, "diff", "--cached", "--no-color", "--", path)
	if err != nil {
		return "", fmt.Errorf("read staged diff for %q: %w", path, err)
	}
	return string(output), nil
}

// CurrentBranch returns the current branch name. A missing repository or a
// detached/unresolvable HEAD is intentionally non-fatal and yields an empty name.
func CurrentBranch() (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", nil
	}

	output, err := runGit(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", nil
	}
	branch := trimGitLine(string(output))
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

// HooksDirectory returns the directory Git will use for hooks in repoRoot.
// A relative core.hooksPath is relative to the repository root.
func HooksDirectory(repoRoot string) (string, error) {
	output, err := runGit(repoRoot, "config", "--get", "core.hooksPath")
	if err == nil {
		if configured := trimGitLine(string(output)); configured != "" {
			return resolveRepoPath(repoRoot, configured), nil
		}
	} else if !gitExitStatus(err, 1) {
		return "", fmt.Errorf("read core.hooksPath: %w", err)
	}

	output, err = runGit(repoRoot, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", fmt.Errorf("find git hooks directory: %w", err)
	}

	hooksDir := trimGitLine(string(output))
	if hooksDir == "" {
		return "", fmt.Errorf("find git hooks directory: git returned an empty path")
	}
	return resolveRepoPath(repoRoot, hooksDir), nil
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	// RepoRoot necessarily runs in the caller's directory. Every operation after
	// that discovery is anchored at the repository root so subdirectory invocations
	// behave the same as invocations at the root.
	if dir != "" {
		cmd.Dir = dir
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

// parseNumstat parses `git diff --numstat -z` output. The third field is kept
// intact after the first two tabs so paths containing tabs are also preserved.
func parseNumstat(output []byte) ([]FileChange, error) {
	records := bytes.Split(output, []byte{0})
	files := make([]FileChange, 0, len(records))

	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) == 0 {
			continue
		}

		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed numstat record")
		}

		added, addedBinary, err := parseNumstatCount(fields[0])
		if err != nil {
			return nil, fmt.Errorf("invalid added-line count %q: %w", fields[0], err)
		}
		deleted, deletedBinary, err := parseNumstatCount(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid deleted-line count %q: %w", fields[1], err)
		}

		path := string(fields[2])
		if path == "" {
			// With -z, renamed and copied files are emitted as an empty path followed
			// by old and new paths in separate NUL-delimited fields. The new path is
			// the staged file we want to describe.
			if i+2 >= len(records) || len(records[i+2]) == 0 {
				return nil, fmt.Errorf("malformed rename or copy numstat record")
			}
			path = string(records[i+2])
			i += 2
		}

		files = append(files, FileChange{
			Path:     path,
			Added:    added,
			Deleted:  deleted,
			IsBinary: addedBinary || deletedBinary,
		})
	}

	return files, nil
}

func parseNumstatCount(value []byte) (count int, binary bool, err error) {
	if bytes.Equal(value, []byte("-")) {
		return 0, true, nil
	}

	count, err = strconv.Atoi(string(value))
	if err != nil || count < 0 {
		if err == nil {
			err = fmt.Errorf("negative line count")
		}
		return 0, false, err
	}
	return count, false, nil
}

func trimGitLine(value string) string {
	value = strings.TrimSuffix(value, "\n")
	return strings.TrimSuffix(value, "\r")
}

func gitExitStatus(err error, status int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == status
}

func resolveRepoPath(repoRoot, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(repoRoot, path))
}
