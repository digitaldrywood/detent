package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProviderReservation(ctx context.Context, lease tracker.LeaseID) (claimstore.ProviderReservation, error) {
	var row claimstore.ProviderReservation
	var report providercapacity.Report
	var resetAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,lease_id,requirement_role,requirement_backend,requirement_model,provider,backend,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at,reason
		FROM provider_reservations WHERE organization_id=$1 AND lease_id=$2`, s.organization, lease).Scan(&row.OrganizationID, &row.LeaseID, &row.Reservation.Role, &row.Reservation.Backend, &row.Reservation.Model, &report.Provider, &report.Backend, &report.AccountAlias, &report.SharedAccountAlias, &report.Billing, &report.MaxConcurrent, &report.Availability, &report.ObservedAt, &resetAt, &row.Reservation.Reason)
	if err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	if resetAt.Valid {
		report.ResetAt = resetAt.Time
	}
	report.Models = []string{}
	report.ModelDetails = []providercapacity.ModelDetail{}
	report.UsageWindows = []providercapacity.UsageWindow{}
	rows, err := s.db.QueryContext(ctx, `SELECT position,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy
		FROM provider_reservation_models WHERE organization_id=$1 AND lease_id=$2 ORDER BY position`, s.organization, lease)
	if err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	defer rows.Close()
	modelDetails := make(map[string]int)
	for rows.Next() {
		var position int
		var model string
		var hasDetail, isDefault, legacy bool
		var label, provider, defaultEffort string
		if err := rows.Scan(&position, &model, &hasDetail, &label, &provider, &isDefault, &defaultEffort, &legacy); err != nil {
			rows.Close()
			return claimstore.ProviderReservation{}, mapError(ctx, err)
		}
		if position != len(report.Models) {
			rows.Close()
			return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
		}
		report.Models = append(report.Models, model)
		if hasDetail {
			modelDetails[model] = len(report.ModelDetails)
			report.ModelDetails = append(report.ModelDetails, providercapacity.ModelDetail{ID: model, Label: label, Provider: provider, Default: isDefault, DefaultReasoningEffort: defaultEffort, Legacy: legacy})
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		rows.Close()
		return claimstore.ProviderReservation{}, err
	}
	if err := rows.Close(); err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT model_id,position,effort FROM provider_reservation_model_efforts WHERE organization_id=$1 AND lease_id=$2 ORDER BY model_id,position`, s.organization, lease)
	if err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var model, effort string
		var position int
		if err := rows.Scan(&model, &position, &effort); err != nil {
			rows.Close()
			return claimstore.ProviderReservation{}, mapError(ctx, err)
		}
		index, ok := modelDetails[model]
		if !ok || position != len(report.ModelDetails[index].ReasoningEfforts) {
			rows.Close()
			return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
		}
		report.ModelDetails[index].ReasoningEfforts = append(report.ModelDetails[index].ReasoningEfforts, effort)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		rows.Close()
		return claimstore.ProviderReservation{}, err
	}
	if err := rows.Close(); err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT position,kind,percent_used,reset_at,observed_at FROM provider_reservation_usage_windows WHERE organization_id=$1 AND lease_id=$2 ORDER BY position`, s.organization, lease)
	if err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var window providercapacity.UsageWindow
		var percent sql.NullFloat64
		var reset, observed sql.NullTime
		if err := rows.Scan(&position, &window.Kind, &percent, &reset, &observed); err != nil {
			rows.Close()
			return claimstore.ProviderReservation{}, mapError(ctx, err)
		}
		if position != len(report.UsageWindows) {
			rows.Close()
			return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
		}
		if percent.Valid {
			value := percent.Float64
			window.PercentUsed = &value
		}
		if reset.Valid {
			window.ResetAt = reset.Time
		}
		if observed.Valid {
			window.ObservedAt = observed.Time
		}
		report.UsageWindows = append(report.UsageWindows, window)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		rows.Close()
		return claimstore.ProviderReservation{}, err
	}
	if err := rows.Close(); err != nil {
		return claimstore.ProviderReservation{}, mapError(ctx, err)
	}
	if err := providercapacity.Validate([]providercapacity.Report{report}); err != nil || !report.Supports(row.Reservation.Requirement) {
		return claimstore.ProviderReservation{}, corestore.ErrInvalidPage
	}
	row.Reservation.Report = report
	return row, nil
}

func (s *session) ProviderReservationUsage(ctx context.Context, report providercapacity.Report, runnerID string, now time.Time) (int, error) {
	if runnerID == "" || report.Provider == "" || report.AccountAlias == "" || now.IsZero() {
		return 0, claimstore.ErrInvalidCandidateFact
	}
	var used int
	err := s.db.QueryRowContext(ctx, `SELECT count(*)
		FROM provider_reservations p
		JOIN leases l ON l.organization_id=p.organization_id AND l.lease_id=p.lease_id
		JOIN runner_lease_assignments lr ON lr.organization_id=l.organization_id AND lr.lease_id=l.lease_id
		WHERE p.organization_id=$1 AND p.provider=$2 AND l.released_at IS NULL AND l.expires_at>$3
		AND (($4<>'' AND p.shared_account_alias=$4) OR ($4='' AND p.shared_account_alias='' AND lr.runner_id=$5 AND p.account_alias=$6))`, s.organization, report.Provider, now, report.SharedAccountAlias, runnerID, report.AccountAlias).Scan(&used)
	return used, mapError(ctx, err)
}

func (s *session) CreateProviderReservation(ctx context.Context, row claimstore.ProviderReservation) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	report := row.Reservation.Report
	if row.LeaseID == "" || row.Reservation.Reason == "" || len(row.Reservation.Reason) > 1024 || row.Reservation.Validate() != nil || !report.Supports(row.Reservation.Requirement) || providercapacity.Validate([]providercapacity.Report{report}) != nil {
		return claimstore.ErrInvalidCandidateFact
	}
	var resetAt any
	if !report.ResetAt.IsZero() {
		resetAt = report.ResetAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO provider_reservations
		(organization_id,lease_id,requirement_role,requirement_backend,requirement_model,provider,backend,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, s.organization, row.LeaseID, row.Reservation.Role, row.Reservation.Backend, row.Reservation.Model, report.Provider, report.Backend, report.AccountAlias, report.SharedAccountAlias, report.Billing, report.MaxConcurrent, report.Availability, report.ObservedAt, resetAt, row.Reservation.Reason)
	if err != nil {
		return mapError(ctx, err)
	}
	details := make(map[string]providercapacity.ModelDetail, len(report.ModelDetails))
	for _, detail := range report.ModelDetails {
		details[detail.ID] = detail
	}
	for position, model := range report.Models {
		detail, hasDetail := details[model]
		if _, err := s.db.ExecContext(ctx, `INSERT INTO provider_reservation_models
			(organization_id,lease_id,position,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, s.organization, row.LeaseID, position, model, hasDetail, detail.Label, detail.Provider, detail.Default, detail.DefaultReasoningEffort, detail.Legacy); err != nil {
			return mapError(ctx, err)
		}
		for effortPosition, effort := range detail.ReasoningEfforts {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO provider_reservation_model_efforts
				(organization_id,lease_id,model_id,position,effort) VALUES($1,$2,$3,$4,$5)`, s.organization, row.LeaseID, model, effortPosition, effort); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	for position, window := range report.UsageWindows {
		var percent, reset, observed any
		if window.PercentUsed != nil {
			percent = *window.PercentUsed
		}
		if !window.ResetAt.IsZero() {
			reset = window.ResetAt
		}
		if !window.ObservedAt.IsZero() {
			observed = window.ObservedAt
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO provider_reservation_usage_windows
			(organization_id,lease_id,position,kind,percent_used,reset_at,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, s.organization, row.LeaseID, position, window.Kind, percent, reset, observed); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
