package dispatch

import (
	"context"
	"errors"
	"fmt"

	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeRunnerE2EGateCreatesBranchDiffStatAndPostgresTokens(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sourceRoot := nativeChangeSourceRepo(t)
	workspacesRoot := filepath.Join(t.TempDir(), "workspaces")
	hub := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Rework", "Done"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken)})
	hub.configureSourceOrigin(t, sourceRoot)
	created, err := hub.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Record agent output", Body: "Write the requested output file." + issueContractTestSections, State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	hub.projectBoard()
	issue := issueFromNative(created)

	workspaceBackend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{
		ProcessSource: emptyProcessSource,
		Root:          workspacesRoot,
		SourceRoot:    sourceRoot,
		AutoBranch:    true,
	})
	if err != nil {
		t.Fatalf("workspace.NewBackend() error = %v", err)
	}

	runnerBackend, err := runpkg.NewRunner(runpkg.Dependencies{
		ProjectID:     "local",
		ProcessSource: emptyProcessSource,
		Workflow: config.Workflow{
			Config: config.Config{
				Policy:  hub.descriptor,
				Tracker: config.Tracker{Kind: config.TrackerHubNative},
				Agents:  config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: config.DefaultAgentBackendID, Model: "gpt-5-codex", Default: true}}},
				Codex: config.Codex{
					ApprovalPolicy: config.StringValue("never"),
				},
			},
			Prompt: "Work on {{ issue.identifier }}",
		},
		Workspace: workspaceBackend,
		AgentBackend: &nativeE2EAgentBackend{
			inputTokens:       321,
			cachedInputTokens: 300,
			outputTokens:      45,
			totalTokens:       366,
			lastInputTokens:   50,
			lastCachedTokens:  45,
			lastOutputTokens:  5,
			lastTotalTokens:   55,
		},
		Store: api.NewRunnerSessions(hub.native),
		Pricing: budget.PricingTable{
			"gpt-5-codex": {
				USDPerInputToken:       0.001,
				USDPerCachedInputToken: 0.0001,
				USDPerOutputToken:      0.01,
			},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("runner.NewRunner() error = %v", err)
	}

	orch, err := orchestrator.New(orchestrator.Config{
		Project:                scheduler.ProjectCandidate{ID: "local"},
		Policy:                 hub.descriptor,
		PollInterval:           time.Hour,
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo", "In Progress"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Hour,
	}, orchestrator.Dependencies{
		Connector:  hub.connector,
		Scheduling: hub.scheduler,
		Runner:     runnerBackend,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.Run(runCtx)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			assertNativeE2EOrchestratorStopped(t, errCh)
		})
	}
	t.Cleanup(stop)

	state := waitForNativeE2ERetriedIssue(t, orch, issue.ID)
	stop()

	if retry, ok := state.Retry[issue.ID]; !ok || retry.Attempt != 1 {
		t.Fatalf("dirty uncommitted result retry = %+v, present = %t", retry, ok)
	}
	diffStat := state.DiffStats[issue.ID]
	if diffStat.FilesChanged != 1 || diffStat.AddedLines != 1 || diffStat.RemovedLines != 0 || diffStat.Status != "changed" {
		t.Fatalf("DiffStats = %#v, want one added file", diffStat)
	}

	entries, err := os.ReadDir(workspacesRoot)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var workspaceKeys []string
	for _, entry := range entries {
		if entry.Name() != ".detent" {
			workspaceKeys = append(workspaceKeys, entry.Name())
		}
	}
	if len(workspaceKeys) != 1 {
		t.Fatalf("workspace entries = %v, want one issue workspace", workspaceKeys)
	}
	if records, err := os.ReadDir(filepath.Join(workspacesRoot, ".detent", "cleanup-ownership")); err != nil || len(records) != 1 {
		t.Fatalf("recovery ownership records = %v, error = %v", records, err)
	}
	workspaceKey := workspaceKeys[0]
	workspacePath := filepath.Join(workspacesRoot, workspaceKey)
	wantBranch := "detent/" + strings.ToLower(workspaceKey)
	branch, err := exec.CommandContext(t.Context(), "git", "-C", workspacePath, "branch", "--show-current").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(branch)); got != wantBranch {
		t.Fatalf("workspace branch = %q, want %q", got, wantBranch)
	}
	output, err := os.ReadFile(filepath.Join(workspacePath, "agent-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(output); got != "done\n" {
		t.Fatalf("agent-output.txt = %q, want done", got)
	}

	reopened := hub.reopen(t)
	spend, err := reopened.IssueTokenSpend(ctx, store.IssueIdentity{ProjectID: "local", IssueID: issue.ID, Identifier: issue.Identifier})
	if err != nil {
		t.Fatalf("IssueTokenSpend() error = %v", err)
	}
	if spend.InputTokens != 321 || spend.OutputTokens != 45 || spend.TotalTokens != 366 || spend.Sessions != 1 {
		t.Fatalf("IssueTokenSpend() = %#v, want recorded codex totals", spend)
	}
	if len(spend.ByModel) != 1 || spend.ByModel[0].Model != "gpt-5-codex" {
		t.Fatalf("IssueTokenSpend().ByModel = %#v, want gpt-5-codex", spend.ByModel)
	}
	evidence, err := reopened.RuntimeEvidence(ctx, tracker.NativeWorkItemID(issue.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Attempt == nil || evidence.Attempt.AttemptID == "" {
		t.Fatalf("persisted native attempt missing: %#v", evidence)
	}
	resume, err := api.NewRunnerSessions(reopened).LatestAgentResumeState(ctx, store.AgentResumeLookup{ProjectID: "local", IssueID: issue.ID, Identifier: issue.Identifier, NativeAttemptID: evidence.Attempt.AttemptID})
	if err != nil || resume.DetentSessionID <= 0 {
		t.Fatalf("persisted session lookup = %#v, error = %v", resume, err)
	}
	var snapshot runnersession.Snapshot
	path := "/api/v2/organizations/" + string(hub.organization) + "/projects/" + string(hub.project) + "/runner-sessions/" + strconv.FormatInt(resume.DetentSessionID, 10)
	if err := reopened.Transport().Request(ctx, http.MethodGet, path, nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != resume.DetentSessionID || snapshot.Finish == nil {
		t.Fatalf("persisted session = %#v, want finished session", snapshot)
	}
	receipts := make([]runnersession.Mutation, 0, 3)
	var usage *store.UsageEvent
	for revision := snapshot.Revision; revision > max(int64(0), snapshot.Revision-runnersession.HistoryLimit); revision-- {
		var entry runnersession.Mutation
		if err := reopened.Transport().Request(ctx, http.MethodGet, path+"/history/"+strconv.FormatInt(revision, 10), nil, &entry); err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, entry)
		if usage == nil && entry.Usage != nil {
			usage = entry.Usage
		}
		if len(receipts) >= 3 && runnersession.VerifyLegacyFinalization(snapshot, receipts) == nil {
			break
		}
	}
	if err := runnersession.VerifyLegacyFinalization(snapshot, receipts); err != nil {
		t.Fatalf("persisted finish, usage and phase receipts: %v", err)
	}
	if usage == nil {
		t.Fatal("persisted usage receipt missing")
	}
	if usage.InputTokens-usage.CachedInputTokens+usage.OutputTokens != 66 || math.Abs(usage.CostUSD-0.501) > 0.000001 {
		t.Fatalf("persisted usage = %#v, want 66 uncached input plus output tokens and $0.501", usage)
	}

}

type nativeE2EAgentBackend struct {
	inputTokens       int64
	cachedInputTokens int64
	outputTokens      int64
	totalTokens       int64
	lastInputTokens   int64
	lastCachedTokens  int64
	lastOutputTokens  int64
	lastTotalTokens   int64
}

func (c *nativeE2EAgentBackend) RunTurn(_ context.Context, req runpkg.AgentTurnRequest, onUpdate runpkg.AgentUpdateHandler) (runpkg.AgentTurnResult, error) {
	if handled, err := testsupport.ReplyToRoundVerdict(req, onUpdate); handled {
		return runpkg.AgentTurnResult{}, err
	}
	if strings.TrimSpace(req.Workspace) == "" {
		return runpkg.AgentTurnResult{}, errors.New("workspace is required")
	}
	if err := os.WriteFile(filepath.Join(req.Workspace, "agent-output.txt"), []byte("done\n"), 0o600); err != nil {
		return runpkg.AgentTurnResult{}, fmt.Errorf("write agent output: %w", err)
	}
	if onUpdate != nil {
		threadTotal := &runpkg.AgentTokenCounts{
			InputTokens:       c.inputTokens,
			CachedInputTokens: c.cachedInputTokens,
			OutputTokens:      c.outputTokens,
			TotalTokens:       c.totalTokens,
		}
		if err := onUpdate(runpkg.AgentUpdate{
			Type: runpkg.AgentUpdateTokenUsage,
			Tokens: runpkg.AgentTokenUsage{
				InputTokens:       c.inputTokens,
				CachedInputTokens: c.cachedInputTokens,
				OutputTokens:      c.outputTokens,
				TotalTokens:       c.totalTokens,
				ThreadTotal:       threadTotal,
				Last: &runpkg.AgentTokenCounts{
					InputTokens:       c.lastInputTokens,
					CachedInputTokens: c.lastCachedTokens,
					OutputTokens:      c.lastOutputTokens,
					TotalTokens:       c.lastTotalTokens,
				},
			},
		}); err != nil {
			return runpkg.AgentTurnResult{}, err
		}
	}
	return runpkg.AgentTurnResult{ThreadID: "thread-e2e", TurnID: "turn-e2e", SessionID: "thread-e2e-turn-e2e"}, nil
}

func waitForNativeE2ERetriedIssue(t *testing.T, orch *orchestrator.Orchestrator, issueID string) orchestrator.State {
	t.Helper()
	var state orchestrator.State
	testenv.WaitUntil(t, t.Context(), func() error {
		var err error
		state, err = orch.State(t.Context())
		if err != nil {
			return err
		}
		if _, ok := state.Retry[issueID]; !ok {
			return fmt.Errorf("issue %s has not retried", issueID)
		}
		return nil
	})
	return state
}

func assertNativeE2EOrchestratorStopped(t *testing.T, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("orchestrator Run() error = %v, want context canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("timed out waiting for orchestrator to stop")
	}
}
