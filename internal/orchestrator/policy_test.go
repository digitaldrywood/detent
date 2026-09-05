package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
)

func orchestratorTestPolicy() policy.Descriptor {
	return policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), AutomatedReview: "off", MergeMethod: "squash"}}.WithID()
}

type checkedPolicyScheduling struct {
	SchedulingSource
	approved policy.Descriptor
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
	o := &Orchestrator{cfg: Config{Policy: descriptor}}
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"active", runningWorkAttemptMetadataJSON(Running{Policy: descriptor}, nil), true},
		{"forged completion metadata", runningWorkAttemptMetadataJSON(Running{Policy: descriptor}, map[string]any{"policy": policy.Descriptor{}}), true},
		{"missing", "{}", false}, {"invalid", "{", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := o.checkAttemptPolicy(store.WorkAttempt{WorkerMetadataJSON: test.raw})
			if (err == nil) != test.valid {
				t.Fatalf("attempt policy = %v, valid=%t", err, test.valid)
			}
		})
	}
	raw, err := json.Marshal(map[string]policy.Descriptor{"policy": descriptor})
	if err != nil {
		t.Fatal(err)
	}
	o.cfg.Policy.Gates.AutoPromote = true
	o.cfg.Policy = o.cfg.Policy.WithID()
	if o.checkAttemptPolicy(store.WorkAttempt{WorkerMetadataJSON: string(raw)}) == nil {
		t.Fatal("resumed attempt under relaxed policy")
	}
}
