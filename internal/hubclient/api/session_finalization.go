package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
)

func (s *RunnerSessions) ReconcileSessionFinalization(ctx context.Context, id int64, value *sessionmodel.Finalization) error {
	if err := nativeEffectError(ctx); err != nil {
		return err
	}
	authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	if !ok || authority.scope != s.base() || authority.lease.ID == "" {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if value == nil {
		current, err := s.snapshot(ctx, id)
		if err != nil {
			return err
		}
		if current.Finalization == nil {
			if _, err := s.ValidateLease(ctx, authority.lease); err != nil {
				return err
			}
			return s.verifyLegacyFinalization(ctx, current)
		}
		value = &current.Finalization.Input
	}
	if value.SessionID != id {
		return errors.New("session finalization identity changed")
	}
	if value.Phase == nil {
		return errors.New("session finalization phase is required")
	}
	key, err := value.Key(sessionmodel.FinalizationCompleteStage)
	if err != nil {
		return err
	}
	current, err := s.mutate(ctx, runnersession.Mutation{
		Key: key, SessionID: id, Finalization: value, Finish: &value.Finish, Usage: &value.Usage, Phase: value.Phase,
	})
	if err != nil {
		return err
	}
	if current.ID != id || current.Finalization == nil || !current.Finalization.Input.Equal(*value) || !current.Finalization.Finished || !current.Finalization.UsageRecorded || !current.Finalization.PhaseRecorded {
		return errors.New("session finalization remains incomplete")
	}
	return nil
}

func (s *RunnerSessions) verifyLegacyFinalization(ctx context.Context, current runnersession.Snapshot) error {
	entries := make([]runnersession.Mutation, 0, 3)
	if current.Finish == nil || current.Revision < 4 {
		return runnersession.ErrFinalizationIncomplete
	}
	for revision := current.Revision; revision > max(int64(0), current.Revision-runnersession.HistoryLimit); revision-- {
		var entry runnersession.Mutation
		path := s.base() + "/runner-sessions/" + strconv.FormatInt(current.ID, 10) + "/history/" + strconv.FormatInt(revision, 10)
		if err := s.client.Request(ctx, http.MethodGet, path, nil, &entry); err != nil {
			return fmt.Errorf("read session finalization history: %w", err)
		}
		entries = append(entries, entry)
		if len(entries) >= 3 && runnersession.VerifyLegacyFinalization(current, entries) == nil {
			return nil
		}
	}
	return runnersession.VerifyLegacyFinalization(current, entries)
}
