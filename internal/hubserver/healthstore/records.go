package healthstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Finding struct {
	OrganizationID                 tracker.OrganizationID
	Sequence                       int64
	Generation                     int64
	SourceAt                       time.Time
	DisplayEvidence                Evidence
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
	ChangeSequence int64
	Findings       []Finding
	Baselines      []Baseline
	LastTickAt     *time.Time
}

type Reader interface {
	HealthFindings(context.Context, tracker.ProjectID, string, *time.Time, int64, string, int) (Page, error)
	HealthSummaryFindings(context.Context, int) ([]Finding, error)
}

type PageMaintainer interface {
	NextHealthPage(context.Context) (Finding, error)
	PublishHealthPage(context.Context, Finding, Evidence) (bool, error)
	AdvanceHealthPage(context.Context, Finding) error
}

var ErrPagePending = errors.New("health page projection pending")
