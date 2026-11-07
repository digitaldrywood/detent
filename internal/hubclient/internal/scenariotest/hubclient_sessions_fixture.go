package scenariotest

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func (h *nativeChangeHub) completedSession(t *testing.T, ctx context.Context, execution runner.Execution, role string) int64 {
	t.Helper()
	sessions := api.NewRunnerSessions(h.native)
	ctx = sessions.BindExecution(ctx, execution)
	started := h.clock()
	identity := agentidentity.Configured("codex", "codex", "", role, "test", "openai", "", "", started)
	id, err := sessions.StartSession(ctx, store.SessionStart{ProjectID: "local", IssueID: string(execution.Recovery().Lease.WorkItemID), StartedAt: started, RequestedModel: "test", Model: "test", AgentBackendID: "codex", AgentBackendKind: "codex", AgentRole: role, RuntimeIdentity: identity})
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.FinishSession(ctx, id, store.SessionFinish{Turns: 1, CompletedAt: started.Add(time.Microsecond), FinalState: "completed", RuntimeIdentity: identity}); err != nil {
		t.Fatal(err)
	}
	return id
}
