package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
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

func TestRunRejectsMismatchedPolicyBeforeWorkspace(t *testing.T) {
	t.Parallel()
	approved := runnerTestPolicy()
	changed := approved
	changed.Gates.AutoPromote = true
	changed = changed.WithID()
	for _, test := range []struct {
		name            string
		loaded, request policy.Descriptor
	}{
		{"missing runner policy", policy.Descriptor{}, approved}, {"missing run policy", approved, policy.Descriptor{}}, {"untrusted workflow", changed, approved},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runner{workflow: config.Workflow{Config: config.Config{Policy: test.loaded}}}
			_, err := r.Run(t.Context(), RunRequest{Policy: test.request})
			if err == nil || !strings.Contains(err.Error(), "policy_mismatch") {
				t.Fatalf("Run() = %v", err)
			}
		})
	}
}

func TestResumeRequiresPinnedPolicy(t *testing.T) {
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
		{"same policy", policySessionStore{policy: approved}, approved, 1, true},
		{"changed policy", policySessionStore{policy: approved}, changed, 1, false},
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
