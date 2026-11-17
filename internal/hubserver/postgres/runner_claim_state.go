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
	var machineID string
	if snapshot, ok := ctx.Value(runnerHeartbeatSnapshotKey{}).(*runnerHeartbeatSnapshot); ok && snapshot.session == s && snapshot.identity.RunnerID == runner && snapshot.project == project && snapshot.now.Equal(now) {
		machineID = snapshot.machine.ID
		page.State.HostCapacity = int(snapshot.machine.Capacity)
		page.State.RunnerCapacity = int(min(identity.CapacityLimit, identity.ReportedCapacity))
		page.State.Slots = append([]runnerauth.ClaimSlot(nil), snapshot.slots...)
		page.State.PolicyIDs = map[tracker.ProjectID]string{project: snapshot.policyID}
	} else {
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
		machineID = machine.ID
	}
	rows, err := s.queries.RunnerReleasedAttemptCandidates(ctx, pgquery.RunnerReleasedAttemptCandidatesParams{OrganizationID: s.organization, ProjectID: project, RunnerID: runner, MachineID: machineID, AfterID: after, PageSize: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	for _, row := range rows {
		if row.WorkItemID != "" {
			candidate := runnerauth.ReleasedExecution{ProjectID: project, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), AttemptID: row.ScannedID}
			if row.CleanupKnown {
				page.Candidates = append(page.Candidates, candidate)
			} else {
				page.Unclassified = append(page.Unclassified, candidate)
			}
		}
	}
	if len(rows) == limit {
		page.Next = rows[len(rows)-1].ScannedID
	}
	return page, nil
}
