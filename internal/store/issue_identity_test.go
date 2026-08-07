package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIssueIdentityReadsAreProjectScoped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	for index, projectID := range []string{"project-a", "project-b"} {
		at := base.Add(time.Duration(index) * time.Minute)
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
			ProjectID:    projectID,
			IssueID:      "same-id",
			Identifier:   "owner/repo#1",
			PhaseType:    WorkflowPhaseTypeLane,
			PhaseName:    projectID,
			Status:       "entered",
			StartedAt:    at,
			MetadataJSON: `{"provenance":{"origin":"unknown"}}`,
		}); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent(%s) error = %v", projectID, err)
		}
		if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
			ProjectID:  projectID,
			IssueID:    "same-id",
			Result:     SchedulerDecisionResultSkipped,
			Reason:     projectID,
			DecisionAt: at,
		}); err != nil {
			t.Fatalf("RecordSchedulerDecision(%s) error = %v", projectID, err)
		}
		sessionID, err := backend.StartSession(ctx, SessionStart{
			ProjectID:        projectID,
			IssueID:          "same-id",
			Identifier:       "owner/repo#1",
			StartedAt:        at,
			AgentBackendKind: "codex",
		})
		if err != nil {
			t.Fatalf("StartSession(%s) error = %v", projectID, err)
		}
		if err := backend.FinishSession(ctx, sessionID, SessionFinish{
			CompletedAt:       at.Add(30 * time.Second),
			FinalState:        "completed",
			ProviderSessionID: projectID + "-provider",
			InputTokens:       int64((index + 1) * 100),
			TotalTokens:       int64((index + 1) * 100),
		}); err != nil {
			t.Fatalf("FinishSession(%s) error = %v", projectID, err)
		}
	}

	identity := IssueIdentity{ProjectID: "project-a", IssueID: "same-id"}
	timeline, err := backend.IssueWorkflowTimeline(ctx, identity)
	if err != nil || len(timeline.Events) != 1 || timeline.Events[0].ProjectID != "project-a" {
		t.Fatalf("IssueWorkflowTimeline() = %#v, %v", timeline, err)
	}
	decisions, err := backend.(IssueSchedulerDecisionStore).ListIssueSchedulerDecisions(ctx, IssueSchedulerDecisionQuery{Identity: identity})
	if err != nil || len(decisions) != 1 || decisions[0].ProjectID != "project-a" {
		t.Fatalf("ListIssueSchedulerDecisions() = %#v, %v", decisions, err)
	}
	session, err := backend.(ActivityStore).LatestIssueAgentSession(ctx, identity)
	if err != nil || session.ProjectID != "project-a" || session.ProviderSessionID != "project-a-provider" {
		t.Fatalf("LatestIssueAgentSession() = %#v, %v", session, err)
	}
	spend, err := backend.IssueTokenSpend(ctx, identity)
	if err != nil || spend.TotalTokens != 100 || spend.Sessions != 1 {
		t.Fatalf("IssueTokenSpend() = %#v, %v", spend, err)
	}
}

func TestIssueIdentityReadsRequireProject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	tests := []struct {
		name string
		read func() error
	}{
		{name: "workflow", read: func() error {
			_, err := backend.IssueWorkflowTimeline(ctx, IssueIdentity{IssueID: "issue-1"})
			return err
		}},
		{name: "scheduler", read: func() error {
			_, err := backend.(IssueSchedulerDecisionStore).ListIssueSchedulerDecisions(ctx, IssueSchedulerDecisionQuery{Identity: IssueIdentity{IssueID: "issue-1"}})
			return err
		}},
		{name: "session", read: func() error {
			_, err := backend.(ActivityStore).LatestIssueAgentSession(ctx, IssueIdentity{IssueID: "issue-1"})
			return err
		}},
		{name: "resume", read: func() error {
			_, err := backend.LatestIssueAgentResumeState(ctx, IssueIdentity{IssueID: "issue-1"})
			return err
		}},
		{name: "spend", read: func() error { _, err := backend.IssueTokenSpend(ctx, IssueIdentity{IssueID: "issue-1"}); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.read(); !errors.Is(err, ErrProjectRequired) {
				t.Fatalf("read error = %v, want ErrProjectRequired", err)
			}
		})
	}
}
