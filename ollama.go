package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultOllamaHost = "http://localhost:11434"

// OllamaClient is a small client for the two Ollama endpoints commitmsg needs.
// It intentionally accepts an http.Client so callers and tests can control
// transports, while request cancellation remains controlled by the context.
type OllamaClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewOllamaClient creates a client for an Ollama server. A nil HTTP client uses
// http.DefaultClient. An empty host falls back to Ollama's local default.
func NewOllamaClient(baseURL string, client *http.Client) *OllamaClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultOllamaHost
	} else if !strings.Contains(baseURL, "://") {
		// OLLAMA_HOST is commonly configured as host:port, while the CLI flag
		// documents a full URL. Accept both forms without changing the local-only
		// transport model.
		baseURL = "http://" + baseURL
	}
	return &OllamaClient{
		BaseURL:    baseURL,
		HTTPClient: client,
	}
}

// OllamaUnreachableError identifies failures to connect to the local Ollama
// server. Its wrapped cause is retained so callers can still test for context
// cancellation or deadlines.
type OllamaUnreachableError struct {
	Host string
	Err  error
}

func (e *OllamaUnreachableError) Error() string {
	return fmt.Sprintf("ollama not reachable at %s — is it running?", e.Host)
}

func (e *OllamaUnreachableError) Unwrap() error {
	return e.Err
}

// ModelNotFoundError is returned when /api/tags does not list the selected model.
type ModelNotFoundError struct {
	Model string
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("model %q not found — run: ollama pull %s", e.Model, e.Model)
}

// OllamaHTTPError describes a non-success HTTP response without exposing a raw
// net/http transport error to command-line users.
type OllamaHTTPError struct {
	Operation  string
	StatusCode int
	Status     string
	Message    string
}

func (e *OllamaHTTPError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "request"
	}
	if e.Message != "" {
		return fmt.Sprintf("ollama %s failed: %s", operation, e.Message)
	}
	if e.Status != "" {
		return fmt.Sprintf("ollama %s failed: %s", operation, e.Status)
	}
	return fmt.Sprintf("ollama %s failed with status %d", operation, e.StatusCode)
}

// OllamaProtocolError indicates that a successful endpoint response could not be
// decoded as the documented Ollama JSON shape.
type OllamaProtocolError struct {
	Operation string
}

func (e *OllamaProtocolError) Error() string {
	if e.Operation == "" {
		return "ollama returned an invalid response"
	}
	return fmt.Sprintf("ollama returned an invalid %s response", e.Operation)
}

// IsOllamaUnreachable reports whether err came from a failed connection to Ollama.
func IsOllamaUnreachable(err error) bool {
	var target *OllamaUnreachableError
	return errors.As(err, &target)
}

// IsModelNotFound reports whether err says the requested model is absent locally.
func IsModelNotFound(err error) bool {
	var target *ModelNotFoundError
	return errors.As(err, &target)
}

// IsOllamaTimeout reports whether a request context reached its deadline.
func IsOllamaTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// GenerateRequest is the non-streaming request payload supported by commitmsg.
// Set Seed for candidates after the first request to encourage variation.
type GenerateRequest struct {
	Model       string
	System      string
	Prompt      string
	Temperature float64
	NumPredict  int
	Seed        *int
}

// GenerateResponse contains the generated message and useful verbose metrics.
type GenerateResponse struct {
	Response      string
	EvalCount     int
	TotalDuration time.Duration
}

// CheckModel verifies that Ollama is reachable and that model is available. A bare
// requested name (for example qwen2.5-coder) matches an installed tagged model
// such as qwen2.5-coder:7b.
func (c *OllamaClient) CheckModel(ctx context.Context, model string) error {
	endpoint := c.endpoint("/api/tags")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return c.connectionError(ctx, err)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return c.connectionError(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newOllamaHTTPError("model check", resp)
	}

	var tags ollamaTagsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&tags); err != nil {
		return &OllamaProtocolError{Operation: "model list"}
	}
	for _, available := range tags.Models {
		if modelMatches(model, available.Name) {
			return nil
		}
	}

	return &ModelNotFoundError{Model: model}
}

// Generate submits one non-streaming completion request to Ollama.
func (c *OllamaClient) Generate(ctx context.Context, input GenerateRequest) (GenerateResponse, error) {
	numPredict := input.NumPredict
	if numPredict <= 0 {
		numPredict = 200
	}

	payload := ollamaGenerateRequest{
		Model:  input.Model,
		System: input.System,
		Prompt: input.Prompt,
		Stream: false,
		Options: ollamaGenerateOptions{
			Temperature: input.Temperature,
			NumPredict:  numPredict,
			Seed:        input.Seed,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// The request shape contains only standard scalar values, so this should be
		// unreachable; still keep a user-safe error instead of leaking internals.
		return GenerateResponse{}, &OllamaProtocolError{Operation: "generation request"}
	}

	endpoint := c.endpoint("/api/generate")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return GenerateResponse{}, c.connectionError(ctx, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return GenerateResponse{}, c.connectionError(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return GenerateResponse{}, newOllamaHTTPError("generation", resp)
	}

	var generated ollamaGenerateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&generated); err != nil {
		return GenerateResponse{}, &OllamaProtocolError{Operation: "generation"}
	}

	return GenerateResponse{
		Response:      generated.Response,
		EvalCount:     generated.EvalCount,
		TotalDuration: time.Duration(generated.TotalDuration),
	}, nil
}

func (c *OllamaClient) endpoint(path string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultOllamaHost
	}
	return baseURL + path
}

func (c *OllamaClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *OllamaClient) connectionError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return &OllamaUnreachableError{Host: c.endpoint(""), Err: err}
}

type ollamaTagsResponse struct {
	Models []ollamaModel `json:"models"`
}

type ollamaModel struct {
	Name string `json:"name"`
}

type ollamaGenerateRequest struct {
	Model   string                `json:"model"`
	System  string                `json:"system"`
	Prompt  string                `json:"prompt"`
	Stream  bool                  `json:"stream"`
	Options ollamaGenerateOptions `json:"options"`
}

type ollamaGenerateOptions struct {
	Temperature float64 `json:"temperature"`
	NumPredict  int     `json:"num_predict"`
	Seed        *int    `json:"seed,omitempty"`
}

type ollamaGenerateResponse struct {
	Response      string `json:"response"`
	EvalCount     int    `json:"eval_count"`
	TotalDuration int64  `json:"total_duration"`
}

func newOllamaHTTPError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var response struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &response)

	message := strings.Join(strings.Fields(response.Error), " ")
	if message == "" {
		message = strings.Join(strings.Fields(string(body)), " ")
	}
	if len(message) > 200 {
		message = message[:200]
	}

	return &OllamaHTTPError{
		Operation:  operation,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    message,
	}
}

func modelMatches(requested, available string) bool {
	requested = strings.TrimSpace(requested)
	available = strings.TrimSpace(available)
	if requested == available {
		return true
	}
	return !hasModelTag(requested) && bareModelName(available) == requested
}

func hasModelTag(name string) bool {
	lastSlash := strings.LastIndex(name, "/")
	return strings.LastIndex(name, ":") > lastSlash
}

func bareModelName(name string) string {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		return name[:lastColon]
	}
	return name
}
