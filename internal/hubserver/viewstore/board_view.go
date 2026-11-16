package viewstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	BoardViewMaxProjects = 500
	BoardViewMaxItems    = 500
	BoardViewMaxPage     = 200
)

type BoardClosedPosition struct {
	ClosedAt   *time.Time
	Identifier string
}

type BoardViewSelection struct {
	Projects     []tracker.ProjectID
	Allowed      []tracker.ProjectID
	Items        []tracker.NativeWorkItemID
	Reporter     string
	Archived     bool
	CredentialID string
	PrincipalID  string
	UserID       string
	ClosedPage   int
	ClosedLane   string
	ClosedAfter  *BoardClosedPosition
}

func (s BoardViewSelection) ProjectStrings() []string { return idStrings(s.Projects) }
func (s BoardViewSelection) AllowedStrings() []string { return idStrings(s.Allowed) }
func (s BoardViewSelection) ItemStrings() []string {
	values := make([]string, len(s.Items))
	for i, id := range s.Items {
		values[i] = string(id)
	}
	return values
}

func idStrings(ids []tracker.ProjectID) []string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	return values
}

type BoardViewCard struct {
	Number               *int64
	Revision             *int64
	State                *string
	Archived             *bool
	Terminal             *bool
	Running              *bool
	Dispatchable         *bool
	Priority             *int64
	ActivityAt           *time.Time
	Identifier           *string
	ClosedAt             *time.Time
	CreatedAt            *time.Time
	UpdatedAt            *time.Time
	LastActivityAt       *time.Time
	ActorPrincipalID     *string
	IssueType            *string
	Title                *string
	Labels               []string
	Assignees            []string
	HasLinks             *bool
	ProvenanceProvider   *string
	ProvenanceExternalID *string
	ProvenanceCreatedAt  *time.Time
	ProvenanceUpdatedAt  *time.Time
	ProvenanceObservedAt *time.Time
	PullRequestNumber    *int64
	PullRequestURL       *string
	ListChangeBranch     *string
	ListChangeHeadSHA    *string
	CardReady            *bool
	StatusLine           *string
	AttemptID            *string
	AttemptCount         *int64
	AttemptStatus        *string
	AttemptRunnerID      *string
	AttemptMachineID     *string
	AttemptStartedAt     *time.Time
	AttemptBackend       *string
	AttemptModel         *string
	AttemptEffort        *string
	ExecutionID          *string
	ExecutionRunning     *bool
	ExecutionStage       *string
	ExecutionRunnerID    *string
	ExecutionStartedAt   *time.Time
	ExecutionBackend     *string
	ExecutionModel       *string
	ExecutionEffort      *string
	ChangeID             *string
	ChangeTitle          *string
	ChangeStatus         *string
	ChangeProvider       *string
	ChangeExternalID     *string
	ChangeExternalURL    *string
	ChangePrincipalID    *string
	ChangeVersionID      *string
	ChangePolicyID       *string
	ChangeReviewPolicyID *string
	ChangeBaseStatus     *string
}

type BoardViewRow struct {
	Kind    string
	Project *string
	ID      *string
	Card    BoardViewCard
	Text    [8]*string
	Int     [8]*int64
	Bool    [8]*bool
	Time    [2]*time.Time
}

func (r *BoardViewRow) Targets() []any {
	c := &r.Card
	targets := []any{&r.Kind, &r.Project, &r.ID,
		&c.Number, &c.Revision, &c.State, &c.Archived, &c.Terminal, &c.Running, &c.Dispatchable, &c.Priority, &c.ActivityAt, &c.Identifier, &c.ClosedAt,
		&c.CreatedAt, &c.UpdatedAt, &c.LastActivityAt, &c.ActorPrincipalID, &c.IssueType, &c.Title, &c.Labels, &c.Assignees, &c.HasLinks, &c.ProvenanceProvider, &c.ProvenanceExternalID,
		&c.ProvenanceCreatedAt, &c.ProvenanceUpdatedAt, &c.ProvenanceObservedAt, &c.PullRequestNumber, &c.PullRequestURL, &c.ListChangeBranch, &c.ListChangeHeadSHA, &c.CardReady, &c.StatusLine,
		&c.AttemptID, &c.AttemptCount, &c.AttemptStatus, &c.AttemptRunnerID, &c.AttemptMachineID, &c.AttemptStartedAt, &c.AttemptBackend, &c.AttemptModel, &c.AttemptEffort, &c.ExecutionID,
		&c.ExecutionRunning, &c.ExecutionStage, &c.ExecutionRunnerID, &c.ExecutionStartedAt, &c.ExecutionBackend, &c.ExecutionModel, &c.ExecutionEffort, &c.ChangeID, &c.ChangeTitle,
		&c.ChangeStatus, &c.ChangeProvider, &c.ChangeExternalID, &c.ChangeExternalURL, &c.ChangePrincipalID, &c.ChangeVersionID, &c.ChangePolicyID, &c.ChangeReviewPolicyID, &c.ChangeBaseStatus}
	for i := range r.Text {
		targets = append(targets, &r.Text[i])
	}
	for i := range r.Int {
		targets = append(targets, &r.Int[i])
	}
	for i := range r.Bool {
		targets = append(targets, &r.Bool[i])
	}
	for i := range r.Time {
		targets = append(targets, &r.Time[i])
	}
	return targets
}

type BoardViewRows struct {
	Rows []BoardViewRow
}
