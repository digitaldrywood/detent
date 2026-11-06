package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReleasePickPublisher interface {
	PublishReleasePick(context.Context, tracker.NativeWorkItemID, tracker.ReleasePickInput, func(context.Context) error) (tracker.ReleasePickPublication, error)
}

func (s *Service) prepareReleasePublication(ctx context.Context, scope nativeScope, item string, r tracker.PublishReleasePick) (tracker.ReleasePickPublication, error) {
	var result tracker.ReleasePickPublication
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin || scope.credential.Runner.RunnerID != "" {
		return result, nativeNotFound()
	}
	if r.PRNumber < 0 {
		return result, nativeInvalid("Publication requires a valid PR identity")
	}
	association := tracker.AssociateReleasePick{ReleasePickInput: r.ReleasePickInput, ExpectedRevision: r.ExpectedRevision}
	if err := validatePreparedReleasePick(r.ReleasePickInput, r.ExpectedRevision); err != nil {
		return result, err
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 128 {
		return result, nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := nativeCommandHash(r)
	if err != nil {
		return result, err
	}
	authorize := func(ctx context.Context) error {
		if s.config.TenantRepository == nil {
			return workItemStorageUnavailable()
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return err
		}
		plan, err := s.prepareReleaseDeliveryCheck(ctx, scope, item, association, s.config.now(), true)
		if err != nil {
			return err
		}
		return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.recheckReleaseDelivery(ctx, w, scope, plan); err != nil {
				return err
			}
			receipt, err := w.Receipt(ctx, scope.credential.ID, releasePublicationOperation(scope, item), r.IdempotencyKey)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			return nil
		})
	}
	if err := authorize(ctx); err != nil {
		return result, err
	}
	publisher, ok := s.config.ReconcileBackend.(ReleasePickPublisher)
	if !ok {
		return result, errors.New("repository transport does not support release PR publication")
	}
	result, err = publisher.PublishReleasePick(ctx, tracker.NativeWorkItemID(item), r.ReleasePickInput, authorize)
	if errors.Is(err, ErrReleasePickMismatch) {
		return result, nativeInvalid("Release publication provenance verification failed")
	}
	if err != nil {
		return result, err
	}
	expected := r.ReleasePickInput
	expected.PRNumber = result.PRNumber
	if result.PRNumber < 1 || result.WorkItemID != tracker.NativeWorkItemID(item) || result.ReleasePickInput != expected {
		return tracker.ReleasePickPublication{}, errors.New("release publisher returned a mismatched receipt")
	}
	return result, nil
}

func releasePublicationOperation(scope nativeScope, item string) string {
	return "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items/" + item + "/release-pick/publication"
}

func (s *Service) publishReleasePick(c echo.Context) error {
	var r tracker.PublishReleasePick
	if err := decodeAPIJSON(c, &r); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.prepareReleasePublication(c.Request().Context(), nativeRequestScope(c), c.Param("item"), r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.recordReleasePublication(c, r, result)
}
