package workspacehub

import (
	"context"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type Deadline struct {
	State   string
	Reason  string
	EndedAt time.Time
	Apply   bool
}

func DecideDeadline(record Record, now time.Time) Deadline {
	switch record.State {
	case workspacesession.StateRequested:
		if record.RebindDeadline != nil {
			if now.Before(*record.RebindDeadline) {
				return Deadline{}
			}
			return Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonHubRestarted, EndedAt: now, Apply: true}
		}
		if !now.Before(record.RequestedExpiresAt) {
			return Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonNoRunner, EndedAt: now, Apply: true}
		}
	case workspacesession.StateStarting, workspacesession.StateReady, workspacesession.StateIdle:
		if record.LastHeartbeatAt != nil {
			expiry := record.LastHeartbeatAt.Add(workspacesession.LeaseTTL)
			if !now.Before(expiry) {
				return Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonLeaseLost, EndedAt: expiry, Apply: true}
			}
			missed := record.LastHeartbeatAt.Add(workspacesession.MissedHeartbeats * workspacesession.HeartbeatInterval)
			if !now.Before(missed) && record.State != workspacesession.StateStarting {
				return Deadline{State: workspacesession.StateUnreachable, Reason: workspacesession.ReasonRunnerRestarted, EndedAt: now, Apply: true}
			}
		}
		if !now.Before(record.ExpiresAt) {
			return Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true}
		}
		if idle := IdleSince(record); !idle.IsZero() {
			if !now.Before(idle.Add(time.Duration(record.IdleTimeoutSeconds) * time.Second)) {
				return Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true}
			}
			if record.State == workspacesession.StateReady && !now.Before(idle.Add(IdleMark)) {
				return Deadline{State: workspacesession.StateIdle, EndedAt: now, Apply: true}
			}
		}
	case workspacesession.StateUnreachable:
		if record.LastHeartbeatAt != nil {
			expiry := record.LastHeartbeatAt.Add(workspacesession.LeaseTTL)
			if !now.Before(expiry) {
				return Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonLeaseLost, EndedAt: expiry, Apply: true}
			}
		}
		if !now.Before(record.UpdatedAt.Add(time.Duration(record.IdleTimeoutSeconds) * time.Second)) {
			return Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true}
		}
	case workspacesession.StateClosing:
		if !now.Before(record.UpdatedAt.Add(workspacesession.LeaseTTL)) {
			return Deadline{State: workspacesession.StateClosed, Reason: record.Reason, EndedAt: now, Apply: true}
		}
	}
	if !now.Before(record.ExpiresAt) {
		return Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true}
	}
	return Deadline{}
}

const IdleMark = 2 * time.Minute

func IdleSince(record Record) time.Time {
	if record.LastActivityAt != nil {
		return *record.LastActivityAt
	}
	if record.OpenedAt != nil {
		return *record.OpenedAt
	}
	return record.CreatedAt
}
func SameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
func TenantUpdate(row Row, state, reason string, now time.Time) Update {
	return Update{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, Reason: reason, RunnerID: row.RunnerID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, HeadSHA: row.HeadSHA, Capabilities: row.Capabilities, Isolation: row.Isolation, Worktree: row.Worktree, MachineHostname: row.MachineHostname, WorktreePath: row.WorktreePath, ReadOnly: row.ReadOnly, OpenedAt: row.OpenedAt, LastActivityAt: row.LastActivityAt, LastHeartbeatAt: row.LastHeartbeatAt, RebindDeadline: row.RebindDeadline, UpdatedAt: now}
}
func UpdateState(ctx context.Context, owner StateWriter, row, expected Row, target, reason string, now time.Time) (Row, error) {
	states := []string{target}
	if row.State != target && !workspacesession.Transition(row.State, target) {
		if !workspacesession.Transition(row.State, workspacesession.StateClosing) {
			return row, &hubapi.Error{Code: "stale_execution", Message: "The workspace cannot enter the requested state", Status: http.StatusConflict}
		}
		states = []string{workspacesession.StateClosing, target}
	}
	for _, state := range states {
		if row.State != state && !workspacesession.Transition(row.State, state) {
			return row, &hubapi.Error{Code: "stale_execution", Message: "The workspace cannot enter the requested state", Status: http.StatusConflict}
		}
		arg := TenantUpdate(row, state, reason, now)
		arg.ExpectedRunnerID, arg.ExpectedLeaseID, arg.ExpectedFencingToken = expected.RunnerID, expected.LeaseID, expected.FencingToken
		var err error
		row, err = owner.UpdateWorkspaceSession(ctx, arg)
		if err != nil {
			return row, err
		}
		expected = row
	}
	return row, nil
}
