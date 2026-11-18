package healthstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledReport struct {
	gate.ScheduledValidationReport
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	RecordedAt     time.Time
}

type ScheduledHistory struct {
	Reports     []ScheduledReport
	FirstReport time.Time
	LastGreen   time.Time
}

type ScheduledReader interface {
	HealthScheduledHistory(context.Context, tracker.ProjectID, time.Time) (ScheduledHistory, error)
}

type ScheduledReportReader interface {
	HealthScheduledReport(context.Context, tracker.ProjectID, string, string, string) (ScheduledReport, error)
}

type ScheduledWriter interface {
	ScheduledReader
	ScheduledReportReader
	RecordHealthScheduledReport(context.Context, ScheduledReport) (ScheduledReport, error)
}
