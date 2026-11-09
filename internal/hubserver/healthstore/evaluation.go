package healthstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type EvaluationProgress struct {
	Phase          string
	Position       int64
	FindingCursor  string
	BaselineCursor Baseline
}

type Evaluation struct {
	OrganizationID tracker.OrganizationID
	ID             string
	At             time.Time
	Input          attachment.ArchiveReference
	FindingCount   int64
	BaselineCount  int64
	Revision       int64
	EvaluationProgress
}

type EvaluationReader interface {
	HealthEvaluationHasProjectLink(context.Context, string, []tracker.ProjectID) (bool, error)
	HealthEvaluation(context.Context) (Evaluation, error)
	HealthEvaluationBaselines(context.Context, Baseline, int) ([]Baseline, error)
	HealthEvaluationProjects(context.Context, []tracker.ProjectID) ([]tracker.ProjectID, error)
}

type EvaluationWriter interface {
	EvaluationReader
	StartHealthEvaluation(context.Context, Evaluation) (Evaluation, error)
	LockHealthEvaluation(context.Context, Evaluation) (Evaluation, error)
	AdvanceHealthEvaluation(context.Context, Evaluation, EvaluationProgress) (Evaluation, error)
	FinishHealthEvaluation(context.Context, Evaluation) error
}
