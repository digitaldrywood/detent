package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/codex"
	"github.com/digitaldrywood/symphony-go/internal/config"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/connector/memory"
	runpkg "github.com/digitaldrywood/symphony-go/internal/runner"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/workspace"
)

func TestMemoryConnectorRunnerE2EGateCreatesBranchDiffStatAndSQLiteTokens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sourceRoot := e2eInitSourceRepo(t)
	workspacesRoot := filepath.Join(t.TempDir(), "workspaces")
	dbPath := filepath.Join(t.TempDir(), "symphony.db")

	storeBackend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := storeBackend.Close(); err != nil {
			t.Fatalf("store Close() error = %v", err)
		}
	})

	workspaceBackend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		Root:       workspacesRoot,
		SourceRoot: sourceRoot,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("workspace.NewBackend() error = %v", err)
	}

	runnerBackend, err := runpkg.NewRunner(runpkg.Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Codex: config.Codex{
					ApprovalPolicy: config.StringValue("never"),
				},
			},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace: workspaceBackend,
		Codex: &e2eCodexClient{
			inputTokens:  321,
			outputTokens: 45,
			totalTokens:  366,
		},
		Store:  storeBackend,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("runner.NewRunner() error = %v", err)
	}

	issue := connector.NewIssue()
	issue.ID = "I_kwDOSskuwc8AAAABD42gFg"
	issue.Identifier = "digitaldrywood/symphony-go#23"
	issue.Title = "transcript byte-parity + e2e gate"
	issue.State = "Todo"
	issue.URL = "https://github.com/digitaldrywood/symphony-go/issues/23"
	issue.ModelOverride = "gpt-5-codex"

	orch, err := New(Config{
		PollInterval:           time.Hour,
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Hour,
	}, Dependencies{
		Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}}),
		Runner:    runnerBackend,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.Run(runCtx)
	}()
	t.Cleanup(cancel)

	state := waitForCompletedIssue(t, orch, issue.ID)
	cancel()
	assertOrchestratorStopped(t, errCh)

	completed := state.Completed[issue.ID]
	if completed.FinalState != runpkg.FinalStateCompleted {
		t.Fatalf("completed final state = %q, want %q", completed.FinalState, runpkg.FinalStateCompleted)
	}
	diffStat := state.DiffStats[issue.ID]
	if diffStat.FilesChanged != 1 || diffStat.AddedLines != 1 || diffStat.RemovedLines != 0 || diffStat.Status != "changed" {
		t.Fatalf("DiffStats = %#v, want one added file", diffStat)
	}

	workspacePath := filepath.Join(workspacesRoot, workspace.SafeKey(issue.Identifier))
	wantBranch := "symphony/" + strings.ToLower(workspace.SafeKey(issue.Identifier))
	if got := strings.TrimSpace(e2eRunGit(t, workspacePath, "branch", "--show-current")); got != wantBranch {
		t.Fatalf("workspace branch = %q, want %q", got, wantBranch)
	}
	if got := e2eReadFile(t, filepath.Join(workspacePath, "agent-output.txt")); got != "done\n" {
		t.Fatalf("agent-output.txt = %q, want done", got)
	}

	spend, err := storeBackend.IssueTokenSpend(ctx, store.IssueIdentity{Identifier: issue.Identifier})
	if err != nil {
		t.Fatalf("IssueTokenSpend() error = %v", err)
	}
	if spend.InputTokens != 321 || spend.OutputTokens != 45 || spend.TotalTokens != 366 || spend.Sessions != 1 {
		t.Fatalf("IssueTokenSpend() = %#v, want recorded codex totals", spend)
	}
	if len(spend.ByModel) != 1 || spend.ByModel[0].Model != "gpt-5-codex" {
		t.Fatalf("IssueTokenSpend().ByModel = %#v, want gpt-5-codex", spend.ByModel)
	}
}

type e2eCodexClient struct {
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

func (c *e2eCodexClient) RunTurn(_ context.Context, req codex.RunTurnRequest, onUpdate codex.UpdateHandler) (codex.RunTurnResult, error) {
	if strings.TrimSpace(req.Workspace) == "" {
		return codex.RunTurnResult{}, errors.New("workspace is required")
	}
	if err := os.WriteFile(filepath.Join(req.Workspace, "agent-output.txt"), []byte("done\n"), 0o600); err != nil {
		return codex.RunTurnResult{}, fmt.Errorf("write agent output: %w", err)
	}
	if onUpdate != nil {
		if err := onUpdate(codex.Update{
			Type: codex.UpdateTokenUsage,
			Tokens: codex.TokenUsage{
				InputTokens:  c.inputTokens,
				OutputTokens: c.outputTokens,
				TotalTokens:  c.totalTokens,
			},
		}); err != nil {
			return codex.RunTurnResult{}, err
		}
	}
	return codex.RunTurnResult{ThreadID: "thread-e2e", TurnID: "turn-e2e", SessionID: "thread-e2e-turn-e2e"}, nil
}

func waitForCompletedIssue(t *testing.T, orch *Orchestrator, issueID string) State {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stateCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		state, err := orch.State(stateCtx)
		cancel()
		if err == nil {
			if _, ok := state.Completed[issueID]; ok {
				return state
			}
		} else if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("State() error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for completed issue %s", issueID)
	return State{}
}

func assertOrchestratorStopped(t *testing.T, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("orchestrator Run() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for orchestrator to stop")
	}
}

func e2eInitSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	e2eRunCommand(t, dir, "git", "init", "-b", "main")
	e2eRunGit(t, dir, "config", "user.name", "Test User")
	e2eRunGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	e2eRunGit(t, dir, "add", "README.md")
	e2eRunGit(t, dir, "commit", "-m", "initial")

	return dir
}

func e2eRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return e2eRunCommand(t, dir, "git", args...)
}

func e2eRunCommand(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func e2eReadFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
