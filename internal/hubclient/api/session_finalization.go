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
	current, err := s.snapshot(ctx, id)
	if err != nil {
		return err
	}
	if value == nil {
		if current.Finalization != nil {
			value = &current.Finalization.Input
		} else {
			if _, err := s.ValidateLease(ctx, authority.lease); err != nil {
				return err
			}
			return s.verifyLegacyFinalization(ctx, current)
		}
	}
	if value.SessionID != id || current.ID != id {
		return errors.New("session finalization identity changed")
	}
	if value.Phase == nil {
		return errors.New("session finalization phase is required")
	}
	if current.Finalization != nil && !current.Finalization.Input.Equal(*value) {
		return errors.New("session finalization inputs changed")
	}
	requests := []runnersession.Mutation{{SessionID: id, Finalization: value, Finish: &value.Finish}, {SessionID: id, Finalization: value, Usage: &value.Usage}}
	if value.Phase != nil {
		requests = append(requests, runnersession.Mutation{SessionID: id, Finalization: value, Phase: value.Phase})
	}
	wrote := false
	for _, request := range requests {
		stage := "finish"
		if request.Usage != nil {
			stage = "usage"
		}
		if request.Phase != nil {
			stage = "phase"
		}
		request.Key, err = value.Key(stage)
		if err != nil {
			return err
		}
		if current.Finalization != nil && (request.Finish != nil && current.Finalization.Finished || request.Usage != nil && current.Finalization.UsageRecorded || request.Phase != nil && current.Finalization.PhaseRecorded) {
			continue
		}
		current, err = s.mutate(ctx, request)
		if err != nil {
			return err
		}
		wrote = true
	}
	if current.Finalization == nil || !current.Finalization.Finished || !current.Finalization.UsageRecorded || !current.Finalization.PhaseRecorded {
		return errors.New("session finalization remains incomplete")
	}
	if !wrote {
		request := requests[0]
		request.Key, err = value.Key("finish")
		if err != nil {
			return err
		}
		_, err := s.mutate(ctx, request)
		return err
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
