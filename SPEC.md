# `commitmsg` — Implementation Spec (v0.1.0)

A Go CLI that generates git commit messages from your staged diff using a **local**
Ollama model. No API keys, no network calls off-machine, no code leaving the laptop.

This document is the complete brief for implementing v0.1.0 from an empty directory.
Follow it top to bottom. Where it says MUST, that's a requirement; where it says
SHOULD, use judgment but default to doing it.

---

## 1. Goal and positioning

The thing that makes this worth building (as opposed to the dozen existing
AI-commit tools) is that it is **local-only** and **installs as a git hook**, so it
becomes part of the normal `git commit` flow rather than a separate command you have
to remember.

Two usage modes, both MUST work in v0.1.0:

1. **Direct:** `commitmsg` prints suggested message(s) to stdout.
2. **Hook:** `commitmsg install` writes a `prepare-commit-msg` hook so that plain
   `git commit` opens the editor pre-filled with a suggestion.

Non-goals for v0.1.0 are listed in §11. Do not build them.

---

## 2. Repository setup

Start from an empty directory named `commitmsg`.

```bash
git init
go mod init github.com/laurenp-2/commitmsg
```

Target **Go 1.22+**. Use the standard library only — `net/http`, `os/exec`,
`encoding/json`, `flag`. **No third-party dependencies** in v0.1.0. (Explicitly: no
cobra, no viper, no color libraries. `flag` is sufficient and keeps the binary
dependency-free, which is part of the pitch.)

### File layout

```
commitmsg/
├── main.go            # CLI entry, flag parsing, subcommand dispatch
├── git.go             # all git interaction (diff, branch, repo root)
├── diff.go            # diff filtering, budgeting, truncation
├── ollama.go          # Ollama HTTP client
├── prompt.go          # prompt construction + response sanitization
├── hook.go            # install / uninstall the prepare-commit-msg hook
├── git_test.go
├── diff_test.go
├── prompt_test.go
├── testdata/          # sample diffs as fixtures
├── .github/workflows/ci.yml
├── README.md
├── LICENSE            # MIT, copyright Lauren Pothuru
└── .gitignore
```

Keep everything in `package main` for v0.1.0. It's a single small binary; splitting
into `internal/` packages is premature and adds import noise.

---

## 3. CLI surface

```
commitmsg [flags]              Generate message(s) for the current staged diff
commitmsg install [--force]    Install the prepare-commit-msg hook in this repo
commitmsg uninstall            Remove the hook (only if we installed it)
commitmsg hook <file> [source] [sha]   Internal: invoked by the git hook
commitmsg version              Print version
```

### Flags (for the default generate command)

| Flag | Default | Behavior |
| --- | --- | --- |
| `--model` | `qwen2.5-coder:7b` | Ollama model tag |
| `-n` | `1` | Number of candidates to generate |
| `--host` | `http://localhost:11434` | Ollama base URL; also read from `OLLAMA_HOST` |
| `--max-chars` | `12000` | Character budget for the diff sent to the model |
| `--temperature` | `0.2` | Sampling temperature |
| `--body` | `false` | Ask for a short body paragraph in addition to the subject |
| `--timeout` | `60s` | Total request timeout |
| `--verbose` | `false` | Log the assembled prompt and timing to stderr |

Env var precedence: explicit flag > env var > default. Support `OLLAMA_HOST` and
`COMMITMSG_MODEL`.

When `-n > 1`, print candidates numbered `1.`, `2.`, `3.` one per line. Do not
prompt interactively in v0.1.0 — printing is enough and keeps it pipe-friendly.

---

## 4. Git interaction (`git.go`)

All git access MUST go through `os/exec` calling the `git` binary. Do not use a Go
git library.

Required functions:

- `RepoRoot() (string, error)` — `git rev-parse --show-toplevel`. Error if not in a
  repo.
- `StagedFiles() ([]FileChange, error)` — `git diff --cached --numstat -z`.
  Returns path, added lines, deleted lines. `numstat` reports `-` for binary files;
  mark those with `IsBinary: true`.
- `StagedDiffForFile(path string) (string, error)` —
  `git diff --cached --no-color -- <path>`.
- `CurrentBranch() (string, error)` — `git rev-parse --abbrev-ref HEAD`. Non-fatal;
  empty string on failure.

Use `-z` / `--no-color` consistently so parsing is stable. Always pass `--` before
paths to avoid ambiguity with refs.

Set `cmd.Dir` to the repo root for every invocation so the tool works from
subdirectories.

---

## 5. Diff selection and budgeting (`diff.go`)

This is the part that determines whether output is good or garbage. A naive
"dump the whole diff into the prompt" implementation fails on any real commit.

### 5.1 Exclusions

Skip entirely (do not send their contents; still mention them in the file summary):

- Binary files (from `numstat`).
- Lockfiles and generated artifacts. Match on basename:
  `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`, `Cargo.lock`,
  `Gemfile.lock`, `poetry.lock`, `composer.lock`, `*.min.js`, `*.min.css`,
  `*.map`.
- Any single file whose diff exceeds 40% of the total budget on its own — include
  only its header and stat line, with a `[truncated: N lines changed]` marker.

Exclusions matter because a lockfile update is thousands of meaningless lines that
will dominate the model's attention and produce "chore: update dependencies" for a
commit that actually contains a feature.

### 5.2 Budget allocation

Given `--max-chars` (default 12000):

1. Always include a **file summary header** listing every staged file with its
   `+N/-M` counts, including excluded ones. This is cheap and gives the model
   structure even when diffs are truncated.
2. If the sum of all included file diffs fits in the remaining budget, send them
   whole.
3. Otherwise allocate the remaining budget across files **proportionally to
   `log(1 + changed_lines)`**, not linearly. Linear allocation lets one large file
   starve five small ones; log weighting keeps small meaningful changes visible.
4. When truncating an individual file's diff, keep whole hunks (`@@` blocks) from
   the top until the allocation is exhausted, then append
   `... [N more hunks truncated]`. Never cut mid-hunk — a half hunk is worse than
   no hunk.

### 5.3 Output

`BuildContext(files []FileChange, budget int) string` returns the assembled text
block that goes into the prompt. It MUST be deterministic for a given input —
this is what makes it testable.

---

## 6. Ollama client (`ollama.go`)

Base URL from `--host` / `OLLAMA_HOST`, default `http://localhost:11434`.

### 6.1 Health and model check

Before generating, `GET {host}/api/tags`. Response shape:

```json
{ "models": [ { "name": "qwen2.5-coder:7b", "size": 4700000000, "details": {...} } ] }
```

Use this for two things:

- **Is Ollama running?** Connection refused here → the "not running" error path (§8).
- **Is the model present?** If the requested model isn't in the list, error with the
  exact pull command: `ollama pull qwen2.5-coder:7b`. Match both exact tag and
  bare name (`qwen2.5-coder` should match `qwen2.5-coder:7b`).

### 6.2 Generation

`POST {host}/api/generate` with `"stream": false`:

```json
{
  "model": "qwen2.5-coder:7b",
  "system": "<system prompt>",
  "prompt": "<user prompt>",
  "stream": false,
  "options": { "temperature": 0.2, "num_predict": 200 }
}
```

Response field of interest is `response` (string). Also read `eval_count` and
`total_duration` and surface them under `--verbose`.

Use `stream: false` in v0.1.0 — there's no UI benefit to streaming a one-line commit
message, and it keeps parsing simple.

For `-n > 1`, issue N sequential requests with `"seed"` varied in `options`, or
temperature nudged upward slightly for candidates after the first. Sequential is
fine; do not parallelize (a local model serves one request at a time anyway).

Honor `--timeout` via `context.WithTimeout` on the request.

---

## 7. Prompt and sanitization (`prompt.go`)

### 7.1 System prompt

Instruct the model to:

- Output **only** the commit message, with no preamble, no explanation, no markdown
  fences.
- Use Conventional Commits: `type(scope): subject`, where type is one of
  `feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert`.
- Subject in imperative mood ("add", not "added"/"adds"), lowercase after the colon,
  no trailing period, **≤ 72 characters**.
- Scope is optional; omit it rather than inventing one.
- With `--body`, add one blank line then at most 3 short bullet lines explaining
  *why*, not restating the diff.

### 7.2 User prompt

Assemble in this order:

1. Current branch name, if available and not `main`/`master` (branch names often
   encode intent, e.g. `fix/null-deref-on-load`).
2. The file summary header from §5.3.
3. The (possibly truncated) diff block.

### 7.3 Sanitization — MUST be applied to every response

Small local models ignore formatting instructions regularly. Sanitize:

1. Strip surrounding markdown code fences (` ``` ` with optional language tag).
2. Strip a leading `Commit message:` / `Here is...` style preamble line.
3. Strip surrounding quotes.
4. Trim whitespace; collapse the subject to a single line.
5. If the subject exceeds 72 chars, do **not** silently truncate mid-word — cut at
   the last word boundary before 72 and drop any trailing punctuation.
6. If the result is empty after sanitization, treat it as a generation failure and
   retry once; if it fails again, exit with a clear error.

Sanitization MUST be a pure function with unit tests covering each of the above
cases. This is the highest-value test surface in the project.

---

## 8. Error handling

Every failure MUST produce a one-line actionable message on stderr and a non-zero
exit code. No Go stack traces, no raw `net/http` errors.

| Condition | Message | Exit |
| --- | --- | --- |
| Not a git repo | `not a git repository` | 1 |
| Nothing staged | `no staged changes — stage something with git add` | 1 |
| Only excluded files staged | `only lockfiles/binaries staged — nothing to describe` | 1 |
| Ollama unreachable | `ollama not reachable at http://localhost:11434 — is it running?` | 1 |
| Model not installed | `model "X" not found — run: ollama pull X` | 1 |
| Timeout | `timed out after 60s — try a smaller model or raise --timeout` | 1 |
| Empty generation after retry | `model returned an empty message` | 1 |

---

## 9. The git hook (`hook.go`)

### 9.1 `commitmsg install`

Writes `<repo>/.git/hooks/prepare-commit-msg` (respect `core.hooksPath` if set):

```sh
#!/bin/sh
# installed by commitmsg
commitmsg hook "$1" "$2" "$3"
```

- `chmod 0755`.
- If a hook already exists and was **not** installed by us (no marker comment),
  refuse and tell the user to use `--force` or merge manually. Never silently
  clobber someone's existing hook.
- `uninstall` removes the file **only** if it contains our marker comment.

### 9.2 `commitmsg hook <file> <source> <sha>`

Invoked by git. Arguments are git's own: `$1` is the path to the commit message
file, `$2` is the source (`message`, `template`, `merge`, `squash`, `commit`, or
empty), `$3` is the commit SHA for amend.

Behavior rules — these are what make the hook tolerable to live with:

1. **If `$2` is `message`, `merge`, `squash`, or `commit`, exit 0 immediately and
   do nothing.** The user already supplied a message (`-m`), or git is composing
   one. Only act when `$2` is empty or `template`.
2. **Never block a commit.** Any failure — Ollama down, timeout, model missing —
   MUST exit 0 silently (log to stderr only under `COMMITMSG_DEBUG=1`). A commit
   tool that prevents you from committing when your AI server is off is unusable.
3. Enforce a **shorter timeout in hook mode** (default 15s, override with
   `COMMITMSG_TIMEOUT`). Waiting a minute at every commit is unacceptable.
4. Read the existing message file. Git puts commented `#` lines (status, diff) in
   it. Write the generated subject at the **top**, followed by a blank line, then
   **preserve all the original commented content** below it untouched.
5. If the file already has a non-comment, non-empty first line, exit 0 — something
   else already populated it.

---

## 10. Testing, CI, and docs

### 10.1 Tests (MUST)

- `prompt_test.go` — sanitization table test covering fences, preambles, quotes,
  over-length subjects, empty output.
- `diff_test.go` — budgeting: fits-in-budget, needs-truncation, one-giant-file,
  all-excluded. Use fixtures in `testdata/`. Assert determinism.
- `git_test.go` — `numstat` parsing including the binary-file `-` case and paths
  containing spaces.

Do **not** require a running Ollama for tests. The Ollama client takes an
`http.Client` (or a base URL) so tests can point at an `httptest.Server`. Add one
test that stubs `/api/tags` and `/api/generate`.

### 10.2 CI

`.github/workflows/ci.yml`: on push and PR, run `go vet ./...`, `go test ./...`,
and `gofmt -l .` (fail if output is non-empty). Add the badge to the README.

### 10.3 README (MUST)

Required sections, in this order:

1. One-sentence description leading with **local / no API key**.
2. **A demo GIF** showing `git commit` opening a pre-filled editor via the hook.
   This is the single most important element — record it before writing prose.
3. Install: `go install github.com/laurenp-2/commitmsg@latest`.
4. Quick start: `ollama pull qwen2.5-coder:7b`, then `commitmsg install`.
5. Flags table.
6. A short "How it works" covering the diff budgeting from §5 — this is the
   interesting engineering and it should be visible, not buried.
7. License.

### 10.4 Release

Tag `v0.1.0`. Create a GitHub release. `go install` works off the tag with no extra
tooling, so goreleaser and Homebrew are out of scope for v0.1.0.

---

## 11. Explicit non-goals for v0.1.0

Do not implement these. They are scope creep and each one delays shipping:

- Interactive candidate picker / TUI.
- Streaming responses.
- Cloud model support (OpenAI, Anthropic, etc.). Local-only is the point.
- Config files. Flags and env vars only.
- Amend, rebase, or merge-commit handling beyond "exit 0 and do nothing".
- Multi-language commit message output.
- Automatic `git commit` execution — this tool suggests, it never commits.
- Homebrew tap, goreleaser, cross-compiled release binaries.

---

## 12. Definition of done

v0.1.0 ships when all of these are true:

- [ ] `commitmsg` prints a sensible conventional commit for a real staged diff.
- [ ] `commitmsg -n 3` prints three distinct candidates.
- [ ] `commitmsg install` then `git commit` opens the editor pre-filled.
- [ ] `git commit -m "x"` is unaffected by the hook.
- [ ] With Ollama stopped, `git commit` still works normally and instantly.
- [ ] A commit staging a lockfile plus one source file describes the source change.
- [ ] A 3000-line diff produces a message without erroring.
- [ ] Every error in §8 produces its exact message, no stack trace.
- [ ] `go test ./...` passes with no Ollama running.
- [ ] CI green, README with demo GIF, tagged `v0.1.0` release.
