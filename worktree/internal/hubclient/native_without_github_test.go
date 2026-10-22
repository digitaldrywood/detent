package hubclient

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type deniedGitHubTransport struct {
	calls atomic.Int64
}

func (t *deniedGitHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, http.ErrUseLastResponse
}

type environmentRecordingAgent struct {
	committingAgent
	requests []runner.AgentTurnRequest
}

func (a *environmentRecordingAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	a.requests = append(a.requests, request)
	return a.committingAgent.RunTurn(ctx, request, onUpdate)
}

func (a *environmentRecordingAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	a.requests = append(a.requests, request)
	return a.committingAgent.RunTurnWithTools(ctx, request, tools, handler, update)
}

func TestNativeRunnerReachesHumanReviewWithoutGitHub(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	isolateNativeChangeGit(t)
	bin := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + ghLog + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "process-canary")
	t.Setenv("GITHUB_TOKEN", "process-canary")
	denied := &deniedGitHubTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = denied
	t.Cleanup(func() { http.DefaultTransport = previous })

	for _, name := range []string{"code", "linked source"} {
		t.Run(name, func(t *testing.T) {
			var h *nativeChangeHub
			var issue connector.Issue
			if name == "linked source" {
				var linked tracker.NativeIssue
				h, linked = newLinkedChangeHub(t)
				h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
					t.Error("runner fetched GitHub source after Hub intake")
					return tracker.GitHubIssueSnapshot{}, errors.New("runner must not fetch a source after Hub intake")
				}
				issue = issueFromNative(linked)
			} else {
				h = newNativeChangeHubWithStates(t, "Human Review", hubserver.HostedProjectStates())
				issue = h.createInProgress(t, "Update the README")
			}
			candidate := h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: nativeChangeSourceRepo(t), AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{
				Tracker:     config.Tracker{Kind: config.TrackerHubNative, Repository: "acme/orders"},
				Deliverable: config.Deliverable{Kind: config.DeliverablePullRequest},
				Gate:        gate.Config{Run: "true"},
			}.WithRuntimeGitHubToken("instance-canary")
			agent := &environmentRecordingAgent{committingAgent: committingAgent{commit: true}}
			run, err := runner.NewRunner(runner.Dependencies{
				Workflow:     config.Workflow{Config: cfg, Prompt: "Keep the Codex Workpad current and open a pull request."},
				Workspace:    backend,
				AgentBackend: agent,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := run.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.FinalState != runner.FinalStateCompleted {
				t.Fatalf("final state = %q", result.FinalState)
			}
			change := result.NativeChange
			if change == nil || !change.Changed || change.ChangeID == "" {
				t.Fatalf("native change = %#v, want an opened Change Request", change)
			}
			h.complete(t, issue.ID, change)
			if state := h.state(t, issue.ID); state != "Human Review" {
				t.Fatalf("state = %s, want Human Review", state)
			}
			if changes := h.changes(t, issue.ID); len(changes) != 1 || changes[0].ID != change.ChangeID {
				t.Fatalf("changes = %#v, reported %#v", changes, change)
			}
			if len(agent.requests) == 0 {
				t.Fatal("agent never ran")
			}
			for _, request := range agent.requests {
				if request.DeliverableKind != "" || request.DeliverableRepository != "" {
					t.Errorf("native turn carried a pull request deliverable: %q %q", request.DeliverableKind, request.DeliverableRepository)
				}
				for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
					if value, ok := request.Environment.Variables[key]; !ok || value != "" {
						t.Errorf("%s reaches the agent: set=%t", key, ok)
					}
				}
				if dir := request.Environment.Variables["GH_CONFIG_DIR"]; dir != "" {
					if _, err := os.Stat(filepath.Join(dir, "hosts.yml")); !os.IsNotExist(err) {
						t.Errorf("agent gh config holds a credential: %v", err)
					}
				}
				if !strings.Contains(request.Prompt, "## Native completion contract") {
					t.Error("native turn prompt has no native completion contract")
				}
			}
			if calls := denied.calls.Load(); calls != 0 {
				t.Errorf("native run made %d default-transport HTTP calls", calls)
			}
			if data, err := os.ReadFile(ghLog); err == nil && len(data) > 0 {
				t.Errorf("native run invoked gh: %s", data)
			}
		})
	}
}
