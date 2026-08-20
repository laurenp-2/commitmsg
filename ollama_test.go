package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaClientCheckModelAndGenerate(t *testing.T) {
	var generateCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if r.Method != http.MethodGet {
				t.Errorf("tags method = %s, want GET", r.Method)
			}
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5-coder:7b","size":4700000000}]}`))
		case "/api/generate":
			generateCalls++
			if r.Method != http.MethodPost {
				t.Errorf("generate method = %s, want POST", r.Method)
			}
			if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}

			var request struct {
				Model   string `json:"model"`
				System  string `json:"system"`
				Prompt  string `json:"prompt"`
				Stream  bool   `json:"stream"`
				Options struct {
					Temperature float64 `json:"temperature"`
					NumPredict  int     `json:"num_predict"`
					Seed        *int    `json:"seed"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode generation request: %v", err)
				return
			}
			if request.Model != "qwen2.5-coder:7b" || request.System != "system" || request.Prompt != "prompt" {
				t.Errorf("unexpected generation request: %#v", request)
			}
			if request.Stream {
				t.Error("generation request has stream=true, want false")
			}
			if request.Options.Temperature != 0.2 || request.Options.NumPredict != 200 || request.Options.Seed == nil || *request.Options.Seed != 17 {
				t.Errorf("unexpected generation options: %#v", request.Options)
			}
			_, _ = w.Write([]byte(`{"response":"feat: add local client","eval_count":14,"total_duration":2500000}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewOllamaClient(server.URL, server.Client())
	ctx := context.Background()
	if err := client.CheckModel(ctx, "qwen2.5-coder"); err != nil {
		t.Fatalf("CheckModel() error = %v", err)
	}

	seed := 17
	response, err := client.Generate(ctx, GenerateRequest{
		Model:       "qwen2.5-coder:7b",
		System:      "system",
		Prompt:      "prompt",
		Temperature: 0.2,
		NumPredict:  200,
		Seed:        &seed,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if generateCalls != 1 {
		t.Fatalf("generate calls = %d, want 1", generateCalls)
	}
	if response.Response != "feat: add local client" || response.EvalCount != 14 || response.TotalDuration != 2500000*time.Nanosecond {
		t.Fatalf("Generate() = %#v", response)
	}
}

func TestOllamaClientMissingModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"other:latest"}]}`))
	}))
	defer server.Close()

	err := NewOllamaClient(server.URL, server.Client()).CheckModel(context.Background(), "qwen2.5-coder:7b")
	if !IsModelNotFound(err) {
		t.Fatalf("CheckModel() error = %T %v, want ModelNotFoundError", err, err)
	}
	if got, want := err.Error(), `model "qwen2.5-coder:7b" not found — run: ollama pull qwen2.5-coder:7b`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
