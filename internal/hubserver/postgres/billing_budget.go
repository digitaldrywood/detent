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
	err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at FROM monthly_budget_policies WHERE organization_id=$1 AND project_id=$2 ORDER BY revision DESC LIMIT 1`, s.organization, project).Scan(&row.OrganizationID, &row.ProjectID, &row.Revision, &row.Policy.Enabled, &row.Policy.Mode, &row.Policy.LunaMicros, &row.Policy.RunnerMicros, &row.Policy.SpriteMicros, &row.Policy.TotalMicros, &row.Actor, &row.UpdatedAt)
	return row, mapError(ctx, err)
}

func (s *session) SetBillingMonthlyPolicy(ctx context.Context, row billingstore.MonthlyPolicy, expected tracker.Revision) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := row.Policy.Validate(); err != nil {
		return err
	}
	if err := s.LockAccessOrganization(ctx); err != nil {
		return err
	}
	previous, err := s.BillingMonthlyPolicy(ctx, row.ProjectID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if previous.Revision != expected {
		return corestore.ErrRevisionConflict
	}
	_, err = s.db.Exec(ctx, `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.organization, row.ProjectID, expected+1, row.Policy.Enabled, row.Policy.EffectiveMode(), row.Policy.LunaMicros, row.Policy.RunnerMicros, row.Policy.SpriteMicros, row.Policy.TotalMicros, row.Actor, row.UpdatedAt)
	return mapError(ctx, err)
}

func (s *session) ImportBillingMonthlyPolicy(ctx context.Context, row billingstore.MonthlyPolicy) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.Revision < 1 || row.UpdatedAt.IsZero() {
		return billingstore.ErrInvalidBudgetFact
	}
	if err := row.Policy.Validate(); err != nil {
		return err
	}
	if row.ProjectID != "" {
		if _, err := s.Project(ctx, row.ProjectID); err != nil {
			return err
		}
	}
	row.Policy.Mode = row.Policy.EffectiveMode()
	row.UpdatedAt = recordTime(row.UpdatedAt)
	_, err := s.db.Exec(ctx, `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (organization_id,project_id,revision) DO NOTHING`, s.organization, row.ProjectID, row.Revision, row.Policy.Enabled, row.Policy.Mode, row.Policy.LunaMicros, row.Policy.RunnerMicros, row.Policy.SpriteMicros, row.Policy.TotalMicros, row.Actor, row.UpdatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	var current billingstore.MonthlyPolicy
	if err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,revision,enabled,mode,luna_micros,runner_micros,sprite_micros,total_micros,actor_id,updated_at
FROM monthly_budget_policies WHERE organization_id=$1 AND project_id=$2 AND revision=$3`, s.organization, row.ProjectID, row.Revision).Scan(&current.OrganizationID, &current.ProjectID, &current.Revision, &current.Policy.Enabled, &current.Policy.Mode, &current.Policy.LunaMicros, &current.Policy.RunnerMicros, &current.Policy.SpriteMicros, &current.Policy.TotalMicros, &current.Actor, &current.UpdatedAt); err != nil {
		return mapError(ctx, err)
	}
	if !sameImportedMonthlyPolicy(current, row) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func sameImportedMonthlyPolicy(left, right billingstore.MonthlyPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.Revision == right.Revision &&
		left.Policy.Enabled == right.Policy.Enabled && left.Policy.EffectiveMode() == right.Policy.EffectiveMode() &&
		sameOptionalMicros(left.Policy.LunaMicros, right.Policy.LunaMicros) && sameOptionalMicros(left.Policy.RunnerMicros, right.Policy.RunnerMicros) &&
		sameOptionalMicros(left.Policy.SpriteMicros, right.Policy.SpriteMicros) && sameOptionalMicros(left.Policy.TotalMicros, right.Policy.TotalMicros) &&
		left.Actor == right.Actor && left.UpdatedAt.Equal(right.UpdatedAt)
}

func sameOptionalMicros(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
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
	_, err := s.queries.RecordBillingMonthlyAdmission(ctx, pgquery.RecordBillingMonthlyAdmissionParams{OrganizationID: s.organization, IssueID: int64(row.IssueID), AdmittedAt: row.AdmittedAt})
	return mapError(ctx, err)
}

func (s *session) ImportBillingMonthlyAdmission(ctx context.Context, row billingstore.MonthlyAdmission) error {
	if err := s.RecordBillingMonthlyAdmission(ctx, row); err != nil {
		return err
	}
	current, err := s.BillingMonthlyAdmission(ctx, row.IssueID)
	if err != nil {
		return err
	}
	if !current.AdmittedAt.Equal(recordTime(row.AdmittedAt)) {
		return corestore.ErrRevisionConflict
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
