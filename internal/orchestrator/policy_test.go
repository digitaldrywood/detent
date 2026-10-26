package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func orchestratorTestPolicy() policy.Descriptor {
	return policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), AutomatedReview: "off", MergeMethod: "squash"}}.WithID()
}

func orchestratorTestAuthoredPolicy(workflow string) policy.Descriptor {
	descriptor := orchestratorTestPolicy()
	digest := policy.Digest([]byte(workflow))
	descriptor.Authored = &policy.Authored{Version: 1, Digest: digest, Files: map[string]string{"WORKFLOW.md": workflow}}
	descriptor.SourceDigest, descriptor.ConfigDigest, descriptor.SourceRevision = digest, digest, digest
	return descriptor.WithID()
}

func TestOrphanRecoveryRetainsPolicyAcrossHubTransition(t *testing.T) {
	t.Parallel()
	descriptor := orchestratorTestAuthoredPolicy("original workflow")
	changed := orchestratorTestAuthoredPolicy("changed workflow")
	for _, test := range []struct {
		name            string
		pinned, current policy.Descriptor
		metadata        string
		resume          bool
		detached        bool
		workerHost      string
	}{
		{name: "same Hub policy", pinned: descriptor, current: descriptor, resume: true},
		{name: "changed workflow", pinned: descriptor, current: changed, resume: true},
		{name: "changed workflow queued grant", pinned: descriptor, current: changed, resume: true, detached: true},
		{name: "native changed workflow on another host", pinned: descriptor, current: changed, resume: true, detached: true, workerHost: "worker-b"},
		{name: "Hub disabled", pinned: descriptor, resume: true},
		{name: "Hub enabled without pinned identity", current: descriptor},
		{name: "missing metadata", current: changed, metadata: "{}"},
		{name: "unreadable metadata", current: changed, metadata: "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			issue := orphanRecoveryIssue()
			issue.Identifier = "example-org/example-project#1155"
			issue.URL = "https://github.com/example-org/example-project/issues/1155"
			session := orphanRecoverySession(now)
			session.Identifier, session.IssueURL = issue.Identifier, issue.URL
			metadata := test.metadata
			if metadata == "" {
				metadata = runningWorkAttemptMetadataJSON(Running{Policy: test.pinned}, nil)
			}
			attempts := &orphanRecoveryAttemptStore{metadata: metadata}
			runner := newWorkerHostRunner()
			var tracker connector.Connector = hydratingDispatchConnector{issue: issue}
			workerHost := "local"
			workerHosts := []string{workerHost}
			if test.workerHost != "" {
				workerHost = test.workerHost
				workerHosts = append(workerHosts, workerHost)
				tracker = &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			}
			o, err := New(Config{
				Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, Policy: test.current,
				WorkerHosts: workerHosts,
			}, Dependencies{Connector: tracker, WorkAttempts: attempts, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(o.cfg)
			o.recoverOrphanedAgentSessions(t.Context(), &state, []store.OrphanedAgentSession{session}, now)
			retry, resumed := state.Retry[issue.ID]
			if resumed != test.resume {
				t.Fatalf("resumed = %t, want %t", resumed, test.resume)
			}
			if !resumed {
				return
			}
			if test.detached {
				delete(state.Retry, issue.ID)
				if !o.dispatchIssueWithAdmission(t.Context(), &state, issue, retry.Attempt, now, workerHost, false, &retry).dispatched {
					t.Fatal("queued orphan resume did not dispatch")
				}
			} else if !o.dispatchIssue(t.Context(), &state, issue, retry.Attempt, now, "local") {
				t.Fatal("orphan resume did not dispatch")
			}
			t.Cleanup(state.Running[issue.ID].cancel)
			request := receiveWorkerHostRunRequest(t, runner.started)
			if request.WorkerHost != workerHost {
				t.Fatalf("worker host = %q, want %q", request.WorkerHost, workerHost)
			}
			if state.Running[issue.ID].ClaimPolicy.ID != test.current.ID {
				t.Fatalf("claim policy = %q, want %q", state.Running[issue.ID].ClaimPolicy.ID, test.current.ID)
			}
			if request.RetryMode != runpkg.RetryModeResume || request.ResumeState.DetentSessionID != session.ResumeState.DetentSessionID || request.Policy.ID != test.pinned.ID || state.Running[issue.ID].Policy.ID != test.pinned.ID {
				t.Fatalf("continuation identity = %+v, running policy = %q; want pinned policy %q", request, state.Running[issue.ID].Policy.ID, test.pinned.ID)
			}
			var persisted struct {
				Policy policy.Descriptor `json:"policy"`
			}
			if err := json.Unmarshal([]byte(attempts.starts[0].WorkerMetadataJSON), &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.Policy.ID != test.pinned.ID {
				t.Fatalf("persisted continuation policy = %q, want %q", persisted.Policy.ID, test.pinned.ID)
			}
		})
	}
}

type checkedPolicyScheduling struct {
	SchedulingSource
	approved policy.Descriptor
}

type preparingProjectScheduling struct {
	SchedulingSource
	err error
}

func (s preparingProjectScheduling) PrepareProject(context.Context, string) error {
	return s.err
}

func TestProjectSetupFailurePreservesIssueAccounting(t *testing.T) {
	t.Parallel()
	for _, granted := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready candidates", true: "granted dispatch"}[granted], func(t *testing.T) {
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "project-one"}})
			state := newState(cfg)
			issue := dispatchTestIssue("issue-one", "Todo")
			state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2}
			beforeRetry := state.Retry[issue.ID]
			beforeBreaker := cloneProjectFailureBreaker(state.FailureBreaker)
			attempts := &recordingWorkAttemptStore{}
			source := &hubSchedulingSource{}
			orch := Orchestrator{cfg: cfg, workAttempts: attempts, scheduling: preparingProjectScheduling{SchedulingSource: source, err: errors.New("instance project setup exited 7")}}
			if granted {
				outcome := orch.dispatchIssueWithGlobalGrant(t.Context(), &state, issue, 2, time.Now(), "", false, true, nil, &scheduler.DispatchResult{})
				if outcome.dispatched || outcome.reason != dispatchIssueFailureWorkerHostUnavailable {
					t.Fatalf("dispatch outcome = %+v", outcome)
				}
			} else {
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
				if source.releases != 1 || len(attempts.decisions) != 1 || attempts.decisions[0].Reason != dispatchIssueFailureWorkerHostUnavailable || attempts.decisions[0].WaitReason != "instance project setup exited 7" {
					t.Fatalf("preparation refusal was not released and recorded: releases=%d decisions=%+v", source.releases, attempts.decisions)
				}
			}
			if !reflect.DeepEqual(state.Retry[issue.ID], beforeRetry) || !reflect.DeepEqual(state.FailureBreaker, beforeBreaker) || len(state.Running) != 0 || len(state.Claimed) != 0 || len(attempts.starts) != 0 {
				t.Fatalf("unexpected state after completion: retry=%+v blocked=%+v breaker=%+v", state.Retry, state.Blocked, state.FailureBreaker)
			}
		})
	}
}

func (s checkedPolicyScheduling) CheckProjectPolicy(_ context.Context, _, _ string, descriptor policy.Descriptor) error {
	return descriptor.Match(s.approved)
}

func TestPolicyMismatchStopsDispatchWithoutAttempt(t *testing.T) {
	t.Parallel()
	descriptor := orchestratorTestPolicy()
	for _, test := range []struct {
		name   string
		actual policy.Descriptor
	}{
		{"missing", policy.Descriptor{}},
		{"stale", func() policy.Descriptor { d := descriptor; d.Gates.AutoPromote = true; return d.WithID() }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := &Orchestrator{cfg: Config{Policy: test.actual}, scheduling: checkedPolicyScheduling{approved: descriptor}}
			out := o.dispatchIssueWithAdmission(t.Context(), nil, connector.Issue{}, 1, time.Now(), "", false, nil)
			if out.reason != "policy_mismatch" || !strings.Contains(out.waitReason, "policy_mismatch") {
				t.Fatalf("dispatch = %#v", out)
			}
		})
	}
}

func TestAttemptMetadataRetainsApprovedPolicy(t *testing.T) {
	t.Parallel()
	descriptor := orchestratorTestPolicy()
	deferredMetadata, err := deferredCompletionMetadataJSON(deferredCompletion{Running: Running{Policy: descriptor}})
	if err != nil {
		t.Fatal(err)
	}
	stale := descriptor
	stale.Gates.AutoPromote = true
	staleMetadata, err := deferredCompletionMetadataJSON(deferredCompletion{Running: Running{Policy: stale.WithID()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, raw string
		want      policy.Descriptor
		valid     bool
	}{
		{"active", runningWorkAttemptMetadataJSON(Running{Policy: descriptor}, nil), descriptor, true},
		{"deferred", deferredMetadata, descriptor, true},
		{"changed deferred policy", staleMetadata, stale.WithID(), true},
		{"forged completion metadata", runningWorkAttemptMetadataJSON(Running{Policy: descriptor}, map[string]any{"policy": policy.Descriptor{}}), descriptor, true},
		{"missing", "{}", policy.Descriptor{}, false}, {"invalid", "{", policy.Descriptor{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := persistedAttemptPolicy(store.WorkAttempt{WorkerMetadataJSON: test.raw})
			if valid != test.valid || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("persisted identity valid=%t, want %t", valid, test.valid)
			}
		})
	}
}
