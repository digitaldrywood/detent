package postgres

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrBudgetOverride = errors.New("invalid budget override or override policy")

type BudgetOverride struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	PerDayMaxUSD   *float64
	PerIssueMaxUSD *float64
	ExpiresAt      time.Time
	CreatedAt      time.Time
	Reason         string
	Revision       int64
}
type BudgetOverridePolicy struct {
	ProjectID          tracker.ProjectID
	SettingsRevision   int64
	Enabled            bool
	BasePerDayMaxUSD   float64
	BasePerIssueMaxUSD float64
	MaxDuration        time.Duration
	MaxMultiplier      float64
	Now                time.Time
}
type BudgetOverrideReader interface {
	ActiveBudgetOverride(context.Context, tracker.ProjectID, time.Time) (BudgetOverride, error)
	ListActiveBudgetOverrides(context.Context, time.Time, tracker.ProjectID, int) ([]BudgetOverride, error)
}
type BudgetOverrideWriter interface {
	BudgetOverrideReader
	SetBudgetOverride(context.Context, BudgetOverride, BudgetOverridePolicy) (BudgetOverride, error)
	ClearBudgetOverride(context.Context, tracker.ProjectID) error
}

func (r *Repository) ReadBudgetOverrides(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(BudgetOverrideReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteBudgetOverrides(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(BudgetOverrideWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func budgetOverrideRecord(row pgquery.ProjectBudgetOverride) BudgetOverride {
	result := BudgetOverride{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, Reason: row.Reason, Revision: row.Revision}
	if row.PerDayMaxUsd.Valid {
		result.PerDayMaxUSD = new(row.PerDayMaxUsd.Float64)
	}
	if row.PerIssueMaxUsd.Valid {
		result.PerIssueMaxUSD = new(row.PerIssueMaxUsd.Float64)
	}
	return result
}
func (s *session) ActiveBudgetOverride(ctx context.Context, project tracker.ProjectID, at time.Time) (BudgetOverride, error) {
	if project == "" || at.IsZero() {
		return BudgetOverride{}, ErrBudgetOverride
	}
	row, err := s.queries.ActiveBudgetOverride(ctx, pgquery.ActiveBudgetOverrideParams{OrganizationID: s.organization, ProjectID: project, ExpiresAt: at})
	return budgetOverrideRecord(row), mapError(ctx, err)
}
func (s *session) ListActiveBudgetOverrides(ctx context.Context, at time.Time, after tracker.ProjectID, limit int) ([]BudgetOverride, error) {
	if at.IsZero() {
		return nil, ErrBudgetOverride
	}
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListActiveBudgetOverrides(ctx, pgquery.ListActiveBudgetOverridesParams{OrganizationID: s.organization, ExpiresAt: at, ProjectID: after, Limit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]BudgetOverride, len(rows))
	for i, row := range rows {
		result[i] = budgetOverrideRecord(row)
	}
	return result, nil
}
func validOverrideCap(value *float64, base, multiplier float64) bool {
	if value == nil {
		return true
	}
	maximum := base * multiplier
	return *value > 0 && base > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0) && !math.IsNaN(maximum) && !math.IsInf(maximum, 0) && *value <= maximum
}
func (s *session) SetBudgetOverride(ctx context.Context, override BudgetOverride, policy BudgetOverridePolicy) (BudgetOverride, error) {
	if err := s.checkOrganization(override.OrganizationID); err != nil {
		return BudgetOverride{}, err
	}
	override.Reason = strings.TrimSpace(override.Reason)
	if override.Revision < 0 || override.ProjectID == "" || override.ProjectID != policy.ProjectID || !policy.Enabled || policy.SettingsRevision < 1 || policy.Now.IsZero() || policy.MaxDuration <= 0 || policy.MaxMultiplier <= 0 || math.IsNaN(policy.MaxMultiplier) || math.IsInf(policy.MaxMultiplier, 0) || override.Reason == "" || override.PerDayMaxUSD == nil && override.PerIssueMaxUSD == nil || !override.ExpiresAt.After(policy.Now) || override.ExpiresAt.Sub(policy.Now) > policy.MaxDuration || !validOverrideCap(override.PerDayMaxUSD, policy.BasePerDayMaxUSD, policy.MaxMultiplier) || !validOverrideCap(override.PerIssueMaxUSD, policy.BasePerIssueMaxUSD, policy.MaxMultiplier) {
		return BudgetOverride{}, ErrBudgetOverride
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: override.ProjectID}); err != nil {
		return BudgetOverride{}, mapError(ctx, err)
	}
	project, err := s.ProjectRecord(ctx, override.ProjectID)
	if err != nil {
		return BudgetOverride{}, err
	}
	if project.Settings.Revision != policy.SettingsRevision {
		return BudgetOverride{}, corestore.ErrRevisionConflict
	}
	current, err := s.queries.CurrentBudgetOverride(ctx, pgquery.CurrentBudgetOverrideParams{OrganizationID: s.organization, ProjectID: override.ProjectID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BudgetOverride{}, mapError(ctx, err)
	}
	if override.Revision > 0 && (err != nil || current.Revision != override.Revision) {
		return BudgetOverride{}, corestore.ErrRevisionConflict
	}
	override.CreatedAt = policy.Now.UTC()
	if err == nil && current.ExpiresAt.After(policy.Now) {
		override.CreatedAt = current.CreatedAt
	}
	if override.ExpiresAt.After(override.CreatedAt.Add(policy.MaxDuration)) {
		return BudgetOverride{}, ErrBudgetOverride
	}
	arg := pgquery.SetBudgetOverrideParams{OrganizationID: s.organization, ProjectID: override.ProjectID, ExpiresAt: override.ExpiresAt, CreatedAt: override.CreatedAt, Reason: override.Reason}
	if override.PerDayMaxUSD != nil {
		arg.PerDayMaxUsd = pgtype.Float8{Float64: *override.PerDayMaxUSD, Valid: true}
	}
	if override.PerIssueMaxUSD != nil {
		arg.PerIssueMaxUsd = pgtype.Float8{Float64: *override.PerIssueMaxUSD, Valid: true}
	}
	row, err := s.queries.SetBudgetOverride(ctx, arg)
	return budgetOverrideRecord(row), mapError(ctx, err)
}
func (s *session) ClearBudgetOverride(ctx context.Context, project tracker.ProjectID) error {
	if project == "" {
		return ErrBudgetOverride
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: project}); err != nil {
		return mapError(ctx, err)
	}
	count, err := s.queries.ClearBudgetOverride(ctx, pgquery.ClearBudgetOverrideParams{OrganizationID: s.organization, ProjectID: project})
	return affected(ctx, count, err)
}
