package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (s *Service) prepareChangeSource(ctx context.Context, scope nativeScope, item, changeID, operation string, request tracker.PublishChangeVersion) error {
	if request.Source == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	replay, err := s.authorizeChangeSource(ctx, scope, item, changeID, operation, request)
	if err != nil || replay {
		return err
	}
	if err := request.Source.Validate(request.BaseSHA, request.HeadSHA, request.SourceBundle); err != nil {
		return nativeRejection(nativeInvalid(err.Error()), "version_validation", "source")
	}
	return s.verifyChangeSource(ctx, *request.Source, request.SourceBundle)
}

func (s *Service) authorizeChangeSource(ctx context.Context, scope nativeScope, item, changeID, operation string, request tracker.PublishChangeVersion) (replay bool, err error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return false, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return false, err
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return false, err
	}
	if _, found, err := nativeCommandReceipt(ctx, tx, scope, operation, request.IdempotencyKey, hash); err != nil || found {
		return found, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return false, err
	}
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return false, err
	}
	if scope.credential.Scope == apiScopeWorker {
		if err := requireNativeMutationLease(ctx, tx, scope, item, request.Mutation, now); err != nil {
			return false, err
		}
	}
	change, err := readChange(ctx, tx, scope, item, changeID)
	if err != nil {
		return false, err
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return false, nativePublicInvalid("Publish through the Change Request's primary issue")
	}
	return false, requireChangeRun(ctx, tx, scope, change, request)
}

func (s *Service) verifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	select {
	case s.sourceVerification <- struct{}{}:
		defer func() { <-s.sourceVerification }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := workspace.VerifyChangeSource(ctx, source, bundle); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nativeRejection(nativeInvalid(err.Error()), "source_verification", "source")
	}
	return nil
}
