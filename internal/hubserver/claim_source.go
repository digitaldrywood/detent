package hubserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) readClaimSourceFacts(ctx context.Context, scope hubscope.NativeScope, item corestore.WorkItem) (hubrecords.ClaimSourceFacts, error) {
	facts := hubrecords.ClaimSourceFacts{Loaded: true}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return facts, hubapi.NativeRunEventStorageUnavailable()
	}
	var snapshot hubrecords.TenantAttemptSnapshot
	var version *eventstore.Version
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner := func(leaseID string) (string, string, error) {
			lease, err := r.Lease(ctx, leaseID)
			if err != nil {
				return "", "", err
			}
			identity, err := r.RunnerForLease(ctx, leaseID)
			if errors.Is(err, corestore.ErrNotFound) {
				return lease.MachineID, "", nil
			}
			return lease.MachineID, identity.RunnerID, err
		}
		change, err := r.LatestChange(ctx, scope.Project, item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && change.CurrentVersionID != nil && *change.CurrentVersionID != "" {
			row, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &row
			facts.ChangeID, facts.VersionID = change.ID, row.ID
			if row.AttemptID != "" {
				attempt, err := r.Attempt(ctx, row.AttemptID)
				if err == nil && attempt.IssueID == item.ID {
					facts.VersionMachine, facts.VersionRunner, err = owner(attempt.LeaseID)
				}
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
		}
		reader, ok := r.(leasestore.CheckpointAttemptReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		attempt, err := reader.SourceCheckpointAttempt(ctx, item.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if attempt.CheckpointObjectKey == nil || attempt.OrganizationID != scope.Organization || attempt.IssueID != item.ID {
			return artifact.ErrIntegrity
		}
		facts.AttemptID, facts.CheckpointObjectKey = attempt.ID, *attempt.CheckpointObjectKey
		snapshot.Attempt = attempt
		if snapshot.Lease, err = r.Lease(ctx, attempt.LeaseID); err != nil {
			return err
		}
		if snapshot.Event, err = r.LatestRunEvent(ctx, scope.Project, attempt.ID); err != nil {
			return err
		}
		if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
			return err
		}
		facts.CheckpointMachine, facts.CheckpointRunner, err = owner(attempt.LeaseID)
		return err
	})
	if err != nil {
		return facts, err
	}
	if facts.AttemptID != "" {
		checkpoint, err := s.readNativeAttemptCheckpointTenant(ctx, snapshot)
		if err != nil {
			return facts, err
		}
		if checkpoint.PreservesSource() {
			facts.Checkpoint = checkpoint
		}
	}
	if version != nil {
		facts.VersionStored, err = s.verifiedNativeSourceTenant(ctx, scope, *version)
	}
	return facts, err
}

func claimSourceCurrent(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item corestore.WorkItem, facts hubrecords.ClaimSourceFacts) error {
	if !facts.Loaded {
		return corestore.ErrRevisionConflict
	}
	reader, ok := r.(leasestore.CheckpointAttemptReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	attempt, err := reader.SourceCheckpointAttempt(ctx, item.ID)
	switch {
	case errors.Is(err, corestore.ErrNotFound):
		if facts.AttemptID != "" {
			return corestore.ErrRevisionConflict
		}
	case err != nil:
		return err
	case attempt.ID != facts.AttemptID || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != facts.CheckpointObjectKey:
		return corestore.ErrRevisionConflict
	}
	change, err := r.LatestChange(ctx, scope.Project, item.NativeID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	current := ""
	if err == nil && change.CurrentVersionID != nil {
		current = *change.CurrentVersionID
	}
	if current != facts.VersionID {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func claimSourceAllowed(facts hubrecords.ClaimSourceFacts, machine, runner string) (bool, string) {
	checkpoint := facts.Checkpoint
	if checkpoint.GitRef() {
		return true, "Checkpoint " + checkpoint.Ref + " is in the project repository; any eligible runner restores it"
	}
	if facts.VersionID != "" && facts.VersionStored {
		return true, "Verified Change source is stored on the Hub; any eligible runner restores it"
	}
	if checkpoint != nil && (checkpoint.Availability == "missing" || checkpoint.Availability == "inaccessible" || checkpoint.Storage == "customer_store") {
		return false, fmt.Sprintf("Checkpoint %s on source runner %s is %s; restore and verify source before recovery", facts.AttemptID, facts.CheckpointMachine, checkpoint.Availability)
	}
	if facts.VersionID != "" {
		owner, ownerRunner := facts.CheckpointMachine, facts.CheckpointRunner
		if checkpoint == nil {
			owner, ownerRunner = facts.VersionMachine, facts.VersionRunner
		}
		reason := fmt.Sprintf("Change %s version %s has no verified Hub source; recapture the exact head on source runner %s", facts.ChangeID, facts.VersionID, owner)
		return owner != "" && machine == owner && (ownerRunner == "" || ownerRunner == runner), reason
	}
	return true, "No source is pinned to a runner"
}
