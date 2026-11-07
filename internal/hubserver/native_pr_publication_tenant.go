package hubserver

import (
	"context"
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"reflect"

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
	change     eventstore.Change
	version    eventstore.Version
	checkpoint string
	diff       leasestore.AttemptDiff
}

func (s *Service) prepareTenantPRPublication(ctx context.Context, authority *NativeRunEventAuthority) (*preparedNativePRPublication, error) {
	data := authority.Event.Data
	publication := tenantRunPRPublication(data)
	if data.Handoff != nil && data.Finalization != nil && data.Handoff.Publication != nil && data.Finalization.Publication != nil && !reflect.DeepEqual(data.Handoff.Publication, data.Finalization.Publication) {
		return nil, nativeExecutionConflict("PR publication receipts disagree")
	}
	if publication == nil {
		return nil, nil
	}
	prepared := &preparedNativePRPublication{}
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
		attempt, err := reader.Attempt(ctx, data.AttemptID)
		if err != nil {
			return err
		}
		if attempt.CheckpointObjectKey == nil {
			return nativeExecutionConflict("PR publication requires its exact confirmed checkpoint")
		}
		prepared.checkpoint = *attempt.CheckpointObjectKey
		prepared.diff, err = reader.AttemptDiff(ctx, authority.ProjectID, data.AttemptID, tracker.DiffGeneration{Source: "attempt", Seq: data.Sequence})
		if err != nil {
			return nativeExecutionConflict("PR publication requires the current fenced final diff")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	change := prepared.change
	version, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).Version(ctx, prepared.version)
	if err != nil {
		return nil, err
	}
	if change.ProjectID != authority.ProjectID || change.WorkItemID != authority.WorkItem.NativeID || change.CurrentVersionID == nil || *change.CurrentVersionID != publication.VersionID || !version.Policy.Gates.GitHubPullRequest || !publication.Matches(publication.ChangeID, version) {
		return nil, nativeExecutionConflict("PR publication no longer matches the current Change version or source")
	}
	store, ok := s.runDetailStore().(attachment.CanonicalDetailStore)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	raw, err := store.ReadCanonicalDetail(ctx, string(authority.OrganizationID), string(authority.ProjectID), prepared.checkpoint)
	if err != nil {
		return nil, err
	}
	var payload checkpointPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	checkpoint := payload.Checkpoint
	if checkpoint == nil || checkpoint.ExternalEffect != "pr_create" || checkpoint.EffectState != "confirmed" || checkpoint.EffectID != publication.EffectID("pr_create") || checkpoint.Change == nil || *checkpoint.Change != publication.SourceVersion || checkpoint.HeadSHA != publication.HeadSHA {
		return nil, nativeExecutionConflict("PR publication requires its exact confirmed checkpoint")
	}
	diff := prepared.diff
	if diff.WorkItemID != authority.WorkItem.NativeID || diff.Producer.LeaseID != data.LeaseID || diff.Producer.FencingToken != data.FencingToken || diff.HeadSHA != publication.HeadSHA {
		return nil, corestore.ErrRevisionConflict
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
	attempt, err := reader.Attempt(ctx, authority.Event.Data.AttemptID)
	if err != nil {
		return err
	}
	diff, err := reader.AttemptDiff(ctx, authority.ProjectID, authority.Event.Data.AttemptID, prepared.diff.Generation)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(change, prepared.change) || !reflect.DeepEqual(version, prepared.version) || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey != prepared.checkpoint || !reflect.DeepEqual(diff, prepared.diff) {
		return corestore.ErrRevisionConflict
	}
	return nil
}
