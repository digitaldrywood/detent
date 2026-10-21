package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubgithub"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type releaseStatusTransport struct {
	t                                    *testing.T
	directory                            string
	input                                tracker.ReleasePickInput
	closed, moved, truncated, wrongPatch bool
	reads                                int
}

func (f *releaseStatusTransport) git(ctx context.Context, args ...string) string {
	f.t.Helper()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", f.directory, "-c", "user.name=Test User", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func (f *releaseStatusTransport) REST(ctx context.Context, method, path string, _ any, out any) error {
	if method != http.MethodGet {
		return errors.New("unexpected release write")
	}
	i := f.input
	root := "/repos/example-org/example-project"
	var response any
	switch {
	case path == root+"/git/ref/heads/release%2Fv1.2":
		f.reads++
		head := f.git(ctx, "rev-parse", i.BaseRef)
		if f.moved && f.reads > 1 {
			head = i.FrozenSHA
		}
		response = map[string]any{"object": map[string]string{"sha": head}}
	case strings.HasPrefix(path, root+"/compare/"):
		refs := strings.Split(strings.Split(strings.TrimPrefix(path, root+"/compare/"), "?")[0], "...")
		base, head := refs[0], f.git(ctx, "rev-parse", refs[1])
		mergeBase := f.git(ctx, "merge-base", base, head)
		commits := []any{}
		for _, sha := range strings.Fields(f.git(ctx, "rev-list", base+".."+head)) {
			commits = append(commits, map[string]any{"sha": sha, "parents": []any{map[string]string{"sha": f.git(ctx, "rev-parse", sha+"^")}}})
		}
		status := "diverged"
		if head == base {
			status = "identical"
		} else if mergeBase == base {
			status = "ahead"
		} else if mergeBase == head {
			status = "behind"
		}
		response = map[string]any{"status": status, "merge_base_commit": map[string]string{"sha": mergeBase}, "commits": commits}
	case strings.HasPrefix(path, root+"/commits/"):
		sha := strings.Split(strings.TrimPrefix(path, root+"/commits/"), "?")[0]
		patch := f.git(ctx, "show", "--format=", sha, "--", "repair.txt")
		if f.wrongPatch && sha != i.SourceMergeSHA {
			patch += "\n+forged change"
		}
		response = map[string]any{"sha": sha, "parents": []any{map[string]string{"sha": f.git(ctx, "rev-parse", sha+"^")}}, "commit": map[string]string{"message": f.git(ctx, "show", "-s", "--format=%B", sha)}, "files": []any{map[string]string{"filename": "repair.txt", "status": "modified", "patch": patch}}}
	case path == root+"/pulls/7":
		state := "open"
		if f.closed {
			state = "closed"
		}
		ref := func(name, sha string) any {
			return map[string]any{"ref": name, "sha": sha, "repo": map[string]string{"full_name": "example-org/example-project"}}
		}
		response = map[string]any{"number": 7, "state": state, "merged": false, "head": ref(i.HeadRef, i.HeadSHA), "base": ref(i.BaseRef, i.BaseSHA)}
	default:
		return fmt.Errorf("unexpected release read: %s", path)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (f *releaseStatusTransport) RESTPage(ctx context.Context, path string, out any) (string, error) {
	err := f.REST(ctx, http.MethodGet, path, nil, out)
	if f.truncated && strings.Contains(path, "/compare/") {
		return "next-page", err
	}
	return "", err
}

func TestNativeReleaseDeliveryReconciliation(t *testing.T) {
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name                                                                            string
		delivered, closed, noTrailer, wrongPatch, moved, truncated, badFrozen, unmerged bool
	}{
		{name: "cherry-picked source completes without provider", delivered: true, closed: true},
		{name: "patch identity completes without trailer", delivered: true, noTrailer: true},
		{name: "closed missing source asks human", closed: true},
		{name: "open missing source still starts provider"},
		{name: "forged trailer cannot complete", delivered: true, wrongPatch: true},
		{name: "moving release branch cannot complete", delivered: true, moved: true},
		{name: "truncated missing-source history cannot request human", closed: true, truncated: true},
		{name: "merged develop violates frozen release", delivered: true, badFrozen: true},
		{name: "unmerged source cannot complete", delivered: true, unmerged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &releaseStatusTransport{t: t, directory: t.TempDir(), closed: test.closed, wrongPatch: test.wrongPatch, moved: test.moved, truncated: test.truncated}
			f.git(t.Context(), "init", "-b", "develop")
			file := filepath.Join(f.directory, "repair.txt")
			if err := os.WriteFile(file, []byte("old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(t.Context(), "add", "repair.txt")
			f.git(t.Context(), "commit", "-m", "freeze release")
			frozen := f.git(t.Context(), "rev-parse", "HEAD")
			if err := os.WriteFile(file, []byte("new\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(t.Context(), "add", "repair.txt")
			f.git(t.Context(), "commit", "-m", "source repair")
			source := f.git(t.Context(), "rev-parse", "HEAD")
			f.git(t.Context(), "checkout", "-b", "pick/repair", frozen)
			f.git(t.Context(), "cherry-pick", "-x", source)
			deliveryHead := f.git(t.Context(), "rev-parse", "HEAD")
			f.git(t.Context(), "checkout", "-b", "release/v1.2", frozen)
			if test.delivered {
				args := []string{"cherry-pick", "-x", source}
				if test.noTrailer {
					args = []string{"cherry-pick", source}
				}
				f.git(t.Context(), args...)
				if test.noTrailer {
					f.git(t.Context(), "commit", "--amend", "-m", "delivery without trailer")
				} else {
					f.git(t.Context(), "commit", "--amend", "-m", "alternate delivery\n\n(cherry picked from commit "+source+")")
				}
			}
			if test.badFrozen {
				f.git(t.Context(), "reset", "--hard", source)
			}
			if test.unmerged {
				f.git(t.Context(), "branch", "-f", "develop", frozen)
			}
			f.input = tracker.ReleasePickInput{SourceIssueID: "wi_source", SourceChangeID: "change_source", SourceMergeSHA: source, Repository: "https://github.com/example-org/example-project", PRNumber: 7, HeadRef: "pick/repair", HeadSHA: deliveryHead, BaseRef: "release/v1.2", BaseSHA: frozen, FrozenSHA: frozen}
			f.git(t.Context(), "config", "url."+f.directory+".insteadOf", f.input.Repository+".git")
			h := newNativeChangeHubTransport(t, "Blocked", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Rework", "Done"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Done"}},
				{Name: "Blocked", Transitions: []string{"Rework"}}, {Name: "Done", Terminal: true},
			}, true)
			issue := h.createInProgress(t, "release repair")
			if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Rework"); err != nil {
				t.Fatal(err)
			}
			candidates := h.candidatesIn(t, "Rework")
			if len(candidates) != 1 {
				t.Fatalf("candidates=%+v", candidates)
			}
			claimed, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.scheduler.now())
			if err != nil {
				t.Fatal(err)
			}
			issue = claimed.Issue
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			execution.claim.recovery.Issue.ReleaseDelivery = &tracker.ReleaseDelivery{ReleasePickInput: f.input, ChangeID: "change_delivery"}
			transport := h.native.client.httpClient.Transport
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/release-pick") {
					status, err := hubgithub.NewReconciler(f).ReleaseDeliveryStatus(request.Context(), f.input)
					if err != nil {
						return nil, err
					}
					raw, err := json.Marshal(status)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: request}, err
				}
				return transport.RoundTrip(request)
			})
			provider := &committingAgent{}
			cfg := config.Config{}
			cfg.Tracker.Kind = config.TrackerHubNative
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: f.directory, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, ProjectID: "local", Workflow: config.Workflow{Config: cfg, Prompt: "Complete release repair"}, Workspace: backend, AgentBackend: provider, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			request := runner.RunRequest{Execution: execution, ProjectID: "local", Issue: issue, Mode: runner.RunModeImplement}
			if test.wrongPatch || test.moved || test.truncated || test.badFrozen || test.unmerged || !test.delivered && !test.closed {
				result, err := agent.Run(t.Context(), request)
				if test.moved || test.truncated || test.badFrozen || test.unmerged {
					if err == nil || provider.calls != 0 || result.TurnStarted {
						t.Fatalf("unsafe verification: %+v calls=%d error=%v", result, provider.calls, err)
					}
				} else if provider.calls == 0 || h.state(t, issue.ID) == "Done" || result.NativeLanding != nil {
					t.Fatalf("missing source completed: %+v error=%v", result, err)
				}
				return
			}
			target := "Done"
			if !test.delivered {
				target = "Blocked"
			}
			result, err := runNativeCompletionOrchestrator(t, h, agent, request, target)
			if err != nil || provider.calls != 0 || result.TurnStarted {
				t.Fatalf("reconciliation ran provider: %+v calls=%d error=%v", result, provider.calls, err)
			}
			if test.delivered && (result.NativeLanding == nil || result.NativeLanding.MergeSHA == deliveryHead || result.NativeLanding.HeadSHA != source) {
				t.Fatalf("delivery did not reconcile the alternate source pick: %+v", result.NativeLanding)
			}
			if test.closed && !test.delivered {
				disposition := runner.NativeDispositionFromMessage(result.FinalMessage)
				if disposition == nil || !disposition.HumanAction || disposition.Status != "blocked" {
					t.Fatalf("missing human action: %+v", disposition)
				}
			}
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil || current.Terminal != test.delivered {
				t.Fatalf("delivery terminal=%t error=%v", current.Terminal, err)
			}
			if test.closed && !test.delivered {
				decision := current.DecisionRequest
				if decision == nil || decision.AttemptID == "" || decision.Question != "How will you restore this release delivery?" || current.HumanAction == "" {
					t.Fatalf("closed delivery decision = %+v", decision)
				}
				if err := decision.Validate(); err != nil {
					t.Fatalf("closed delivery decision is invalid: %v", err)
				}
			}
			if candidates := h.candidatesIn(t, "Rework", "In Progress", "Todo"); len(candidates) != 0 {
				t.Fatalf("delivery redispatched: %+v", candidates)
			}
			attempts, err := h.admin.AttemptsDetailPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 100)
			if err != nil || len(attempts.Items) != 1 || attempts.Items[0].Runtime == nil || attempts.Items[0].Runtime.Identity.BackendKind != "git" {
				t.Fatalf("unexpected provider attempt: %+v error=%v", attempts, err)
			}
		})
	}
}
