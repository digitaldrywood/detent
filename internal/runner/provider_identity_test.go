package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestProviderIdentityFailureDoesNotCancelTurn(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"implementation", "validator", "triage"} {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"cancelled", context.Canceled},
			{"timed out", context.DeadlineExceeded},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ss := &failingProviderIdentityStore{fakeSessionStore: &fakeSessionStore{sessionID: 2626}, err: tc.err}
				backend := &fakeCodexClient{
					updates: []AgentUpdate{
						{Type: AgentUpdateTurnStarted, ThreadID: "thread", TurnID: "turn"},
						{Type: AgentUpdateMessageDelta, ThreadID: "thread", Delta: `{"verdict":"pass","score":1,"summary":"continued"}`},
						{Type: AgentUpdateTurnCompleted, ThreadID: "thread", TurnID: "turn"},
					},
					result: AgentTurnResult{ThreadID: "thread", TurnID: "turn"},
				}
				info := workspace.Info{Path: t.TempDir(), Key: "provider-identity"}
				var logs bytes.Buffer
				r, err := NewRunner(Dependencies{
					SecurityAuditRoot: t.TempDir(),
					Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
					Workflow:          config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Work"},
					Workspace:         &fakeWorkspaceBackend{info: info}, AgentBackend: backend, Store: ss,
				})
				if err != nil {
					t.Fatal(err)
				}
				issue := connector.Issue{ID: "2626", Identifier: "digitaldrywood/detent#2626"}
				switch role {
				case "triage":
					result, err := r.Run(t.Context(), RunRequest{Issue: issue, Mode: RunModeTriage})
					if err != nil {
						t.Fatalf("Run triage: %v", err)
					}
					if result.FinalState != FinalStateCompleted || !strings.Contains(result.Output, "continued") {
						t.Fatalf("result = %+v", result)
					}
				case "validator":
					result, err := r.Validate(t.Context(), ValidatorRequest{Issue: issue})
					if err != nil {
						t.Fatalf("Validate: %v", err)
					}
					if result.Summary != "continued" || result.Verdict != gate.ValidatorVerdictPass {
						t.Fatalf("result = %+v", result)
					}
				default:
					execution := r.runAgentTurn(t.Context(), backend, AgentTurnRequest{Workspace: info.Path}, RunRequest{Issue: issue}, info, workspace.Issue{ID: issue.ID, Identifier: issue.Identifier}, config.Agent{}, "", nil, time.Now(), 2626, agentidentity.Identity{}, nil, 0, "", "")
					if execution.err != nil {
						t.Fatalf("runAgentTurn: %v", execution.err)
					}
					if execution.cleanupErr != nil {
						t.Fatal(execution.cleanupErr)
					}
				}
				if !strings.Contains(logs.String(), "agent session provider identity persistence deferred") {
					t.Fatalf("missing persistence warning: %s", logs.String())
				}
				if ss.calls != 3 || len(ss.providerUpdates) != 2 {
					t.Fatalf("writes = %d, successful writes = %d; want 3 and 2", ss.calls, len(ss.providerUpdates))
				}
				if ss.providerUpdates[1].ThreadID != "thread" {
					t.Fatalf("persisted identity = %+v", ss.providerUpdates[1])
				}
				if !ss.bounded {
					t.Fatal("provider persistence context has no deadline")
				}
			})
		}
	}
}

type failingProviderIdentityStore struct {
	*fakeSessionStore
	err        error
	calls      int
	bounded    bool
	contextErr error
	writeDone  <-chan struct{}
}

func (s *failingProviderIdentityStore) UpdateSessionProviderIdentity(ctx context.Context, id int64, identity store.SessionProviderIdentity) error {
	s.calls++
	s.contextErr = ctx.Err()
	s.writeDone = ctx.Done()
	_, s.bounded = ctx.Deadline()
	if s.calls == 2 {
		return fmt.Errorf("updating codex session provider identity: %w", s.err)
	}
	return s.fakeSessionStore.UpdateSessionProviderIdentity(ctx, id, identity)
}

func TestProviderIdentityPersistenceDetachesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ss := &failingProviderIdentityStore{fakeSessionStore: &fakeSessionStore{}}
	r := &Runner{store: ss}
	r.persistSessionProviderIdentity(ctx, 2626, AgentUpdate{ThreadID: "thread"})
	if !ss.bounded || len(ss.providerUpdates) != 1 {
		t.Fatal("identity was not persisted with a bounded context")
	}
	if ss.contextErr != nil {
		t.Fatalf("store inherited cancellation: %v", ss.contextErr)
	}
	select {
	case <-ss.writeDone:
	default:
		t.Fatal("write context was not released")
	}
}
