package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerClaimState(ctx context.Context, runner string, project tracker.ProjectID, after string, limit int, now time.Time) (runnerstore.ClaimStatePage, error) {
	var page runnerstore.ClaimStatePage
	if limit < 1 || limit > 100 || now.IsZero() {
		return page, corestore.ErrInvalidPage
	}
	identity, err := s.RunnerIdentity(ctx, runner)
	if err != nil {
		return page, err
	}
	machine, err := s.Machine(ctx, string(identity.MachineID))
	if err != nil {
		return page, err
	}
	page.State.HostCapacity = int(machine.Capacity)
	page.State.RunnerCapacity = int(min(identity.CapacityLimit, identity.ReportedCapacity))
	slots, err := s.queries.RunnerClaimSlots(ctx, pgquery.RunnerClaimSlotsParams{OrganizationID: s.organization, MachineID: machine.ID, Now: now})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(slots) > 10000 {
		return page, corestore.ErrInvalidPage
	}
	for _, slot := range slots {
		page.State.Slots = append(page.State.Slots, runnerauth.ClaimSlot{ID: tracker.LeaseID(slot.LeaseID), RunnerID: slot.RunnerID})
	}
	policy, err := s.ProjectPolicy(ctx, project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return page, err
	}
	page.State.PolicyIDs = map[tracker.ProjectID]string{project: policy.PolicyID}
	rows, err := s.queries.RunnerReleasedAttemptCandidates(ctx, pgquery.RunnerReleasedAttemptCandidatesParams{OrganizationID: s.organization, ProjectID: project, RunnerID: runner, MachineID: machine.ID, AfterID: after, PageSize: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	for _, row := range rows {
		if row.WorkItemID != "" {
			page.Candidates = append(page.Candidates, runnerauth.ReleasedExecution{ProjectID: project, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), AttemptID: row.ScannedID})
		}
	}
	if len(rows) == limit {
		page.Next = rows[len(rows)-1].ScannedID
	}
	return page, nil
}
