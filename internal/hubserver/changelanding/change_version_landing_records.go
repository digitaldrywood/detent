package changelanding

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Observations) HydrateChangeVersion(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version) (tracker.ChangeVersion, error) {
	return s.HydrateChangeVersionWithRows(ctx, scope, item, row, nil)
}

func (s *Observations) HydrateChangeVersionWithRows(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version, captured *tenantstore.ChangeVersionRows) (tracker.ChangeVersion, error) {
	content := eventstore.NewContent(s.deps.RunDetailStoreFor(ctx))
	version, err := content.Version(ctx, row)
	if err != nil {
		return version, err
	}
	if frozen := row.FrozenLanding; frozen != nil {
		if frozen.OrganizationID != scope.Organization || frozen.ProjectID != scope.Project || frozen.ChangeID != row.ChangeID || frozen.VersionID != row.ID || frozen.HeadSHA != row.HeadSHA {
			return version, artifact.ErrIntegrity
		}
		selected := *frozen
		selected.WorkItemID = item
		landing, err := content.VersionLanding(ctx, selected)
		if err != nil {
			return version, err
		}
		version.LandingQuality = landing.Quality
	}
	request := tenantstore.ChangeVersionRequest{WorkItemID: item, Reference: tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}}
	if captured == nil {
		captured = &tenantstore.ChangeVersionRows{OrganizationID: scope.Organization, ProjectID: scope.Project, Request: request}
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			publication, err := r.PRPublication(ctx, version.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				captured.Publication = &publication
			}
			reader, ok := r.(eventstore.ChangeLandingReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			landing, err := reader.LatestVersionLandingEvent(ctx, scope.Project, item, request.Reference)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				captured.Landing = &landing
			}
			return nil
		})
		if err != nil {
			return version, err
		}
	}
	if captured.OrganizationID != scope.Organization || captured.ProjectID != scope.Project || captured.Request != request {
		return version, corestore.ErrRevisionConflict
	}
	if publication := captured.Publication; publication != nil {
		if publication.OrganizationID != scope.Organization {
			return version, corestore.ErrTenantMismatch
		}
		receipt := tracker.NativePRPublication{ChangeID: publication.ChangeID, VersionID: publication.VersionID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceAttemptID: publication.SourceAttemptID, SourceVersion: tracker.NativeChangeReference{ChangeID: publication.ChangeID, VersionID: publication.VersionID, HeadSHA: publication.HeadSHA}, External: publication.External}
		if receipt.Matches(version.ChangeID, version) {
			version.External = &publication.External
		}
	}
	if captured.Landing == nil {
		return version, nil
	}
	landing, _, err := s.hydrateNativeLandingObservation(ctx, scope, item, request.Reference, *captured.Landing)
	if err != nil {
		return version, err
	}
	version.Landing = landing
	return version, nil
}

func SameNativeLandingObservation(left, right *tracker.NativeLandingReceipt) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	a, b := *left, *right
	a.FromState, a.TargetState, b.FromState, b.TargetState = "", "", "", ""
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func (s *Observations) ReadNativeLandingObservation(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference) (*tracker.NativeLandingReceipt, string, error) {
	var observation eventstore.LandingObservation
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(eventstore.ChangeLandingReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		observation, err = reader.LatestVersionLandingEvent(ctx, scope.Project, item, ref)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return s.hydrateNativeLandingObservation(ctx, scope, item, ref, observation)
}

func (s *Observations) hydrateNativeLandingObservation(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference, observation eventstore.LandingObservation) (*tracker.NativeLandingReceipt, string, error) {
	event := observation.Event
	if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.WorkItemID != item {
		return nil, "", corestore.ErrTenantMismatch
	}
	payload, err := eventstore.NewContent(s.deps.RunDetailStoreFor(ctx)).Event(ctx, event)
	if err != nil {
		return nil, "", err
	}
	if payload.ProjectID != scope.Project || payload.AggregateID != item || payload.Data.Run == nil || payload.Data.Run.Runtime == nil || payload.Data.Run.Runtime.Landing == nil {
		return nil, "", artifact.ErrIntegrity
	}
	landing := payload.Data.Run.Runtime.Landing
	if landing.ChangeID != ref.ChangeID || landing.VersionID != ref.VersionID || landing.HeadSHA != ref.HeadSHA {
		return nil, "", artifact.ErrIntegrity
	}
	landing.FromState, landing.TargetState = observation.FromState, observation.TargetState
	return landing, event.AttemptID, nil
}
