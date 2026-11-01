package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) BillingMonthlyPolicy(ctx context.Context, project tracker.ProjectID) (billingstore.MonthlyPolicy, error) {
	var row billingstore.MonthlyPolicy
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at FROM monthly_budget_policies WHERE organization_id=$1 AND project_id=$2 ORDER BY revision DESC LIMIT 1`, s.organization, project).Scan(&row.OrganizationID, &row.ProjectID, &row.Revision, &row.Policy.Enabled, &row.Policy.Mode, &row.Policy.LunaMicros, &row.Policy.RunnerMicros, &row.Policy.SpriteMicros, &row.Policy.TotalMicros, &row.Actor, &row.UpdatedAt)
	return row, mapError(ctx, err)
}

func (s *session) SetBillingMonthlyPolicy(ctx context.Context, row billingstore.MonthlyPolicy, expected tracker.Revision) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := row.Policy.Validate(); err != nil {
		return err
	}
	var id tracker.OrganizationID
	if err := s.db.QueryRowContext(ctx, `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&id); err != nil {
		return mapError(ctx, err)
	}
	previous, err := s.BillingMonthlyPolicy(ctx, row.ProjectID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if previous.Revision != expected {
		return corestore.ErrRevisionConflict
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.organization, row.ProjectID, expected+1, row.Policy.Enabled, row.Policy.EffectiveMode(), row.Policy.LunaMicros, row.Policy.RunnerMicros, row.Policy.SpriteMicros, row.Policy.TotalMicros, row.Actor, row.UpdatedAt)
	return mapError(ctx, err)
}

func (s *session) BillingMonthlyAdmission(ctx context.Context, issueID tracker.WorkItemID) (billingstore.MonthlyAdmission, error) {
	row, err := s.queries.BillingMonthlyAdmission(ctx, pgquery.BillingMonthlyAdmissionParams{OrganizationID: s.organization, IssueID: int64(issueID)})
	return billingstore.MonthlyAdmission{OrganizationID: row.OrganizationID, IssueID: tracker.WorkItemID(row.IssueID), AdmittedAt: row.AdmittedAt}, mapError(ctx, err)
}

func (s *session) RecordBillingMonthlyAdmission(ctx context.Context, row billingstore.MonthlyAdmission) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.IssueID <= 0 || row.AdmittedAt.IsZero() {
		return billingstore.ErrInvalidBudgetFact
	}
	n, err := s.queries.RecordBillingMonthlyAdmission(ctx, pgquery.RecordBillingMonthlyAdmissionParams{OrganizationID: s.organization, IssueID: int64(row.IssueID), AdmittedAt: row.AdmittedAt})
	if err != nil || n != 0 {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) BillingMonthlyLease(ctx context.Context, leaseID tracker.LeaseID) (billingstore.MonthlyLease, error) {
	row, err := s.queries.BillingMonthlyLease(ctx, pgquery.BillingMonthlyLeaseParams{OrganizationID: s.organization, LeaseID: string(leaseID)})
	return billingstore.MonthlyLease{OrganizationID: row.OrganizationID, LeaseID: tracker.LeaseID(row.LeaseID), LunaAPI: row.LunaApi, RunnerAPI: row.RunnerApi, SpriteInfrastructure: row.SpriteInfrastructure}, mapError(ctx, err)
}

func (s *session) RecordBillingMonthlyLease(ctx context.Context, row billingstore.MonthlyLease) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.LeaseID == "" {
		return billingstore.ErrInvalidBudgetFact
	}
	n, err := s.queries.RecordBillingMonthlyLease(ctx, pgquery.RecordBillingMonthlyLeaseParams{OrganizationID: s.organization, LeaseID: string(row.LeaseID), LunaApi: row.LunaAPI, RunnerApi: row.RunnerAPI, SpriteInfrastructure: row.SpriteInfrastructure})
	if err != nil || n != 0 {
		return mapError(ctx, err)
	}
	previous, err := s.BillingMonthlyLease(ctx, row.LeaseID)
	if err != nil {
		return err
	}
	if previous.LunaAPI != row.LunaAPI || previous.RunnerAPI != row.RunnerAPI || previous.SpriteInfrastructure != row.SpriteInfrastructure {
		return corestore.ErrRevisionConflict
	}
	return nil
}
