package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
)

func (s *Service) readOnboardingProgressRecord(ctx context.Context, scope nativeScope) (onboarding.Progress, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var result onboarding.Progress
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		result, err = r.ProjectOnboardingProgress(ctx, scope.project)
		return err
	})
	return result, projectRecordError(err)
}

func (s *Service) saveOnboardingProgressRecord(c echo.Context, request saveOnboardingRequest) error {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.Progress.Revision < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes and a nonnegative revision are required"))
	}
	scope := nativeRequestScope(c)
	_, viewerKey, err := onboardingViewer(scope.credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSONBlob(http.StatusOK, raw)
	}
	progress := request.Progress
	progress.Revision++
	progress.UpdatedAt = formatHubTime(s.config.now().UTC())
	raw, err := json.Marshal(progress)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now().UTC()}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		writer, ok := w.(projectstore.OnboardingPageWriter)
		if !ok {
			return errors.New("onboarding page storage unavailable")
		}
		if err := writer.InvalidateOnboardingPage(ctx, scope.project, viewerKey); err != nil {
			return err
		}
		if err := w.SaveProjectOnboardingProgress(ctx, scope.project, progress, request.Progress.Revision); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
		if raw, found, readErr := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || readErr != nil {
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			return c.JSONBlob(http.StatusOK, raw)
		}
		current, readErr := s.readOnboardingProgressRecord(ctx, scope)
		if readErr != nil {
			return s.nativeAPIError(c, readErr)
		}
		return s.nativeAPIError(c, nativeConflict(current.Revision))
	}
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if replay != nil {
		raw, err = content.Receipt(ctx, *replay)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSONBlob(http.StatusOK, raw)
}
