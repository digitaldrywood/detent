package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

var ErrBillingUsageUnavailable = errors.New("billing usage requires the selected tenant transaction")

type hostedConsumptionRecord struct {
	usage       map[string]int64
	unavailable []string
}

func (d *database) readHostedConsumptionRecord(ctx context.Context, reader tenantstore.Reader, now time.Time, metrics ...string) (hostedConsumptionRecord, error) {
	result := hostedConsumptionRecord{usage: map[string]int64{}}
	plans := d.plans()
	if plans != nil && plans.WindowSeconds <= 0 {
		return result, ErrBillingUsageUnavailable
	}
	if len(metrics) == 0 {
		metrics = append([]string{"events_total", "usage_windows"}, hostedAllowanceNames()...)
	}
	selected := map[string]bool{}
	for _, metric := range metrics {
		selected[metric] = true
	}
	windows := selected["usage_windows"]
	windowMetrics := []string{"api_mutations", "ingested_events", "heartbeats", "http_requests", "http_duration_microseconds", "http_response_bytes", "http_request_bytes", "http_request_bytes_known", "artifact_bytes", "artifact_upload_bytes", "artifact_retention_seconds", "relay_bytes"}
	for _, metric := range windowMetrics {
		windows = windows || selected[metric]
	}
	byMetric := map[string]int64{}
	if windows && plans != nil {
		start := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
		names := map[string]bool{}
		for _, metric := range windowMetrics {
			if selected["usage_windows"] || selected[metric] {
				names[metric] = true
			}
		}
		if selected["usage_windows"] {
			for _, metric := range hostedAllowanceNames() {
				if metric != "artifact_retained_bytes" && metric != "artifact_reserved_bytes" {
					names[metric] = true
				}
			}
		}
		for _, metric := range slices.Sorted(maps.Keys(names)) {
			amount, err := reader.BillingUsage(ctx, start, metric)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return result, err
			}
			byMetric[metric] = amount
		}
	}
	populations, ok := reader.(billingstore.PopulationReader)
	if !ok {
		return result, ErrBillingUsageUnavailable
	}
	var artifactUsage *artifact.Usage
	for _, metric := range slices.Sorted(maps.Keys(selected)) {
		known := true
		var used int64
		var err error
		switch metric {
		case "usage_windows":
			continue
		case "events_total", "history_records", "unarchived_issues", "members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work":
			if plans == nil && metric == "connected_runners" {
				known = false
				break
			}
			connected := time.Duration(0)
			if plans != nil {
				connected = time.Duration(plans.ConnectedSeconds) * time.Second
			}
			used, known, err = populations.BillingPopulation(ctx, metric, now, connected)
		case "collaboration_bytes":
			used, err = reader.CollaborationBytes(ctx)
			if errors.Is(err, corestore.ErrNotFound) {
				known = false
				err = nil
			}
		case "artifact_retained_bytes", "artifact_reserved_bytes":
			if artifactUsage == nil {
				value, readErr := reader.HostedArtifactUsage(ctx)
				if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
					return result, readErr
				}
				artifactUsage = &value
			}
			known = artifactUsage.ObservedAt.After(time.Unix(0, 0))
			used = artifactUsage.RetainedBytes
			if metric == "artifact_reserved_bytes" {
				used = artifactUsage.ReservedBytes
			}
		default:
			if !slices.Contains(windowMetrics, metric) {
				return result, ErrBillingUsageUnavailable
			}
			if plans == nil {
				known = false
				break
			}
			used = byMetric[metric]
		}
		if err != nil {
			return result, err
		}
		if selected["usage_windows"] && metric != "artifact_retained_bytes" && metric != "artifact_reserved_bytes" {
			if override, exists := byMetric[metric]; exists {
				used, known = override, true
			}
		}
		if known {
			result.usage[metric] = used
		} else {
			result.unavailable = append(result.unavailable, metric)
		}
	}
	if selected["usage_windows"] {
		for metric, amount := range byMetric {
			if metric != "artifact_retained_bytes" && metric != "artifact_reserved_bytes" {
				result.usage[metric] = amount
			}
		}
	}
	return result, nil
}

func (d *database) hostedConsumption(ctx context.Context, query nativeQueryer, now time.Time, metrics ...string) (map[string]int64, error) {
	runtimePlans := d.plans()
	result := make(map[string]int64)
	if runtimePlans == nil {
		return result, nil
	}
	if d.billingRepository != nil {
		var selected hostedConsumptionRecord
		err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
			var err error
			selected, err = d.readHostedConsumptionRecord(ctx, reader, now, metrics...)
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(selected.unavailable) != 0 {
			return selected.usage, ErrBillingUsageUnavailable
		}
		return selected.usage, nil
	}
	queries := map[string]string{
		"members":             "SELECT count(*) FROM hosted_members WHERE active = 1",
		"projects":            "SELECT count(*) FROM projects WHERE deleted_at IS NULL",
		"unarchived_issues":   "SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.organization_id = ? AND p.deleted_at IS NULL AND p.profile = 'native' AND i.archived = 0 AND coalesce(ws.terminal, 0) = 0",
		"repositories":        "SELECT count(*) FROM repositories",
		"registered_runners":  `SELECT (SELECT count(*) FROM runner_identities r JOIN api_tokens t ON t.id = r.token_id WHERE t.revoked_at IS NULL) + (SELECT count(*) FROM machines m JOIN api_tokens t ON t.id = m.token_id WHERE t.revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM runner_identities r WHERE r.machine_id = m.id))`,
		"history_records":     "SELECT value FROM hosted_usage_counters WHERE name = 'history_records'",
		"events_total":        "SELECT value FROM hosted_usage_counters WHERE name = 'events_total'",
		"collaboration_bytes": "SELECT value FROM hosted_usage_counters WHERE name = 'collaboration_bytes'",
	}
	for name, statement := range queries {
		if len(metrics) > 0 && !slices.Contains(metrics, name) {
			continue
		}
		var count int64
		var args []any
		if name == "unarchived_issues" {
			args = append(args, d.hostedOrganization)
		}
		if err := query.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
			return nil, err
		}
		result[name] = count
	}
	if len(metrics) == 0 || slices.Contains(metrics, "artifact_retained_bytes") || slices.Contains(metrics, "artifact_reserved_bytes") {
		return nil, ErrBillingUsageUnavailable
	}
	for _, sample := range []struct {
		name, statement string
		after           time.Time
	}{
		{"concurrent_work", "SELECT expires_at FROM leases WHERE released_at IS NULL", now},
		{"connected_runners", `SELECT r.last_heartbeat_at FROM runner_identities r JOIN api_tokens t ON t.id = r.token_id WHERE t.revoked_at IS NULL UNION ALL SELECT m.last_heartbeat_at FROM machines m JOIN api_tokens t ON t.id = m.token_id WHERE t.revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM runner_identities r WHERE r.machine_id = m.id)`, now.Add(-time.Duration(runtimePlans.ConnectedSeconds) * time.Second)},
	} {
		if len(metrics) > 0 && !slices.Contains(metrics, sample.name) {
			continue
		}
		count, err := countHostedAfter(ctx, query, sample.statement, sample.after)
		if err != nil {
			return nil, err
		}
		result[sample.name] = count
	}
	if len(metrics) == 0 || slices.Contains(metrics, "members") {
		var reserved int64
		if err := query.QueryRowContext(ctx, "SELECT count(*) FROM hosted_member_reservations WHERE expires_at > ?", now.Unix()).Scan(&reserved); err != nil {
			return nil, err
		}
		result["members"] += reserved
	}
	if len(metrics) > 0 && !slices.Contains(metrics, "usage_windows") {
		return result, nil
	}
	window := now.Unix() / runtimePlans.WindowSeconds * runtimePlans.WindowSeconds
	rows, err := query.QueryContext(ctx, "SELECT metric,amount FROM hosted_usage_windows WHERE window_start = ?", window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var metric string
		var amount int64
		if err := rows.Scan(&metric, &amount); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		result[metric] = amount
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func countHostedAfter(ctx context.Context, query nativeQueryer, statement string, after time.Time, args ...any) (int64, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	var count int64
	defer rows.Close()
	for rows.Next() {
		var stamp string
		if err := rows.Scan(&stamp); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		parsed, err := parseTimeValue(stamp)
		if err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		if parsed.After(after) {
			count++
		}
	}
	return count, errors.Join(rows.Err(), rows.Close())
}

func (d *database) recordHostedUsage(ctx context.Context, tx *sql.Tx, now time.Time, metric string, amount int64) error {
	runtimePlans := d.plans()
	if runtimePlans == nil || amount == 0 {
		return nil
	}
	if d.billingRepository != nil {
		return ErrBillingUsageUnavailable
	}
	window := now.Unix() / runtimePlans.WindowSeconds * runtimePlans.WindowSeconds
	if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_usage_windows WHERE window_start <= ?", window-runtimePlans.RetentionWindows*runtimePlans.WindowSeconds); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO hosted_usage_windows(window_start,metric,amount) VALUES(?,?,?) ON CONFLICT(window_start,metric) DO UPDATE SET amount = min(amount+excluded.amount,1125899906842624)`, window, metric, amount)
	return err
}

func (d *database) checkHostedGrowth(ctx context.Context, tx *sql.Tx, before map[string]int64, now time.Time, completion bool, metrics ...string) error {
	if d.plans() == nil {
		return nil
	}
	after, err := d.hostedConsumption(ctx, tx, now, metrics...)
	if err != nil {
		return err
	}
	return d.checkHostedConsumptionGrowth(ctx, tx, before, after, now, completion)
}

func (d *database) checkHostedConsumptionGrowth(ctx context.Context, tx *sql.Tx, before, after map[string]int64, now time.Time, completion bool) error {
	if err := d.validateHostedConsumptionGrowth(ctx, tx, before, after, now, completion); err != nil {
		return err
	}
	if ctx.Value(runnerRequestBudgetContext{}) != true {
		if err := d.recordHostedUsage(ctx, tx, now, "api_mutations", 1); err != nil {
			return err
		}
	}
	return d.recordHostedUsage(ctx, tx, now, "ingested_events", max(int64(0), after["events_total"]-before["events_total"]))
}

func (d *database) validateHostedConsumptionGrowth(ctx context.Context, query nativeQueryer, before, after map[string]int64, now time.Time, completion bool) error {
	entitlement, err := d.hostedEntitlement(ctx, query, now)
	if err != nil {
		return err
	}
	for _, name := range []string{"unarchived_issues", "projects", "repositories", "registered_runners", "connected_runners", "collaboration_bytes", "history_records"} {
		if completion && (name == "collaboration_bytes" || name == "history_records" || name == "connected_runners") {
			continue
		}
		limit, limited := entitlement.Allowances[name]
		if limited && after[name] > before[name] && after[name] > limit {
			return &hostedLimitError{Resource: name, Allowance: entitlement.Allowances[name], Consumption: before[name]}
		}
	}
	for _, sample := range []struct {
		name   string
		amount int64
	}{
		{"api_mutations", 1}, {"ingested_events", max(int64(0), after["events_total"]-before["events_total"])},
	} {
		if sample.name == "api_mutations" && ctx.Value(runnerRequestBudgetContext{}) == true {
			continue
		}
		if !completion && sample.amount > 0 && sample.amount > entitlement.Allowances[sample.name]-after[sample.name] {
			return &hostedLimitError{Resource: sample.name, Allowance: entitlement.Allowances[sample.name], Consumption: after[sample.name]}
		}
	}
	return nil
}

func (d *database) checkHostedClaim(ctx context.Context, tx *sql.Tx, now time.Time) error {
	runtimePlans := d.plans()
	if runtimePlans == nil {
		return nil
	}
	entitlement, err := d.hostedEntitlement(ctx, tx, now)
	if err != nil {
		return err
	}
	if !slices.Contains(entitlement.Features, "native_execution") {
		return &hostedLimitError{Resource: "native_execution"}
	}
	if limit, limited := entitlement.Allowances["concurrent_work"]; limited {
		usage, err := d.hostedConsumption(ctx, tx, now, "concurrent_work")
		if err != nil {
			return err
		}
		if usage["concurrent_work"] >= limit {
			return &nativeError{Code: "plan_capacity", Message: fmt.Sprintf("The plan's concurrent work limit is reached (%d of %d); work stays queued until a slot frees", usage["concurrent_work"], limit), status: http.StatusConflict, publicMessage: true}
		}
	}
	if ctx.Value(runnerRequestBudgetContext{}) == true {
		return nil
	}
	usage, err := d.hostedConsumption(ctx, tx, now, "usage_windows")
	if err != nil {
		return err
	}
	used := usage["api_mutations"]
	if used >= entitlement.Allowances["api_mutations"] {
		return &hostedLimitError{Resource: "api_mutations", Allowance: entitlement.Allowances["api_mutations"], Consumption: used}
	}
	return d.recordHostedUsage(ctx, tx, now, "api_mutations", 1)
}
