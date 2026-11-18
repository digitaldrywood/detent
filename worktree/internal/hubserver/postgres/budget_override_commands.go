package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type BudgetOverrideCommandWriter interface {
	BudgetOverrideWriter
	CompareAndSetBudgetOverride(context.Context, BudgetOverride, BudgetOverridePolicy, int64) (BudgetOverride, error)
}

func (s *session) CompareAndSetBudgetOverride(ctx context.Context, value BudgetOverride, policy BudgetOverridePolicy, expected int64) (BudgetOverride, error) {
	if expected < 0 {
		return BudgetOverride{}, ErrBudgetOverride
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: value.ProjectID}); err != nil {
		return BudgetOverride{}, mapError(ctx, err)
	}
	current, err := s.ActiveBudgetOverride(ctx, value.ProjectID, policy.Now)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return BudgetOverride{}, err
	}
	if err == nil && current.Revision != expected || errors.Is(err, corestore.ErrNotFound) && expected != 0 {
		return BudgetOverride{}, corestore.ErrRevisionConflict
	}
	return s.SetBudgetOverride(ctx, value, policy)
}
