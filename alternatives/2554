package project

import (
	"context"
	"errors"
	"log/slog"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func InitializeIssueLane(ctx context.Context, projectID string, cfg workflowconfig.Config, tracker connector.Connector, ledger store.LaneLedgerStore, metrics orchestrator.WorkflowMetricsRecorder, issue connector.Issue, target string) error {
	manager, _, _, err := buildScheduleOwnership(cfg, Dependencies{}, slog.Default(), nil)
	if err != nil {
		return err
	}
	err = orchestrator.InitializeIssueLane(ctx, orchestrator.Config{Project: scheduler.ProjectCandidate{ID: projectID}}, orchestrator.Dependencies{Connector: tracker, LaneLedger: ledger, LaneCoordination: manager.CoordinationStore(), WorkflowMetrics: metrics}, issue, target)
	return errors.Join(err, closeScheduleOwner(manager))
}
