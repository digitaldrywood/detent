package releaseaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

type recoveryProvider struct {
	calls int
	mode  string
}

func (p *recoveryProvider) Invoke(_ context.Context, invocation Invocation, observe func(Usage) error) (Response, error) {
	p.calls++
	response := Response{SessionID: invocation.ID, Output: json.RawMessage(`{"complete":true}`)}
	if p.mode == "reused failed session" {
		response.SessionID = "same-session"
	}
	if p.mode != "missing usage" {
		if err := observe(Usage{Input: 3, Output: 2, Cached: 1}); err != nil {
			return response, err
		}
	}
	switch p.mode {
	case "interrupted":
		if p.calls == 1 {
			return response, errors.New("provider interrupted")
		}
	case "truncated":
		if p.calls == 1 {
			response.Output = json.RawMessage(`{"complete":`)
		}
	case "always":
		return response, errors.New("provider unavailable")
	case "reused failed session":
		if p.calls == 1 {
			return response, errors.New("provider interrupted")
		}
	case "cancelled":
		return response, context.Canceled
	case "deadline":
		return response, context.DeadlineExceeded
	}
	return response, nil
}

func TestProviderRecoveryRetainsEvidenceAndBudget(t *testing.T) {
	t.Parallel()
	source := pinnedFixture(t)
	for _, tc := range []struct {
		name, mode                            string
		limit, calls                          int
		wantError, reused, publicationFailure bool
	}{
		{name: "interrupted", mode: "interrupted", calls: 2},
		{name: "truncated", mode: "truncated", calls: 2},
		{name: "permanent", mode: "always", calls: 3, wantError: true},
		{name: "invocation bound", mode: "always", limit: 1, calls: 1, wantError: true},
		{name: "missing usage", mode: "missing usage", calls: 1, wantError: true},
		{name: "reused session", reused: true, calls: 1, wantError: true},
		{name: "cancelled", mode: "cancelled", calls: 1, wantError: true},
		{name: "deadline", mode: "deadline", calls: 1, wantError: true},
		{name: "reused failed session", mode: "reused failed session", calls: 2, wantError: true},
		{name: "usage bound", mode: "interrupted", calls: 1, wantError: true},
		{name: "publication failure", mode: "interrupted", publicationFailure: true, calls: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &recoveryProvider{mode: tc.mode}
			records := &memoryRecords{}
			if tc.publicationFailure {
				records.failStatus = "incomplete"
			}
			limit := tc.limit
			if limit == 0 {
				limit = 10
			}
			e := &execution{stage: &Stage{provider: provider, records: records}, request: Request{Source: source, Identity: Identity{Commit: source.Commit()}, Allowance: Allowance{Invocations: limit, Usage: Usage{Input: 100, Output: 100, Cached: 100}}}, directory: t.TempDir(), sessions: map[string]bool{}, state: Snapshot{Status: "incomplete", Ledger: json.RawMessage(`[]`)}}
			if tc.reused {
				e.sessions["agent-0001"] = true
			}
			if tc.name == "usage bound" {
				e.request.Allowance.Usage.Input = 3
			}
			response, err := e.invoke(t.Context(), "recon-surfaces", nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if provider.calls != tc.calls || len(e.state.Evidence) != tc.calls {
				t.Fatalf("calls=%d evidence=%d want=%d", provider.calls, len(e.state.Evidence), tc.calls)
			}
			if !tc.wantError && string(response.Output) != `{"complete":true}` {
				t.Fatalf("incomplete response accepted: %s", response.Output)
			}
			if tc.mode != "missing usage" && e.state.Usage.Input != int64(tc.calls*3) {
				t.Fatalf("lost retry usage: %+v", e.state.Usage)
			}
			for i, evidence := range e.state.Evidence {
				if evidence.InvocationID != fmt.Sprintf("agent-%04d", i+1) {
					t.Fatal("retry reused invocation")
				}
			}
			if (tc.limit == 1 || tc.name == "usage bound") && !errors.Is(err, ErrAllowance) {
				t.Fatalf("lost allowance error: %v", err)
			}
		})
	}
}
