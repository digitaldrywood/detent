package sshexec

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type callerFunc func(context.Context, string, any, ...any) error

func (call callerFunc) Call(ctx context.Context, method string, result any, args ...any) error {
	return call(ctx, method, result, args...)
}

func TestAdaptersPreserveWireContract(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		method   string
		args     []any
		response any
		call     func(context.Context, Caller) (any, error)
	}{
		{
			name: "session start", method: "store.StartSession",
			args: []any{store.SessionStart{ProjectID: "project", IssueID: "issue"}}, response: int64(41),
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (SessionStore{Peer: peer}).StartSession(ctx, store.SessionStart{ProjectID: "project", IssueID: "issue"})
			},
		},
		{
			name: "session finish", method: "store.FinishSession",
			args: []any{int64(41), store.SessionFinish{TotalTokens: 73}},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return nil, (SessionStore{Peer: peer}).FinishSession(ctx, 41, store.SessionFinish{TotalTokens: 73})
			},
		},
		{
			name: "activity profile", method: "store.SaveWorkflowActivityProfile",
			args: []any{int64(41), store.WorkflowPhaseEvent{}, workflowmetrics.ActivityProfile{SessionID: 41, AttemptID: 17}}, response: int64(43),
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (SessionStore{Peer: peer}).SaveWorkflowActivityProfile(ctx, 41, store.WorkflowPhaseEvent{}, workflowmetrics.ActivityProfile{SessionID: 41, AttemptID: 17})
			},
		},
		{
			name: "session policy", method: "store.SessionPolicy",
			args: []any{"project", int64(41)}, response: policy.Descriptor{ID: "policy", SourceRevision: "revision"},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (SessionStore{Peer: peer}).SessionPolicy(ctx, "project", 41)
			},
		},
		{
			name: "resume state", method: "store.LatestAgentResumeState",
			args:     []any{store.AgentResumeLookup{ProjectID: "project", IssueID: "issue", NativeAttemptID: "attempt"}},
			response: store.AgentResumeState{DetentSessionID: 41, ProviderThreadID: "thread", ProviderSessionID: "session"},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (SessionStore{Peer: peer}).LatestAgentResumeState(ctx, store.AgentResumeLookup{ProjectID: "project", IssueID: "issue", NativeAttemptID: "attempt"})
			},
		},
		{
			name: "budget check", method: "budget.check",
			args:     []any{budget.DispatchRequest{IssueID: "issue", Model: "model", Estimate: budget.TokenEstimate{TotalTokens: 73}}},
			response: budget.Decision{Allowed: true, Projection: &budget.Projection{CostUSD: 1.25}},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (BudgetGuard{Peer: peer}).CheckDispatch(ctx, budget.DispatchRequest{IssueID: "issue", Model: "model", Estimate: budget.TokenEstimate{TotalTokens: 73}})
			},
		},
		{
			name: "budget estimate", method: "budget.estimate",
			args: []any{"project"}, response: budget.TokenEstimate{TotalTokens: 73, Sessions: 2},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (BudgetGuard{Peer: peer}).EstimateDispatch(ctx, "project")
			},
		},
		{
			name: "receipt attestation", method: "go_test_receipts.attest",
			response: gotestreceipt.Attestation{ID: "attestation", DigestKey: []byte("key")},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (GoTestReceipts{Peer: peer}).Attest(ctx)
			},
		},
		{
			name: "receipt lookup", method: "go_test_receipts.lookup",
			args: []any{"key"}, response: []gotestreceipt.Candidate{{Inputs: []string{"env\tHOME"}, Digest: "digest"}},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return (GoTestReceipts{Peer: peer}).Lookup(ctx, "key")
			},
		},
		{
			name: "receipt record", method: "go_test_receipts.record",
			args: []any{gotestreceipt.Receipt{Key: "key", Package: "internal/example", Inputs: []string{"env\tHOME"}, Digest: "digest"}},
			call: func(ctx context.Context, peer Caller) (any, error) {
				return nil, (GoTestReceipts{Peer: peer}).Record(ctx, gotestreceipt.Receipt{Key: "key", Package: "internal/example", Inputs: []string{"env\tHOME"}, Digest: "digest"})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, callErr := range []error{nil, errors.New("channel disconnected")} {
				called := false
				ctx := t.Context()
				peer := callerFunc(func(gotCtx context.Context, method string, result any, args ...any) error {
					called = true
					if gotCtx != ctx || method != test.method || !reflect.DeepEqual(args, test.args) {
						t.Fatalf("wire call = %q, %+v; want %q, %+v with original context", method, args, test.method, test.args)
					}
					if callErr != nil {
						return callErr
					}
					if test.response == nil {
						if result != nil {
							t.Fatalf("write expects no response, got %T", result)
						}
						return nil
					}
					data, err := json.Marshal(test.response)
					if err != nil {
						t.Fatal(err)
					}
					return json.Unmarshal(data, result)
				})
				got, err := test.call(ctx, peer)
				if !called || !errors.Is(err, callErr) {
					t.Fatalf("called = %t, error = %v; want %v", called, err, callErr)
				}
				if callErr == nil && !reflect.DeepEqual(got, test.response) {
					t.Fatalf("response = %+v; want %+v", got, test.response)
				}
			}
		})
	}
}
