package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/codex"
	"github.com/digitaldrywood/symphony-go/internal/config"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/workspace"
)

func TestRunnerRunPreparesWorkspaceRunsCodexAndRecordsSession(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	writeSkill(t, workspacePath, "review.md", "review", "Review code.", "Issue needs code review.")

	startedAt := time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(2 * time.Second)
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{
			Path:   workspacePath,
			Key:    "digitaldrywood_symphony-go_22",
			Branch: "symphony/digitaldrywood_symphony-go_22",
		},
		diffStat: workspace.DiffStat{Files: 2, Added: 5, Removed: 1},
	}
	codexClient := &fakeCodexClient{
		updates: []codex.Update{
			{
				Type: codex.UpdateTokenUsage,
				Tokens: codex.TokenUsage{
					InputTokens:  100,
					OutputTokens: 25,
					TotalTokens:  125,
				},
			},
			{
				Type: codex.UpdateRateLimits,
				RateLimits: &codex.RateLimitSnapshot{
					LimitID:   "codex-primary",
					LimitName: "Codex primary",
				},
			},
		},
		result: codex.RunTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "thread-1-turn-1"},
	}
	sessionStore := &fakeSessionStore{sessionID: 42}
	now := newFakeClock(startedAt, completedAt)

	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{
					Skills: config.Skills{
						Enabled:           true,
						Path:              ".symphony/skills",
						MaxSkillsInPrompt: 10,
					},
				},
				Codex: config.Codex{
					ApprovalPolicy: config.StringValue("never"),
					ThreadSandbox:  "workspace-write",
					TurnSandboxPolicy: map[string]any{
						"type":          "workspaceWrite",
						"networkAccess": true,
					},
				},
			},
			Prompt: "Work on {{ issue.identifier }} attempt {{ attempt }}",
		},
		Workspace: workspaceBackend,
		Codex:     codexClient,
		Store:     sessionStore,
		Now:       now.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:            "issue-22",
			Identifier:    "digitaldrywood/symphony-go#22",
			Title:         "Add runner",
			URL:           "https://github.com/digitaldrywood/symphony-go/issues/22",
			BranchName:    "symphony/digitaldrywood_symphony-go_22",
			ModelOverride: "gpt-5-codex-high",
		},
		Attempt:   2,
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.FinalState != FinalStateCompleted {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateCompleted)
	}
	if result.Tokens.TotalTokens != 125 || result.Tokens.RuntimeSeconds != 2 {
		t.Fatalf("Tokens = %#v, want total 125 and runtime 2s", result.Tokens)
	}
	if result.DiffStats.FilesChanged != 2 || result.DiffStats.AddedLines != 5 || result.DiffStats.RemovedLines != 1 {
		t.Fatalf("DiffStats = %#v, want 2 files, 5 added, 1 removed", result.DiffStats)
	}
	if result.RateLimits == nil || result.RateLimits.LimitID != "codex-primary" {
		t.Fatalf("RateLimits = %#v, want codex-primary", result.RateLimits)
	}
	if !workspaceBackend.created || !workspaceBackend.beforeRun || !workspaceBackend.afterRun || !workspaceBackend.diffed {
		t.Fatalf("workspace calls = created:%v before:%v after:%v diff:%v, want all true", workspaceBackend.created, workspaceBackend.beforeRun, workspaceBackend.afterRun, workspaceBackend.diffed)
	}
	if codexClient.request.Workspace != workspacePath {
		t.Fatalf("codex workspace = %q, want %q", codexClient.request.Workspace, workspacePath)
	}
	if codexClient.request.Model != "gpt-5-codex-high" {
		t.Fatalf("codex model = %q, want issue override", codexClient.request.Model)
	}
	for _, want := range []string{
		"Work on digitaldrywood/symphony-go#22 attempt 2",
		"## Available skills",
		"review — Issue needs code review.",
	} {
		if !strings.Contains(codexClient.request.Prompt, want) {
			t.Fatalf("codex prompt missing %q:\n%s", want, codexClient.request.Prompt)
		}
	}
	if sessionStore.started.Identifier != "digitaldrywood/symphony-go#22" || sessionStore.started.Model != "gpt-5-codex-high" {
		t.Fatalf("SessionStart = %#v, want issue identity and model", sessionStore.started)
	}
	if sessionStore.finished.FinalState != FinalStateCompleted || sessionStore.finished.TotalTokens != 125 || sessionStore.finished.Turns != 1 {
		t.Fatalf("SessionFinish = %#v, want completed session with tokens", sessionStore.finished)
	}
}

func TestRunnerRunFinishesFailedSessionAndAfterRunOnCodexError(t *testing.T) {
	t.Parallel()

	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-22", Branch: "symphony/issue-22"},
	}
	codexClient := &fakeCodexClient{err: errors.New("codex failed")}
	sessionStore := &fakeSessionStore{sessionID: 7}
	now := newFakeClock(time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC))

	runner, err := NewRunner(Dependencies{
		Workflow:  config.Workflow{Config: config.Config{}},
		Workspace: workspaceBackend,
		Codex:     codexClient,
		Store:     sessionStore,
		Now:       now.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-22",
			Identifier: "digitaldrywood/symphony-go#22",
			Title:      "Add runner",
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want codex failure")
	}
	if !strings.Contains(err.Error(), "codex failed") {
		t.Fatalf("Run() error = %v, want codex failure", err)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun was not called after codex failure")
	}
	if workspaceBackend.diffed {
		t.Fatal("DiffStat was called after codex failure")
	}
	if sessionStore.finished.FinalState != FinalStateFailed {
		t.Fatalf("SessionFinish.FinalState = %q, want %q", sessionStore.finished.FinalState, FinalStateFailed)
	}
}

type fakeWorkspaceBackend struct {
	info      workspace.Info
	diffStat  workspace.DiffStat
	created   bool
	beforeRun bool
	afterRun  bool
	diffed    bool
}

func (b *fakeWorkspaceBackend) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	b.created = true
	b.info.Branch = issue.BranchName
	return b.info, nil
}

func (b *fakeWorkspaceBackend) Cleanup(context.Context, string) error {
	return nil
}

func (b *fakeWorkspaceBackend) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	b.beforeRun = true
	return nil
}

func (b *fakeWorkspaceBackend) AfterRun(context.Context, workspace.Info, workspace.Issue) {
	b.afterRun = true
}

func (b *fakeWorkspaceBackend) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	b.diffed = true
	return b.diffStat, nil
}

type fakeCodexClient struct {
	request codex.RunTurnRequest
	updates []codex.Update
	result  codex.RunTurnResult
	err     error
}

func (c *fakeCodexClient) RunTurn(_ context.Context, req codex.RunTurnRequest, onUpdate codex.UpdateHandler) (codex.RunTurnResult, error) {
	c.request = req
	for _, update := range c.updates {
		if err := onUpdate(update); err != nil {
			return codex.RunTurnResult{}, err
		}
	}
	return c.result, c.err
}

type fakeSessionStore struct {
	sessionID int64
	started   store.SessionStart
	finished  store.SessionFinish
}

func (s *fakeSessionStore) StartSession(_ context.Context, attrs store.SessionStart) (int64, error) {
	s.started = attrs
	return s.sessionID, nil
}

func (s *fakeSessionStore) FinishSession(_ context.Context, _ int64, attrs store.SessionFinish) error {
	s.finished = attrs
	return nil
}

type fakeClock struct {
	values []time.Time
}

func newFakeClock(values ...time.Time) *fakeClock {
	return &fakeClock{values: values}
}

func (c *fakeClock) Now() time.Time {
	if len(c.values) == 0 {
		return time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC)
	}
	value := c.values[0]
	c.values = c.values[1:]
	return value
}

func writeSkill(t *testing.T, workspacePath, name, skillName, description, whenToUse string) {
	t.Helper()

	skillsDir := filepath.Join(workspacePath, ".symphony", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	content := strings.Join([]string{
		"---",
		"name: " + skillName,
		"description: " + description,
		"when_to_use: " + whenToUse,
		"---",
		"Skill body stays out of the prompt.",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(skillsDir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}
