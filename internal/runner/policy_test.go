package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func runnerTestPolicy() policy.Descriptor {
	return policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), AutomatedReview: "off", MergeMethod: "squash"}}.WithID()
}

type policySessionStore struct {
	SessionStore
	policy policy.Descriptor
	err    error
}

func (s policySessionStore) SessionPolicy(context.Context, string, int64) (policy.Descriptor, error) {
	return s.policy, s.err
}

func TestResumeRetainsIdentityAcrossConfigurationChanges(t *testing.T) {
	t.Parallel()
	approved := runnerTestPolicy()
	changed := approved
	changed.Gates.MergeMethod = "merge"
	changed = changed.WithID()
	for _, test := range []struct {
		name    string
		backend SessionStore
		request policy.Descriptor
		session int64
		valid   bool
	}{
		{"fresh", nil, approved, 0, true},
		{"standalone", nil, policy.Descriptor{}, 1, true},
		{"legacy standalone session", policySessionStore{}, policy.Descriptor{}, 1, true},
		{"Hub disabled after pinning", policySessionStore{policy: approved}, policy.Descriptor{}, 1, true},
		{"legacy session moved to Hub", policySessionStore{}, approved, 1, true},
		{"same policy", policySessionStore{policy: approved}, approved, 1, true},
		{"changed policy", policySessionStore{policy: approved}, changed, 1, true},
		{"missing store", nil, approved, 1, false},
		{"missing identity", policySessionStore{err: errors.New("missing session")}, approved, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runner{store: test.backend}
			err := r.checkResumePolicy(t.Context(), RunRequest{ProjectID: "one", Policy: test.request}, store.AgentResumeState{DetentSessionID: test.session})
			if (err == nil) != test.valid {
				t.Fatalf("resume error=%v, valid=%t", err, test.valid)
			}
		})
	}
}

func TestHubResumeRequiresStoredIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		resume store.AgentResumeState
		valid  bool
	}{
		{"fresh", store.AgentResumeState{}, true},
		{"thread without stored session", store.AgentResumeState{ProviderThreadID: "thread"}, false},
		{"provider session without stored session", store.AgentResumeState{ProviderSessionID: "session"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runner{}
			err := r.checkResumePolicy(t.Context(), RunRequest{Policy: runnerTestPolicy()}, test.resume)
			if (err == nil) != test.valid {
				t.Fatalf("resume error = %v, valid=%t", err, test.valid)
			}
		})
	}
}

type workflowReloadExecution struct {
	testExecution
	reload func()
}

func (e *workflowReloadExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	e.reload()
	return e.testExecution.Guard(ctx)
}

func TestRunKeepsPinnedPromptAcrossReload(t *testing.T) {
	t.Parallel()
	makeWorkflow := func(prompt string) config.Workflow {
		w := config.Workflow{Config: config.Default().ForNativeTracker(), Prompt: prompt, SharedPrompt: prompt}
		descriptor, err := config.ResolvePolicy(w)
		if err != nil {
			t.Fatal(err)
		}
		w.Config.Policy = descriptor
		return w
	}
	before := makeWorkflow("First approved instructions.")
	after := makeWorkflow("Second approved instructions.")
	agent := &fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, Delta: "Completed the work."}, {Type: AgentUpdateTurnCompleted, Status: "completed"}}}
	installed := before
	installed.Prompt, installed.SharedPrompt = "Stale installed instructions.", "Stale installed instructions."
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workflow: installed, Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "work", Branch: "work"}}, AgentBackend: agent})
	if err != nil {
		t.Fatal(err)
	}
	execution := &workflowReloadExecution{reload: func() {
		if err := r.UpdateWorkflowChecked(after); err != nil {
			t.Fatal(err)
		}
	}}
	req := RunRequest{Policy: before.Config.Policy, Execution: execution, Issue: connector.Issue{ID: "work", Identifier: "work-1"}, Mode: RunModeImplement}
	if _, err := r.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !strings.Contains(agent.request.Prompt, before.Prompt) || strings.Contains(agent.request.Prompt, after.Prompt) || strings.Contains(agent.request.Prompt, installed.Prompt) {
		t.Fatal("provider did not receive the instructions pinned before reload")
	}
	if _, err := r.Run(t.Context(), req); !errors.Is(err, ErrExecutionAuthorityUnavailable) || agent.calls != 1 {
		t.Fatalf("stale request started a provider turn or lost its refusal: turns=%d err=%v", agent.calls, err)
	}
	if _, err := r.CheckProgress(t.Context(), req, progresscheck.Request{}); !errors.Is(err, ErrExecutionAuthorityUnavailable) || agent.calls != 1 {
		t.Fatalf("stale progress check started a provider turn or lost its refusal: turns=%d err=%v", agent.calls, err)
	}
	execution.recovery.Lease.ID = "lease-example"
	execution.recovery.Lease.PolicyID = before.Config.Policy.ID
	for _, claimed := range []policy.Descriptor{{}, after.Config.Policy} {
		req.Policy = claimed
		if _, err := r.Run(t.Context(), req); !errors.Is(err, ErrExecutionAuthorityUnavailable) || agent.calls != 1 {
			t.Fatalf("request ignored the lease policy: turns=%d err=%v", agent.calls, err)
		}
	}
}
