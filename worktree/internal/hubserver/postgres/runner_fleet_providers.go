package postgres

import (
	"context"
	"database/sql"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func (s *session) fleetProviderRecords(ctx context.Context, records []runnerstore.FleetRecord, runnerIndices map[string]int, ids []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT runner_id,backend,provider,account_alias,shared_account_alias,billing,max_concurrent,availability,observed_at,reset_at FROM runner_provider_reports WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id, backend`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	reports := make(map[string][]providercapacity.Report)
	indices := make(map[[2]string]int)
	for rows.Next() {
		var runnerID string
		var report providercapacity.Report
		var resetAt sql.NullTime
		if err := rows.Scan(&runnerID, &report.Backend, &report.Provider, &report.AccountAlias, &report.SharedAccountAlias, &report.Billing, &report.MaxConcurrent, &report.Availability, &report.ObservedAt, &resetAt); err != nil {
			return mapError(ctx, err)
		}
		if resetAt.Valid {
			report.ResetAt = resetAt.Time
		}
		report.Models = []string{}
		report.ModelDetails = []providercapacity.ModelDetail{}
		report.UsageWindows = []providercapacity.UsageWindow{}
		indices[[2]string{runnerID, report.Backend}] = len(reports[runnerID])
		reports[runnerID] = append(reports[runnerID], report)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,backend,model_id,has_detail,label,model_provider,is_default,default_reasoning_effort,legacy FROM runner_provider_report_models WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id, backend,position`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var runnerID string
		var backend, id, label, provider, defaultEffort string
		var hasDetail, isDefault, legacy bool
		if err := rows.Scan(&runnerID, &backend, &id, &hasDetail, &label, &provider, &isDefault, &defaultEffort, &legacy); err != nil {
			return mapError(ctx, err)
		}
		index, ok := indices[[2]string{runnerID, backend}]
		if !ok {
			return corestore.ErrInvalidPage
		}
		report := &reports[runnerID][index]
		report.Models = append(report.Models, id)
		if hasDetail {
			report.ModelDetails = append(report.ModelDetails, providercapacity.ModelDetail{ID: id, Label: label, Provider: provider, Default: isDefault, DefaultReasoningEffort: defaultEffort, Legacy: legacy})
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,backend,model_id,position,effort FROM runner_provider_model_efforts WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id, backend,model_id,position`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var runnerID string
		var backend, model, effort string
		var position int
		if err := rows.Scan(&runnerID, &backend, &model, &position, &effort); err != nil {
			return mapError(ctx, err)
		}
		index, ok := indices[[2]string{runnerID, backend}]
		if !ok {
			return corestore.ErrInvalidPage
		}
		for i := range reports[runnerID][index].ModelDetails {
			if reports[runnerID][index].ModelDetails[i].ID == model {
				reports[runnerID][index].ModelDetails[i].ReasoningEfforts = append(reports[runnerID][index].ModelDetails[i].ReasoningEfforts, effort)
				break
			}
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT runner_id,backend,kind,percent_used,reset_at,observed_at FROM runner_provider_usage_windows WHERE organization_id=$1 AND runner_id=ANY($2) ORDER BY runner_id, backend,position`, s.organization, pq.Array(ids))
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var runnerID string
		var backend, kind string
		var percent sql.NullFloat64
		var resetAt, observedAt sql.NullTime
		if err := rows.Scan(&runnerID, &backend, &kind, &percent, &resetAt, &observedAt); err != nil {
			return mapError(ctx, err)
		}
		index, ok := indices[[2]string{runnerID, backend}]
		if !ok {
			return corestore.ErrInvalidPage
		}
		window := providercapacity.UsageWindow{Kind: kind}
		if percent.Valid {
			value := percent.Float64
			window.PercentUsed = &value
		}
		if resetAt.Valid {
			window.ResetAt = resetAt.Time
		}
		if observedAt.Valid {
			window.ObservedAt = observedAt.Time
		}
		reports[runnerID][index].UsageWindows = append(reports[runnerID][index].UsageWindows, window)
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	for id, report := range reports {
		if len(report) > 32 || providercapacity.Validate(report) != nil {
			return corestore.ErrInvalidPage
		}
		records[runnerIndices[id]].Reports = report
	}
	return nil
}
