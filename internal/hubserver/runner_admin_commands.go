package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

// runnerAdminTransaction is the extracted administration application boundary.
// Worker claim/lease/heartbeat transactions keep their existing protocol path.
func (s *Service) runnerAdminTransaction(ctx context.Context, scope nativeScope, completion bool, operation func(context.Context, *sql.Tx, time.Time) (any, error)) (any, error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
		return nil, err
	}
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	value, err := operation(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, completion); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notifications.notify(dispatchNotificationKey(scope.organization))
	return value, nil
}

func (s *Service) requireRunnerAdministration(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	if scope.credential.Runner.RunnerID != "" || scope.credential.NativeOnly && scope.credential.Hosted == nil {
		return operatortool.ErrAccessDenied
	}
	if scope.credential.Hosted != nil {
		// Use current application authority, including the same all-project runner
		// grants checked by the dashboard, inside the command transaction.
		if scope.credential.HostedRole == "viewer" {
			return operatortool.ErrAccessDenied
		}
		scope.credential.ManageRunners = !scope.requireHostedAdmin
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
	} else if scope.credential.Scope != apiScopeAdmin {
		return operatortool.ErrAccessDenied
	}
	if err := requireCredentialAuthority(ctx, tx, scope.credential, now); err != nil {
		return err
	}
	return nil
}
