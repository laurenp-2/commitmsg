package main

import (
	"strings"
	"testing"
)

func TestSanitizeResponse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "strips surrounding code fence",
			raw:  "```commit-message\nfeat: add local generation\n```",
			want: "feat: add local generation",
		},
		{
			name: "strips preamble lines",
			raw:  "Here is the commit message:\n\nCommit message: feat: handle empty responses",
			want: "feat: handle empty responses",
		},
		{
			name: "strips nested preamble fence and quotes",
			raw:  "Commit message:\n```text\n\"feat: sanitize nested model formatting\"\n```",
			want: "feat: sanitize nested model formatting",
		},
		{
			name: "keeps inline preamble message",
			raw:  "Commit message: feat: add a local-only client",
			want: "feat: add a local-only client",
		},
		{
			name: "strips surrounding quotes",
			raw:  "  “feat: support local models”  ",
			want: "feat: support local models",
		},
		{
			name: "collapses a wrapped subject",
			raw:  "feat: add prompt\n  sanitization",
			want: "feat: add prompt sanitization",
		},
		{
			name: "truncates at a word boundary",
			raw:  "feat: add support for configuring per-project cache retention behavior across workspaces reliably",
			want: "feat: add support for configuring per-project cache retention behavior",
		},
		{
			name: "returns empty output",
			raw:  "```\n\n```",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeResponse(tt.raw); got != tt.want {
				t.Fatalf("SanitizeResponse() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeResponseWithBody(t *testing.T) {
	raw := "feat: explain generation failures\n\n* preserve hook behavior\n- avoid blocking commits\n- keep errors actionable\n- ignore this fourth bullet"
	want := "feat: explain generation failures\n\n- preserve hook behavior\n- avoid blocking commits\n- keep errors actionable"

	if got := SanitizeResponseWithBody(raw, true); got != want {
		t.Fatalf("SanitizeResponseWithBody(..., true) = %q, want %q", got, want)
	}
	if got := SanitizeResponseWithBody(raw, false); got != "feat: explain generation failures" {
		t.Fatalf("SanitizeResponseWithBody(..., false) = %q", got)
	}
}

func TestBuildUserPrompt(t *testing.T) {
	context := "Files changed:\n- prompt.go (+1/-0)"
	if got, want := BuildUserPrompt("fix/sanitize-output", context), "Current branch: fix/sanitize-output\n\n"+context; got != want {
		t.Fatalf("BuildUserPrompt() = %q, want %q", got, want)
	}
	if got := BuildUserPrompt("main", context); got != context {
		t.Fatalf("BuildUserPrompt() on main = %q, want context only", got)
	}
}

func TestSystemPromptBodyInstruction(t *testing.T) {
	withoutBody := SystemPrompt(false)
	withBody := SystemPrompt(true)
	if withBody == withoutBody {
		t.Fatal("SystemPrompt(true) should add body instructions")
	}
	if !strings.Contains(withBody, "at most three short bullet lines") {
		t.Fatalf("SystemPrompt(true) = %q, missing body instruction", withBody)
	}
}
