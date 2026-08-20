package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const version = "v0.1.0"

var (
	errNotGitRepository    = errors.New("not a git repository")
	errNoStagedChanges     = errors.New("no staged changes — stage something with git add")
	errOnlyExcluded        = errors.New("only lockfiles/binaries staged — nothing to describe")
	errEmptyGeneration     = errors.New("model returned an empty message")
	errDuplicateCandidates = errors.New("model returned duplicate candidates — try a higher --temperature")
	errHookExists          = errors.New("prepare-commit-msg hook already exists — use --force or merge manually")
	errHookNotOurs         = errors.New("prepare-commit-msg hook was not installed by commitmsg")
)

type generateOptions struct {
	model       string
	host        string
	maxChars    int
	temperature float64
	body        bool
	timeout     time.Duration
	verbose     bool
	candidates  int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runInstall(args[1:], stdout, stderr)
		case "uninstall":
			if len(args) != 1 {
				printError(stderr, "uninstall does not accept arguments")
				return 1
			}
			if err := UninstallHook(); err != nil {
				printError(stderr, formatCommandError(err))
				return 1
			}
			fmt.Fprintln(stdout, "removed prepare-commit-msg hook")
			return 0
		case "hook":
			return RunHook(args[1:], stderr)
		case "version":
			if len(args) != 1 {
				printError(stderr, "version does not accept arguments")
				return 1
			}
			fmt.Fprintf(stdout, "commitmsg %s\n", version)
			return 0
		case "help", "--help", "-h":
			printUsage(stdout)
			return 0
		}
	}

	options, err := parseGenerateOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		printUsage(stdout)
		return 0
	}
	if err != nil {
		printError(stderr, "invalid flags — "+singleLine(err.Error()))
		return 1
	}

	messages, err := generateMessages(options, stderr)
	if err != nil {
		printError(stderr, formatGenerationError(err, options))
		return 1
	}
	for i, message := range messages {
		if options.candidates > 1 {
			fmt.Fprintf(stdout, "%d. %s\n", i+1, message)
			continue
		}
		fmt.Fprintln(stdout, message)
	}
	return 0
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	force := flags.Bool("force", false, "replace an existing prepare-commit-msg hook")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "usage: commitmsg install [--force]")
			return 0
		}
		printError(stderr, "invalid flags — "+singleLine(err.Error()))
		return 1
	}
	if len(flags.Args()) != 0 {
		printError(stderr, "install does not accept positional arguments")
		return 1
	}
	if err := InstallHook(*force); err != nil {
		printError(stderr, formatCommandError(err))
		return 1
	}
	fmt.Fprintln(stdout, "installed prepare-commit-msg hook")
	return 0
}

func parseGenerateOptions(args []string) (generateOptions, error) {
	options := generateOptions{
		model:       envOrDefault("COMMITMSG_MODEL", defaultModel),
		host:        envOrDefault("OLLAMA_HOST", defaultHost),
		maxChars:    defaultMaxDiff,
		temperature: 0.2,
		timeout:     60 * time.Second,
		candidates:  1,
	}

	flags := flag.NewFlagSet("commitmsg", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.model, "model", options.model, "Ollama model")
	flags.StringVar(&options.host, "host", options.host, "Ollama base URL")
	flags.IntVar(&options.candidates, "n", options.candidates, "number of candidates")
	flags.IntVar(&options.maxChars, "max-chars", options.maxChars, "maximum diff characters")
	flags.Float64Var(&options.temperature, "temperature", options.temperature, "sampling temperature")
	flags.BoolVar(&options.body, "body", false, "include a short body")
	flags.DurationVar(&options.timeout, "timeout", options.timeout, "request timeout")
	flags.BoolVar(&options.verbose, "verbose", false, "log prompt and timing")
	if err := flags.Parse(args); err != nil {
		return generateOptions{}, err
	}
	if len(flags.Args()) != 0 {
		return generateOptions{}, fmt.Errorf("unexpected argument %q", flags.Args()[0])
	}

	options.model = strings.TrimSpace(options.model)
	options.host = normalizeHost(options.host)
	if options.model == "" {
		return generateOptions{}, errors.New("--model cannot be empty")
	}
	if err := validateLocalOllamaHost(options.host); err != nil {
		return generateOptions{}, err
	}
	if options.candidates < 1 {
		return generateOptions{}, errors.New("-n must be at least 1")
	}
	if options.maxChars < 1 {
		return generateOptions{}, errors.New("--max-chars must be at least 1")
	}
	if options.timeout <= 0 {
		return generateOptions{}, errors.New("--timeout must be greater than zero")
	}
	return options, nil
}

func generateMessages(options generateOptions, stderr io.Writer) ([]string, error) {
	return generateMessagesWithContext(context.Background(), options, stderr)
}

func generateMessagesWithContext(parent context.Context, options generateOptions, stderr io.Writer) ([]string, error) {
	if _, err := RepoRoot(); err != nil {
		return nil, errNotGitRepository
	}

	files, err := StagedFiles()
	if err != nil {
		return nil, fmt.Errorf("read staged files: %w", err)
	}
	if len(files) == 0 {
		return nil, errNoStagedChanges
	}

	included := false
	for i := range files {
		if IsExcluded(files[i]) {
			continue
		}
		included = true
		diff, err := StagedDiffForFile(files[i].Path)
		if err != nil {
			return nil, fmt.Errorf("read staged diff: %w", err)
		}
		files[i].Diff = diff
	}
	if !included {
		return nil, errOnlyExcluded
	}

	diffContext := BuildContext(files, options.maxChars)
	branch, _ := CurrentBranch()
	prompt := BuildUserPrompt(branch, diffContext)
	system := SystemPrompt(options.body)
	if options.verbose {
		fmt.Fprintf(stderr, "system prompt:\n%s\n\nuser prompt:\n%s\n", system, prompt)
	}

	client := NewOllamaClient(options.host, nil)
	checkStarted := time.Now()
	checkContext, cancel := context.WithTimeout(parent, options.timeout)
	err = client.CheckModel(checkContext, options.model)
	cancel()
	if err != nil {
		return nil, err
	}
	if options.verbose {
		fmt.Fprintf(stderr, "model check: %s\n", time.Since(checkStarted).Round(time.Millisecond))
	}

	messages := make([]string, 0, options.candidates)
	seen := make(map[string]struct{}, options.candidates)
	for i := 0; i < options.candidates; i++ {
		var message string
		var response GenerateResponse
		unique := false
		for variation := 0; variation < candidateVariationAttempts(options.candidates); variation++ {
			// The seed is changed for every retry as well as every requested
			// candidate, giving a deterministic way to avoid repeated suggestions.
			seedIndex := i + variation*options.candidates
			message, response, err = generateCandidate(parent, client, options, system, prompt, seedIndex)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[message]; exists {
				continue
			}
			unique = true
			break
		}
		if !unique {
			return nil, errDuplicateCandidates
		}
		if options.verbose {
			fmt.Fprintf(stderr, "generation %d: %s", i+1, response.TotalDuration.Round(time.Millisecond))
			if response.EvalCount > 0 {
				fmt.Fprintf(stderr, " (%d eval tokens)", response.EvalCount)
			}
			fmt.Fprintln(stderr)
		}
		seen[message] = struct{}{}
		messages = append(messages, message)
	}
	return messages, nil
}

func candidateVariationAttempts(candidateCount int) int {
	if candidateCount <= 1 {
		return 1
	}
	return 3
}

func generateCandidate(parent context.Context, client *OllamaClient, options generateOptions, system, prompt string, candidate int) (string, GenerateResponse, error) {
	// Retry once only when the model returns no usable text. Every retry gets a
	// distinct deterministic seed, which also supplies variation for -n > 1.
	for attempt := 0; attempt < 2; attempt++ {
		seedValue := 1000 + candidate*17 + attempt
		var seed *int
		if candidate > 0 || attempt > 0 {
			seed = &seedValue
		}

		ctx, cancel := context.WithTimeout(parent, options.timeout)
		response, err := client.Generate(ctx, GenerateRequest{
			Model:       options.model,
			System:      system,
			Prompt:      prompt,
			Temperature: options.temperature,
			NumPredict:  200,
			Seed:        seed,
		})
		cancel()
		if err != nil {
			return "", GenerateResponse{}, err
		}
		if message := SanitizeResponseWithBody(response.Response, options.body); message != "" {
			return message, response, nil
		}
	}
	return "", GenerateResponse{}, errEmptyGeneration
}

func formatGenerationError(err error, options generateOptions) string {
	switch {
	case errors.Is(err, errNotGitRepository), errors.Is(err, errNoStagedChanges),
		errors.Is(err, errOnlyExcluded), errors.Is(err, errEmptyGeneration),
		errors.Is(err, errDuplicateCandidates):
		return err.Error()
	case IsOllamaUnreachable(err), IsModelNotFound(err):
		return err.Error()
	case IsOllamaTimeout(err):
		return fmt.Sprintf("timed out after %s — try a smaller model or raise --timeout", displayDuration(options.timeout))
	default:
		return "could not generate a commit message — check Ollama and try again"
	}
}

func formatCommandError(err error) string {
	switch {
	case errors.Is(err, errNotGitRepository), errors.Is(err, errHookExists), errors.Is(err, errHookNotOurs):
		return err.Error()
	default:
		return "could not update the prepare-commit-msg hook — check repository permissions"
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return defaultHost
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// validateLocalOllamaHost protects the tool's local-only guarantee. A staged
// diff must never be sent to a host outside the current machine, even when an
// environment variable or an explicit --host value is supplied.
func validateLocalOllamaHost(host string) error {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("--host must be a valid local http URL")
	}

	name := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if name == "localhost" {
		return nil
	}
	if ip := net.ParseIP(name); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return nil
	}
	return errors.New("--host must point to a local Ollama server")
}

func displayDuration(duration time.Duration) string {
	if duration%time.Second == 0 {
		return fmt.Sprintf("%ds", int64(duration/time.Second))
	}
	return duration.String()
}

func printError(stderr io.Writer, message string) {
	fmt.Fprintln(stderr, singleLine(message))
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: commitmsg [flags]")
	fmt.Fprintln(writer, "       commitmsg install [--force]")
	fmt.Fprintln(writer, "       commitmsg uninstall")
	fmt.Fprintln(writer, "       commitmsg version")
}
