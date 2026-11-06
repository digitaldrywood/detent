package healthstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Finding struct {
	Sequence                       int64
	ID, Fingerprint, Signal, Class string
	SubjectKind, SubjectID         string
	OpenedAt, LastSeenAt           time.Time
	ResolvedAt                     *time.Time
	Severity, Summary, NextAction  string
	Evidence                       attachment.ArchiveReference
	EmailUnavailable               bool
	SlackUnavailableAt             *time.Time
	SlackStatusCode                int
	Projects                       []tracker.ProjectID
	Issues                         []Issue
}

type Issue struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
}

type Baseline struct {
	ProjectID    tracker.ProjectID
	Signal, Lane string
}

type Page struct {
	Findings   []Finding
	Baselines  []Baseline
	LastTickAt *time.Time
}

type Reader interface {
	HealthFindings(context.Context, tracker.ProjectID, string, *time.Time, int64, string, int) (Page, error)
	HealthSummaryFindings(context.Context, int) ([]Finding, error)
}
