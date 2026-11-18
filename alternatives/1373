package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDependencyReasonRefresh(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{true, false} {
		name := "source_first"
		if !sourceFirst {
			name = "target_first"
		}
		t.Run(name, func(t *testing.T) {
			hub := newNativeChangeHub(t, true)
			requests := fixtureClient(hub.admin.Transport()).requests
			transport := requests.Load()
			requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(request)
				item := strings.TrimPrefix(request.URL.Path, hub.admin.Scope()+"/work-items/")
				if err != nil || response.StatusCode != http.StatusOK || request.Method != http.MethodGet || item == request.URL.Path || strings.Contains(item, "/") || request.URL.Query().Has("view") {
					return response, err
				}
				var issue tracker.NativeIssue
				decodeErr := json.NewDecoder(response.Body).Decode(&issue)
				if err := errors.Join(decodeErr, response.Body.Close()); err != nil {
					return nil, err
				}
				issue.OmittedFields = append(issue.OmittedFields, "blockers", "dependents", "blocked_reason", "unblocks_reason")
				issue.Dependencies, issue.Blockers, issue.Dependents = nil, nil, nil
				issue.BlockedReason, issue.UnblocksReason = tracker.DependencyReason{}, tracker.DependencyReason{}
				payload, err := json.Marshal(issue)
				if err != nil {
					return nil, err
				}
				response.Body, response.ContentLength = io.NopCloser(bytes.NewReader(payload)), int64(len(payload))
				response.Header.Del("Content-Length")
				return response, nil
			}))
			dependencyIssue := func(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeIssue, error) {
				recovery, err := hub.native.Recovery(ctx, id)
				return recovery.Issue, err
			}
			first, second := hub.createInProgress(t, "First feature"), hub.createInProgress(t, "Shared foundation")
			if (first.ID < second.ID) != sourceFirst {
				first, second = second, first
			}
			dependent, err := hub.native.Issue(t.Context(), tracker.NativeWorkItemID(first.ID))
			if err != nil {
				t.Fatal(err)
			}
			blocker := tracker.NativeWorkItemID(second.ID)
			calls := 0
			generate := func(_ context.Context, prompt string) (string, error) {
				calls++
				if !strings.Contains(prompt, "First feature") || !strings.Contains(prompt, "Shared foundation") {
					t.Fatalf("reason prompt omitted authorized issue context")
				}
				return "Waits for the shared foundation.", nil
			}
			ctx := runner.WithDependencyReasonGenerator(t.Context(), generate)
			changed, err := hub.admin.Dependency(ctx, dependent.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker, Operation: "add"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []tracker.NativeWorkItemID{dependent.WorkItemID, blocker} {
				issue, err := dependencyIssue(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if issue.BlockedReason.Basis != issue.BlockedReason.CurrentBasis || issue.UnblocksReason.Basis != issue.UnblocksReason.CurrentBasis {
					t.Fatalf("neighbor not refreshed: %+v", issue)
				}
			}
			if calls != 2 {
				t.Fatalf("generated %d sentences, want two stale edge directions", calls)
			}
			if err := hub.admin.RefreshDependencyReasons(ctx, dependent.WorkItemID, generate); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("current bases regenerated: %d calls", calls)
			}
			if _, err := hub.admin.Dependency(ctx, dependent.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: changed.Revision, RelatedWorkItemID: blocker, Operation: "remove"}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []tracker.NativeWorkItemID{dependent.WorkItemID, blocker} {
				issue, err := dependencyIssue(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if issue.BlockedReason.Text != "" || issue.UnblocksReason.Text != "" || issue.BlockedReason.Basis != issue.BlockedReason.CurrentBasis || issue.UnblocksReason.Basis != issue.UnblocksReason.CurrentBasis {
					t.Fatalf("removed edge reason retained: %+v", issue)
				}
			}
			if calls != 2 {
				t.Fatalf("empty edges called model: %d", calls)
			}
			current, err := dependencyIssue(ctx, dependent.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			raceCalls := 0
			racing := runner.WithDependencyReasonGenerator(t.Context(), func(ctx context.Context, prompt string) (string, error) {
				raceCalls++
				if strings.Contains(prompt, "explaining blocked:") {
					issue, err := dependencyIssue(ctx, dependent.WorkItemID)
					if err != nil {
						return "", err
					}
					path, err := nativeItemPath(dependent.WorkItemID)
					if err != nil {
						return "", err
					}
					request := tracker.UpdateDependencyReason{Mutation: nativeMutationKey(), WorkItemID: dependent.WorkItemID, Kind: "blocked", Text: "Uses the first accepted sentence.", Basis: issue.BlockedReason.CurrentBasis, ExpectedRevision: issue.BlockedReason.Revision}
					var result tracker.DependencyReason
					if err := hub.admin.Transport().Request(ctx, http.MethodPost, hub.native.Scope()+path+"/dependency-reasons", request, &result); err != nil {
						return "", err
					}
				}
				return "Uses the competing generated sentence.", nil
			})
			if _, err := hub.admin.Dependency(racing, dependent.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, RelatedWorkItemID: blocker, Operation: "add"}); err != nil {
				t.Fatal(err)
			}
			current, err = dependencyIssue(ctx, dependent.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			if current.BlockedReason.Text != "Uses the first accepted sentence." || raceCalls != 2 {
				inverse, inverseErr := dependencyIssue(ctx, blocker)
				t.Fatalf("competing writer did not converge: %+v, generations=%d inverse=%+v links=%+v error=%v", current.BlockedReason, raceCalls, inverse.UnblocksReason, inverse.Dependents, inverseErr)
			}
		})
	}
}

func TestNativeOutcomeRefreshesDependencyReasonsAndUsage(t *testing.T) {
	t.Parallel()
	hub := newNativeChangeHub(t, true)
	source := hub.createInProgress(t, "Dependent feature")
	blocker, err := hub.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Foundation", Body: "Foundation context", State: "Done"})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := hub.native.Issue(t.Context(), tracker.NativeWorkItemID(source.ID))
	if err != nil {
		t.Fatal(err)
	}
	claimed := hub.claim(t, source.ID)
	execution := hub.scheduler.RunExecution(claimed.ID).(*nativeExecution)
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	dependent, err = hub.native.Issue(t.Context(), dependent.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.admin.Dependency(t.Context(), dependent.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	execution.SetDependencyReasonGenerator(func(ctx context.Context, _ string) (string, error) {
		calls++
		if err := execution.RecordUsage(ctx, tracker.NativeUsage{Provider: "codex", Model: "test", Input: 10, Output: 2, Currency: "USD"}); err != nil {
			return "", err
		}
		return "Enables the dependent feature.", nil
	})
	if err := execution.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	current, err := hub.native.Recovery(t.Context(), dependent.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := hub.native.Recovery(t.Context(), blocker.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || current.Issue.BlockedReason.Text == "" || neighbor.Issue.UnblocksReason.Text == "" {
		t.Fatalf("outcome did not refresh endpoints: calls=%d reasons=%+v %+v", calls, current.Issue.BlockedReason, neighbor.Issue.UnblocksReason)
	}
	attempts, err := hub.admin.AttemptsDetailPage(t.Context(), dependent.WorkItemID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Items) != 1 || len(attempts.Items[0].Usage) != 1 || attempts.Items[0].Usage[0].Input != 20 {
		t.Fatalf("reason usage not persisted: %+v", attempts.Items)
	}
}
