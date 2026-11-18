package scenariotest

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
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
	var snapshot runnersession.Snapshot
	if err := h.native.Transport().Request(ctx, http.MethodGet, h.native.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != id {
		t.Fatalf("session snapshot identity changed: got=%d want=%d", snapshot.ID, id)
	}
	completed := snapshot.Start.StartedAt.Add(time.Microsecond)
	finalization := &sessionmodel.Finalization{
		SessionID: id,
		Finish:    store.SessionFinish{Turns: 1, CompletedAt: completed, FinalState: "completed", RuntimeIdentity: identity},
		Usage:     store.UsageEvent{ProjectID: snapshot.Start.ProjectID, IssueID: snapshot.Start.IssueID, SessionID: id, StartedAt: snapshot.Start.StartedAt, FinishedAt: completed, Outcome: "completed", Model: snapshot.Start.Model},
		Phase:     &store.WorkflowPhaseEvent{ProjectID: snapshot.Start.ProjectID, IssueID: snapshot.Start.IssueID, SessionID: id, StartedAt: snapshot.Start.StartedAt, FinishedAt: completed, PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed", Turns: 1},
	}
	if err := sessions.ReconcileSessionFinalization(ctx, id, finalization); err != nil {
		t.Fatal(err)
	}
	return id
}
