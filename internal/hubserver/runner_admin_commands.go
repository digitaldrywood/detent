package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) requireRunnerAdministration(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	return s.requireRunnerAccess(ctx, tx, scope, now, apikey.ScopeWrite)
}

func (s *Service) requireRunnerAccess(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time, required apikey.Scope) error {
	if scope.credential.Runner.RunnerID != "" || scope.credential.NativeOnly && scope.credential.Hosted == nil {
		return operatortool.ErrAccessDenied
	}
	if scope.credential.Hosted != nil {
		if scope.credential.HostedRole == "viewer" {
			return operatortool.ErrAccessDenied
		}
		scope.credential.ManageRunners = !scope.requireHostedAdmin
		if err := s.recheckHostedAuthority(ctx, tx, &scope, required); err != nil {
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
