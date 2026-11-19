package claimapi

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const ClaimSourceClassificationLimit = 50

type claimSourceState int

const (
	claimSourceKnown claimSourceState = iota
	claimSourcePending
	claimSourceUnverifiable
)

type claimSourceBudget struct {
	remaining int
}

func (d Deps) classifyClaimCandidateSource(ctx context.Context, scope hubscope.NativeScope, candidate claimstore.Candidate, budget *claimSourceBudget) (claimSourceState, error) {
	if budget.remaining < 1 {
		return claimSourcePending, nil
	}
	var snapshots []hubrecords.TenantAttemptSnapshot
	more, classifiedTail := false, false
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		snapshots, more, classifiedTail = nil, false, false
		reader, ok := r.(leasestore.CheckpointAttemptReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		attempts, err := reader.SourceCheckpointAttempts(ctx, candidate.ID, budget.remaining+1)
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if attempt.CheckpointPreservesSource != nil {
				classifiedTail = true
				break
			}
			if len(snapshots) == budget.remaining {
				more = true
				break
			}
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Event: eventstore.Event{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: candidate.WorkItemID}}
			if err := hubrecords.ReadTenantAttemptCheckpointPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		return claimSourcePending, err
	}
	facts := make([]leasestore.CheckpointSourceFact, 0, len(snapshots))
	preserved, unverifiable := false, false
	for _, snapshot := range snapshots {
		budget.remaining--
		checkpoint, err := d.ReadCheckpoint(ctx, snapshot)
		if err == nil && checkpoint == nil {
			err = artifact.ErrIntegrity
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return claimSourcePending, ctxErr
			}
			d.Logger.WarnContext(ctx, "claim checkpoint source classification unavailable", "organization_id", scope.Organization, "project_id", scope.Project, "work_item_id", candidate.WorkItemID, "attempt_id", snapshot.Attempt.ID, "error", err)
			unverifiable = true
			break
		}
		facts = append(facts, leasestore.CheckpointSourceFact{AttemptID: snapshot.Attempt.ID, IssueID: snapshot.Attempt.IssueID, Sequence: snapshot.Attempt.Sequence, CheckpointObjectKey: *snapshot.Attempt.CheckpointObjectKey, PreservesSource: checkpoint.PreservesSource()})
		if checkpoint.PreservesSource() {
			preserved = true
			break
		}
	}
	if len(facts) > 0 {
		if err := d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(leasestore.CheckpointSourceWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			for _, fact := range facts {
				if err := owner.SetCheckpointSource(ctx, fact); err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
					return err
				}
			}
			return nil
		}); err != nil {
			return claimSourcePending, err
		}
	}
	switch {
	case unverifiable:
		return claimSourceUnverifiable, nil
	case preserved || classifiedTail:
		return claimSourceKnown, nil
	case more:
		return claimSourcePending, nil
	}
	return claimSourceKnown, nil
}
