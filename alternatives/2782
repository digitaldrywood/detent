package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerFollowupPrompt(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		resume   bool
		changed  bool
		wantFull bool
	}{
		{"same thread", true, false, false},
		{"changed issue context", true, true, false},
		{"fresh thread", false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeCodexClient{result: AgentTurnResult{ThreadID: "thread-2661", SessionID: "session-2661"}}
			full := strings.Repeat("Standing workflow instructions.\n", 2000)
			runner, err := NewRunner(Dependencies{
				Workflow:     config.Workflow{Prompt: full + "\n{{ issue.description }}"},
				Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "2661", Branch: "detent/2661"}},
				AgentBackend: backend,
				Store:        &fakeSessionStore{sessionID: 2661},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := RunRequest{Issue: connector.Issue{ID: "2661", Identifier: "digitaldrywood/detent#2661", Title: "Prompt delta", Description: "Original handoff"}}
			if _, err := runner.Run(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(backend.request.Prompt, full) {
				t.Fatal("initial prompt lost workflow")
			}
			if tt.resume {
				req.RetryMode = RetryModeResume
				req.ResumeState = store.AgentResumeState{ProviderThreadID: "thread-2661", ProviderSessionID: "session-2661"}
			}
			if tt.changed {
				req.Issue.Description = "New handoff text"
			}
			if _, err := runner.Run(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			got := backend.request.Prompt
			if strings.Contains(got, full) != tt.wantFull {
				t.Fatalf("full prompt present = %v, want %v", strings.Contains(got, full), tt.wantFull)
			}
			if !tt.wantFull && len(got) >= 4096 {
				t.Fatalf("follow-up has %d bytes", len(got))
			}
			if tt.changed && !strings.Contains(got, "New handoff text") {
				t.Fatalf("missing changed context: %s", got)
			}
			if tt.resume {
				req.Issue.Description = "New handoff text\nAdditional comment"
				if _, err := runner.Run(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				got = backend.request.Prompt
				if len(got) >= 4096 || !strings.Contains(got, "Additional comment") || !strings.Contains(got, "after all earlier updates") {
					t.Fatalf("third turn prompt = %q", got)
				}
			}
		})
	}
}

func TestPromptDelta(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, before, after, want string }{
		{"unchanged", "same", "same", "Continue the assigned work"},
		{"replacement", "first\nold\nlast", "first\nnew\nlast", "Replace 1 line(s) starting at line 2 with:\nnew"},
		{"insertion", "first\nlast", "first\nnew\nlast", "Replace 0 line(s) starting at line 2 with:\nnew"},
		{"deletion", "first\nold\nlast", "first\nlast", "Replace 1 line(s) starting at line 2 with:\n(deleted)"},
		{"append", "first", "first\nnew", "Replace 0 line(s) starting at line 2 with:\nnew"},
		{"trailing deletion", "first\nold", "first", "Replace 1 line(s) starting at line 2 with:\n(deleted)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptDelta(tt.before, tt.after); !strings.Contains(got, tt.want) {
				t.Fatalf("delta = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFollowupPromptUnknownHistory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		previous AgentResume
		resume   AgentResume
		wantFull bool
	}{
		{"unknown thread", AgentResume{}, AgentResume{ThreadID: "new"}, true},
		{"different thread", AgentResume{ThreadID: "old"}, AgentResume{ThreadID: "new"}, true},
		{"session backend", AgentResume{SessionID: "same"}, AgentResume{SessionID: "same"}, false},
		{"thread with new turn session", AgentResume{ThreadID: "same", SessionID: "turn1"}, AgentResume{ThreadID: "same", SessionID: "turn2"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{promptHistory: map[string]sessionPrompt{"issue": {resume: tt.previous, text: "full prompt"}}}
			got := r.followupPrompt("issue", tt.resume, "full prompt")
			if (got == "full prompt") != tt.wantFull {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

func TestPromptDeltaInsertedParagraphs(t *testing.T) {
	t.Parallel()
	var workflow strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&workflow, "Standing instruction %d\n", i)
	}
	before := "head\n\n" + workflow.String() + "\nend"
	for _, after := range []string{
		"head\nnew handoff\n\n\n" + workflow.String() + "\nend",
		"head\nnew handoff\n\n\n" + workflow.String() + "\nnew summary\nend",
	} {
		got := promptDelta(before, after)
		if len(got) >= 4096 || strings.Contains(got, "Standing instruction") {
			t.Fatalf("small update repeats workflow (%d bytes)", len(got))
		}
		if !strings.Contains(got, "new handoff") {
			t.Fatal("missing handoff")
		}
	}
}
