package hubserver

import (
	"context"
	"errors"
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func tenantRunPRPublication(data tracker.NativeRunData) *tracker.NativePRPublication {
	if data.Finalization != nil && data.Finalization.Publication != nil {
		return data.Finalization.Publication
	}
	if data.Handoff != nil {
		return data.Handoff.Publication
	}
	return nil
}

func tenantPRPublicationRecord(authority NativeRunEventAuthority, publication tracker.NativePRPublication) eventstore.NativePRPublication {
	return eventstore.NativePRPublication{OrganizationID: authority.OrganizationID, VersionID: publication.VersionID, ChangeID: publication.ChangeID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceAttemptID: publication.SourceAttemptID, RecorderAttemptID: authority.Event.Data.AttemptID, External: publication.External}
}

type preparedNativePRPublication struct {
	change   eventstore.Change
	version  eventstore.Version
	record   eventstore.NativePRPublication
	diff     leasestore.AttemptDiff
	retained bool
}

func (s *Service) prepareTenantPRPublication(ctx context.Context, authority *NativeRunEventAuthority, previous *tracker.NativeRunData) (preparedNativePRPublication, error) {
	data := authority.Event.Data
	publication := tenantRunPRPublication(data)
	if data.Handoff != nil && data.Finalization != nil && data.Handoff.Publication != nil && data.Finalization.Publication != nil && !reflect.DeepEqual(data.Handoff.Publication, data.Finalization.Publication) {
		return preparedNativePRPublication{}, nativeExecutionConflict("PR publication receipts disagree")
	}
	if publication == nil {
		return preparedNativePRPublication{}, nil
	}
	prepared := preparedNativePRPublication{record: tenantPRPublicationRecord(*authority, *publication)}
	err := s.config.TenantRepository.ReadTenant(ctx, authority.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		prepared.change, err = reader.Change(ctx, publication.ChangeID)
		if err != nil {
			return err
		}
		prepared.version, err = reader.Version(ctx, publication.ChangeID, publication.VersionID)
		if err != nil {
			return err
		}
		stored, err := reader.PRPublication(ctx, publication.VersionID)
		if err == nil {
			expected := prepared.record
			expected.RecorderAttemptID = stored.RecorderAttemptID
			if expected != stored {
				return corestore.ErrRevisionConflict
			}
			prepared.retained = true
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		prepared.diff, err = reader.LatestAttemptDiff(ctx, authority.ProjectID, data.AttemptID, tracker.DiffSourceAttempt)
		return err
	})
	if err != nil {
		return preparedNativePRPublication{}, err
	}
	change := prepared.change
	version, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).Version(ctx, prepared.version)
	if err != nil {
		return preparedNativePRPublication{}, err
	}
	if change.ProjectID != authority.ProjectID || change.WorkItemID != authority.WorkItem.NativeID || change.CurrentVersionID == nil || *change.CurrentVersionID != publication.VersionID || !version.Policy.Gates.GitHubPullRequest || !publication.Matches(publication.ChangeID, version) {
		return preparedNativePRPublication{}, nativeExecutionConflict("PR publication no longer matches the current Change version or source")
	}
	if !prepared.retained {
		if previous == nil || previous.Handoff == nil || previous.Handoff.ExternalEffect != "pr_create" || previous.Handoff.EffectState != "confirmed" || previous.Handoff.EffectID != publication.EffectID("pr_create") || previous.Handoff.Change == nil || *previous.Handoff.Change != publication.SourceVersion || previous.Handoff.HeadSHA != publication.HeadSHA {
			return preparedNativePRPublication{}, nativeExecutionConflict("PR publication requires its exact confirmed checkpoint")
		}
		diff := prepared.diff
		if diff.WorkItemID != authority.WorkItem.NativeID || diff.Producer.ID != data.AttemptID || diff.Producer.LeaseID != data.LeaseID || diff.Producer.FencingToken != data.FencingToken || diff.HeadSHA != publication.HeadSHA || diff.Generation.Seq < data.Sequence || diff.Generation.Seq > authority.BatchThrough {
			return preparedNativePRPublication{}, corestore.ErrRevisionConflict
		}
	}
	return prepared, nil
}

func (s *Service) validateTenantPRPublication(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	publication := tenantRunPRPublication(authority.Event.Data)
	if publication == nil {
		return nil
	}
	prepared := authority.Publication
	if prepared == nil {
		return nativeRunEventStorageUnavailable()
	}
	change, err := reader.Change(ctx, publication.ChangeID)
	if err != nil {
		return err
	}
	version, err := reader.Version(ctx, publication.ChangeID, publication.VersionID)
	if err != nil {
		return err
	}
	if change.OrganizationID != authority.OrganizationID || change.ProjectID != authority.ProjectID || change.WorkItemID != authority.WorkItem.NativeID || valueOrEmpty(change.CurrentVersionID) != publication.VersionID || !reflect.DeepEqual(version, prepared.version) {
		return corestore.ErrRevisionConflict
	}
	if prepared.retained {
		stored, err := reader.PRPublication(ctx, publication.VersionID)
		if err != nil {
			return err
		}
		expected := prepared.record
		expected.RecorderAttemptID = stored.RecorderAttemptID
		if expected != stored {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	diff, err := reader.LatestAttemptDiff(ctx, authority.ProjectID, authority.Event.Data.AttemptID, tracker.DiffSourceAttempt)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(diff, prepared.diff) {
		return corestore.ErrRevisionConflict
	}
	return nil
}
