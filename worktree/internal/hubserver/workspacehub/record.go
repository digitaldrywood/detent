package workspacehub

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type Record struct {
	ID                 string
	OrganizationID     tracker.OrganizationID
	ProjectID          tracker.ProjectID
	SubjectWorkItemID  string
	AttemptID          string
	Ref                string
	HeadSHA            string
	RunnerID           string
	MachineID          tracker.MachineID
	MachineHostname    string
	WorktreePath       string
	LeaseID            tracker.LeaseID
	FencingToken       tracker.FencingToken
	State              string
	Reason             string
	Requires           []string
	Capabilities       *workspacesession.Capabilities
	Isolation          string
	Worktree           string
	ReadOnly           bool
	IdleTimeoutSeconds int
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

func (r Record) Owner() workspacesession.Owner {
	return workspacesession.Owner{
		WorkspaceID: r.ID, RunnerID: r.RunnerID, MachineID: string(r.MachineID),
		LeaseID: string(r.LeaseID), FencingToken: int64(r.FencingToken),
	}
}
func (r Record) Resource() workspacesession.Session {
	requires := r.Requires
	if requires == nil {
		requires = []string{}
	}
	return workspacesession.Session{
		ID: r.ID, OrganizationID: string(r.OrganizationID), ProjectID: string(r.ProjectID),
		WorkItemID: r.SubjectWorkItemID, AttemptID: r.AttemptID, Ref: r.Ref, HeadSHA: r.HeadSHA,
		RunnerID: r.RunnerID, MachineID: string(r.MachineID), MachineHostname: r.MachineHostname,
		WorktreePath: r.WorktreePath, State: r.State, Reason: r.Reason,
		Requires: requires, Capabilities: r.Capabilities, Isolation: r.Isolation, Worktree: r.Worktree,
		ReadOnly: r.ReadOnly, IdleTimeoutSeconds: r.IdleTimeoutSeconds, ExpiresAt: r.ExpiresAt,
		OpenedAt: r.OpenedAt, LastActivityAt: r.LastActivityAt, CreatedBy: r.CreatedBy,
		Revision: r.Revision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
func TenantRecord(row Row) Record {
	var capabilities *workspacesession.Capabilities
	if row.Capabilities != nil {
		capabilities = &workspacesession.Capabilities{}
		for _, name := range row.Capabilities {
			switch name {
			case "files":
				capabilities.Files = true
			case "diff":
				capabilities.Diff = true
			case "preview":
				capabilities.Preview = true
			case "terminal":
				capabilities.Terminal = true
			case "exec":
				capabilities.Exec = true
			case "git":
				capabilities.Git = true
			}
		}
	}
	return Record{ID: row.ID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, SubjectWorkItemID: row.SubjectWorkItemID, AttemptID: row.AttemptID, Ref: row.Ref, HeadSHA: row.HeadSHA, RunnerID: row.RunnerID, MachineID: tracker.MachineID(row.MachineID), MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, LeaseID: tracker.LeaseID(row.LeaseID), FencingToken: tracker.FencingToken(row.FencingToken), State: row.State, Reason: row.Reason, Requires: row.Requires, Capabilities: capabilities, Isolation: row.Isolation, Worktree: row.Worktree, ReadOnly: row.ReadOnly, IdleTimeoutSeconds: int(row.IdleTimeoutSeconds), ExpiresAt: row.ExpiresAt, RequestedExpiresAt: row.RequestedExpiresAt, OpenedAt: row.OpenedAt, LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, CreatedBy: row.CreatedBy, Revision: row.Revision, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
