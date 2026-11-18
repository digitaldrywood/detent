package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func readCostObservations(ctx context.Context, query hubapi.NativeQueryer, organization string, projects []string, window usageWindow) ([]attributedCostObservation, error) {
	encoded, err := hubapi.SQLList(projects)
	if err != nil {
		return nil, err
	}
	rows, err := hubapi.QuerySQL(ctx, query, `SELECT u.project_id,u.observation_json,u.received_at,u.voided FROM usage_cost_observations u
WHERE u.organization_id=? AND u.period_start<? AND u.period_end>? AND (? OR u.project_id IN (SELECT value FROM sql_rows(?)))
AND (u.voided=0 OR u.voided IS NULL)
AND NOT EXISTS (SELECT 1 FROM usage_cost_observations newer WHERE newer.organization_id=u.organization_id AND newer.provider=u.provider AND newer.provider_account=u.provider_account AND newer.source_id=u.source_id AND newer.period_start=u.period_start AND newer.period_end=u.period_end AND newer.revision>u.revision)
ORDER BY u.project_id,u.bucket,u.provider,u.provider_account,u.resource_id,u.metric,u.period_start,u.source_id`, organization, window.To.Format(costTimeLayout), window.From.Format(costTimeLayout), projects == nil, encoded)
	if err != nil {
		return nil, fmt.Errorf("read monthly cost observations: %w", err)
	}
	defer rows.Close()
	result := []attributedCostObservation{}
	for rows.Next() {
		var entry attributedCostObservation
		var raw, received string
		var voided sql.NullBool
		if err := rows.Scan(&entry.ProjectID, &raw, &received, &voided); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &entry.Observation); err != nil {
			return nil, fmt.Errorf("decode monthly cost observation: %w", err)
		}
		if !voided.Valid && entry.Voided {
			continue
		}
		entry.ReceivedAt, err = time.Parse(costTimeLayout, received)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}
