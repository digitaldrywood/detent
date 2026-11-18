package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSourceRework(t *testing.T) {
	t.Parallel()
	complete := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name    string
		change  func(*NativeChange)
		message string
		want    bool
	}{
		{name: "validator rework", want: true},
		{name: "implementation rework", want: true, change: func(c *NativeChange) { c.ImplementationReview, c.Validator = c.Validator, nil }},
		{name: "audit rework", want: true, change: func(c *NativeChange) {
			c.Validator = nil
			c.SecurityAudit = &tracker.SecurityAuditReview{VersionID: "v2", Verdict: "rework", Run: securityaudit.Run{HeadSHA: "head2"}}
		}},
		{name: "wait", change: func(c *NativeChange) { c.Validator.Verdict = "wait" }},
		{name: "infrastructure", change: func(c *NativeChange) { c.Error = "provider unavailable" }},
		{name: "review infrastructure", change: func(c *NativeChange) { c.ImplementationReviewError = "provider unavailable" }},
		{name: "source custody", change: func(c *NativeChange) { c.VersionError = "source unavailable" }},
		{name: "stale policy", change: func(c *NativeChange) { c.VersionError, c.VersionCode = "stale policy", "policy_mismatch" }},
		{name: "landing failure", change: func(c *NativeChange) { c.Landing = &NativeLanding{GateFailed: true} }},
		{name: "blocked", message: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Supply access\n```"},
		{name: "human action", message: strings.Replace(complete, "human_action: null", "human_action: Supply access", 1)},
		{name: "unfinished", message: strings.Replace(complete, "status: complete", "status: in_progress", 1)},
		{name: "older head", change: func(c *NativeChange) { c.Validator.HeadSHA = "head1" }},
		{name: "older version", change: func(c *NativeChange) { c.Validator.VersionID = "v1" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			change := &NativeChange{Changed: true, VersionID: "v2", HeadSHA: "head2", Validator: &gate.ValidatorResult{VersionID: "v2", HeadSHA: "head2", Verdict: "rework", Summary: "Repair the defect"}}
			if test.change != nil {
				test.change(change)
			}
			message := complete
			if test.message != "" {
				message = test.message
			}
			if got := nativeSourceRework(change, message); (got != "") != test.want {
				t.Fatalf("feedback=%q want repair=%v", got, test.want)
			}
		})
	}
}

func TestSourceTurnBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		updates       []AgentUpdate
		wantRemaining int
	}{
		{name: "provider without turn notifications", wantRemaining: 2},
		{name: "two distinct turns", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "two"}}, wantRemaining: 1},
		{name: "repeated notification counts once", updates: []AgentUpdate{{Type: AgentUpdateTurnStarted, TurnID: "one"}, {Type: AgentUpdateTurnStarted, TurnID: "one"}}, wantRemaining: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := &sourceTurnBudget{remaining: 3}
			ctx := context.WithValue(t.Context(), sourceTurnBudgetKey{}, budget)
			backend := &fakeCodexClient{updates: test.updates}
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
				t.Fatal(errors.Join(err, cleanupErr))
			}
			if backend.request.MaxTurns != 3 || budget.remaining != test.wantRemaining {
				t.Fatalf("per-call budget reset: max=%d remaining=%d", backend.request.MaxTurns, budget.remaining)
			}
			backend.updates = nil
			for budget.remaining > 0 {
				remaining := budget.remaining
				if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{MaxTurns: 20}, nil, nil, nil); err != nil || cleanupErr != nil {
					t.Fatal(errors.Join(err, cleanupErr))
				}
				if backend.request.MaxTurns != remaining {
					t.Fatalf("remaining budget=%d provider limit=%d", remaining, backend.request.MaxTurns)
				}
			}
			calls := backend.calls
			if _, err, cleanupErr := runAgentBackendTurnWithTools(ctx, backend, AgentTurnRequest{}, nil, nil, nil); !errors.Is(err, ErrSessionTurnLimitExceeded) || cleanupErr != nil || backend.calls != calls {
				t.Fatalf("exhausted budget started another provider call: %v calls=%d", err, backend.calls)
			}
		})
	}
}
