package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	releasepkg "github.com/digitaldrywood/detent/internal/release"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestEvaluateReleaseRecordsSchedulerDecision(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	orch := &Orchestrator{
		cfg:     Config{Project: scheduler.ProjectCandidate{ID: "detent"}},
		release: fixedReleaseCoordinator{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(normalizeConfig(orch.cfg))
	orch.evaluateRelease(context.Background(), &state, now)

	if state.Release.State != "release_pending" || state.Release.PendingTag != "v1.3.0" {
		t.Fatalf("release status = %#v", state.Release)
	}
	if len(state.SchedulerDecisions) != 1 {
		t.Fatalf("scheduler decisions = %#v", state.SchedulerDecisions)
	}
	decision := state.SchedulerDecisions[0]
	if decision.ProjectID != "detent" || decision.Lane != "release" || !decision.Selected || decision.WaitReason != "tag_created" {
		t.Fatalf("scheduler decision = %#v", decision)
	}
}

type fixedReleaseCoordinator struct{}

func (fixedReleaseCoordinator) Evaluate(context.Context, time.Time) (releasepkg.Status, releasepkg.Decision) {
	return releasepkg.Status{Enabled: true, State: "release_pending", PendingTag: "v1.3.0"}, releasepkg.Decision{Action: "tag_created", Reason: "created v1.3.0", Selected: true}
}
