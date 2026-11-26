package claimsource

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository tenantstore.Repository
	Details    attachment.DetailStore
	Checkpoint func(context.Context, hubrecords.TenantAttemptSnapshot) (*tracker.NativeCheckpoint, error)
}

func VersionPushed(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item int64, version eventstore.Version) (bool, error) {
	if version.AttemptID == "" {
		return false, nil
	}
	receipts, ok := reader.(leasestore.PhaseReceiptReader)
	if !ok {
		return false, hubapi.NativeRunEventStorageUnavailable()
	}
	return receipts.AttemptHasSourceReceipt(ctx, project, item, version.AttemptID, version.HeadSHA, version.PolicyRevisionID)
}

func VersionStored(ctx context.Context, details attachment.DetailStore, organization tracker.OrganizationID, row eventstore.Version) (bool, error) {
	if row.OrganizationID != organization {
		return false, artifact.ErrIntegrity
	}
	content := eventstore.NewContent(details)
	version, err := content.Version(ctx, row)
	if err != nil {
		return false, err
	}
	if version.Source == nil {
		return false, nil
	}
	_, err = content.SourceBundle(ctx, organization, row, *version.Source)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !hubcontent.ExplanationRead(ctx) && (errors.Is(err, artifact.ErrIntegrity) || errors.Is(err, artifact.ErrMissing)) {
		return false, nil
	}
	return err == nil, err
}

func Read(ctx context.Context, deps Deps, organization tracker.OrganizationID, project tracker.ProjectID, item corestore.WorkItem) (hubrecords.ClaimSourceFacts, error) {
	facts := hubrecords.ClaimSourceFacts{Loaded: true}
	var snapshot hubrecords.TenantAttemptSnapshot
	var version *eventstore.Version
	var pushed bool
	err := deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		if workspace, err := r.IsWorkspaceWorkItem(ctx, project, item.NativeID); err != nil || workspace {
			return err
		}
		runner := func(leaseID string) (string, error) {
			identity, err := r.RunnerForLease(ctx, leaseID)
			if errors.Is(err, corestore.ErrNotFound) {
				return "", nil
			}
			return identity.RunnerID, err
		}
		owner := func(leaseID string) (string, string, error) {
			lease, err := r.Lease(ctx, leaseID)
			if err != nil {
				return "", "", err
			}
			runnerID, err := runner(leaseID)
			return lease.MachineID, runnerID, err
		}
		change, err := r.LatestChange(ctx, project, item.NativeID)
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
			pushed, err = VersionPushed(ctx, r, project, item.ID, row)
			if err != nil {
				return err
			}
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
		if attempt.CheckpointObjectKey == nil || attempt.OrganizationID != organization || attempt.IssueID != item.ID {
			return artifact.ErrIntegrity
		}
		facts.AttemptID, facts.CheckpointObjectKey = attempt.ID, *attempt.CheckpointObjectKey
		snapshot.Attempt = attempt
		if snapshot.Lease, err = r.Lease(ctx, attempt.LeaseID); err == nil {
			facts.CheckpointMachine = snapshot.Lease.MachineID
			if facts.CheckpointRunner, err = runner(attempt.LeaseID); err != nil {
				return err
			}
			snapshot.Event, err = r.LatestRunEvent(ctx, project, attempt.ID)
		}
		if err == nil {
			err = hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot)
		}
		if errors.Is(err, corestore.ErrNotFound) || candidateLocal(err) {
			facts.CheckpointUnreadable = true
			return nil
		}
		return err
	})
	if err != nil {
		return facts, err
	}
	if facts.AttemptID != "" && !facts.CheckpointUnreadable {
		checkpoint, err := deps.Checkpoint(ctx, snapshot)
		if candidateLocal(err) {
			facts.CheckpointUnreadable, checkpoint, err = true, nil, nil
		}
		if err != nil {
			return facts, err
		}
		if checkpoint.PreservesSource() {
			facts.Checkpoint = checkpoint
		}
	}
	if pushed {
		facts.VersionStored = true
	} else if version != nil {
		facts.VersionStored, err = VersionStored(ctx, deps.Details, organization, *version)
	}
	return facts, err
}

func candidateLocal(err error) bool {
	if err == nil || errors.Is(err, artifact.ErrStorage) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, artifact.ErrMissing) || errors.Is(err, artifact.ErrIntegrity) || errors.Is(err, attachment.ErrInvalid)
}

func Refusal(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, item corestore.WorkItem, facts hubrecords.ClaimSourceFacts, machine, runner string) (string, error) {
	if !facts.Loaded {
		return "", corestore.ErrRevisionConflict
	}
	reader, ok := r.(leasestore.CheckpointAttemptReader)
	if !ok {
		return "", hubapi.NativeRunEventStorageUnavailable()
	}
	attempt, err := reader.SourceCheckpointAttempt(ctx, item.ID)
	switch {
	case errors.Is(err, corestore.ErrNotFound):
		if facts.AttemptID != "" {
			return "", corestore.ErrRevisionConflict
		}
	case err != nil:
		return "", err
	case attempt.ID != facts.AttemptID || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != facts.CheckpointObjectKey:
		return "", corestore.ErrRevisionConflict
	}
	change, err := r.LatestChange(ctx, project, item.NativeID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return "", err
	}
	current := ""
	if err == nil && change.CurrentVersionID != nil {
		current = *change.CurrentVersionID
	}
	if current != facts.VersionID {
		return "", corestore.ErrRevisionConflict
	}
	if allowed, reason := Allowed(facts, machine, runner); !allowed {
		return reason, nil
	}
	return "", nil
}

func Allowed(facts hubrecords.ClaimSourceFacts, machine, runner string) (bool, string) {
	checkpoint := facts.Checkpoint
	if checkpoint.GitRef() {
		return true, "Checkpoint " + checkpoint.Ref + " is in the project repository; any eligible runner restores it"
	}
	if facts.VersionID != "" && facts.VersionStored {
		return true, "Verified Change source is stored on the Hub; any eligible runner restores it"
	}
	if facts.CheckpointUnreadable {
		return false, fmt.Sprintf("Checkpoint %s on source runner %s is unreadable; restore and verify source before recovery", facts.AttemptID, facts.CheckpointMachine)
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
