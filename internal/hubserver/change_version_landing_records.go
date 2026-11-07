package hubserver

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hydrateChangeVersion(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, row eventstore.Version) (tracker.ChangeVersion, error) {
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	version, err := content.Version(ctx, row)
	if err != nil {
		return version, err
	}
	var publication eventstore.NativePRPublication
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		publication, err = r.PRPublication(ctx, version.ID)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return version, err
	}
	if err == nil {
		receipt := tracker.NativePRPublication{ChangeID: publication.ChangeID, VersionID: publication.VersionID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceAttemptID: publication.SourceAttemptID, SourceVersion: tracker.NativeChangeReference{ChangeID: publication.ChangeID, VersionID: publication.VersionID, HeadSHA: publication.HeadSHA}, External: publication.External}
		if receipt.Matches(version.ChangeID, version) {
			version.External = &publication.External
		}
	}
	landing, _, err := s.readNativeLandingObservation(ctx, scope, item, tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA})
	if errors.Is(err, corestore.ErrNotFound) {
		return version, nil
	}
	if err != nil {
		return version, err
	}
	version.Landing = landing
	return version, nil
}

func sameNativeLandingObservation(left, right *tracker.NativeLandingReceipt) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	a, b := *left, *right
	a.FromState, a.TargetState, b.FromState, b.TargetState = "", "", "", ""
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func (s *Service) readNativeLandingObservation(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference) (*tracker.NativeLandingReceipt, string, error) {
	var observation eventstore.LandingObservation
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(eventstore.ChangeLandingReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		observation, err = reader.LatestVersionLandingEvent(ctx, scope.project, item, ref)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	event := observation.Event
	payload, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).Event(ctx, event)
	if err != nil {
		return nil, "", err
	}
	if payload.ProjectID != scope.project || payload.AggregateID != item || payload.Data.Run == nil || payload.Data.Run.Runtime == nil || payload.Data.Run.Runtime.Landing == nil {
		return nil, "", artifact.ErrIntegrity
	}
	landing := payload.Data.Run.Runtime.Landing
	if landing.ChangeID != ref.ChangeID || landing.VersionID != ref.VersionID || landing.HeadSHA != ref.HeadSHA {
		return nil, "", artifact.ErrIntegrity
	}
	landing.FromState, landing.TargetState = observation.FromState, observation.TargetState
	return landing, event.AttemptID, nil
}
