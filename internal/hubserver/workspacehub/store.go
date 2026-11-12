package workspacehub

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Row struct {
	OrganizationID     tracker.OrganizationID
	ProjectID          tracker.ProjectID
	ID                 string
	SubjectWorkItemID  string
	AttemptID          string
	Ref                string
	HeadSHA            string
	RunnerID           string
	MachineID          string
	LeaseID            string
	FencingToken       int64
	State              string
	Reason             string
	Requires           []string
	Capabilities       []string
	Isolation          string
	Worktree           string
	MachineHostname    string
	WorktreePath       string
	ReadOnly           bool
	IdleTimeoutSeconds int32
	ExpiresAt          time.Time
	RequestedExpiresAt time.Time
	OpenedAt           *time.Time
	LastActivityAt     *time.Time
	LastHeartbeatAt    *time.Time
	RebindDeadline     *time.Time
	CreatedBy          string
	Revision           int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
type Update struct {
	State                string
	Reason               string
	RunnerID             string
	MachineID            string
	LeaseID              string
	FencingToken         int64
	HeadSHA              string
	Capabilities         []string
	Isolation            string
	Worktree             string
	MachineHostname      string
	WorktreePath         string
	ReadOnly             bool
	OpenedAt             *time.Time
	LastActivityAt       *time.Time
	LastHeartbeatAt      *time.Time
	RebindDeadline       *time.Time
	UpdatedAt            time.Time
	OrganizationID       tracker.OrganizationID
	ProjectID            tracker.ProjectID
	ID                   string
	ExpectedRevision     int64
	ExpectedRunnerID     string
	ExpectedLeaseID      string
	ExpectedFencingToken int64
}
type Reader interface {
	WorkspaceOpenSessions(context.Context, time.Time, string, int) ([]Row, error)
	WorkspaceSession(context.Context, tracker.ProjectID, string) (Row, error)
	WorkspaceDispatchItem(context.Context, tracker.ProjectID, string) (claimstore.WorkspaceItem, error)
}
type StateWriter interface {
	UpdateWorkspaceSession(context.Context, Update) (Row, error)
}
type Writer interface {
	Reader
	StateWriter
	CloseWorkspaceItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, time.Time) error
	EndWorkspaceOccupancy(context.Context, tracker.OrganizationID, tracker.ProjectID, string, string, time.Time) error
}
