package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

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
		tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := s.database.requireHostedFeature(ctx, tx, "collaboration", s.config.now()); err != nil {
			return err
		}
		current := scope
		if err := s.recheckHostedMutation(ctx, tx, &current); err != nil {
			return err
		}
		if err := authorizeNativeProject(ctx, tx, current); err != nil {
			return err
		}
		if err := s.requireHostedProject(ctx, tx, current, true); err != nil {
			return err
		}
		if current.credential.Hosted == nil {
			var active bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM api_tokens WHERE id = ? AND scope = ? AND native_only = ? AND revoked_at IS NULL AND (expires_at IS NULL OR julianday(expires_at) > julianday(?)))", current.credential.ID, current.credential.Scope, current.credential.NativeOnly, formatHubTime(s.config.now())).Scan(&active); err != nil {
				return err
			}
			if !active {
				return nativeNotFound()
			}
		}
		if err := s.checkReleasePickDelivery(ctx, tx, current, item, association, s.config.now(), true); err != nil {
			return err
		}
		_, _, err = nativeCommandReceipt(ctx, tx, current, releasePublicationOperation(scope, item), r.IdempotencyKey, hash)
		return err
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
	return s.nativeMutation(c, r.Mutation, r, func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) { return result, nil })
}
