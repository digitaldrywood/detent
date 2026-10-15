package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func writeProviderUsage(ctx context.Context, tx *sql.Tx, runner string, reports []providercapacity.Report) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM runner_provider_usage_windows WHERE runner_id = ?", runner); err != nil {
		return err
	}
	for _, report := range reports {
		for _, window := range report.UsageWindows {
			var observed, reset *string
			if !window.ObservedAt.IsZero() {
				value := window.ObservedAt.UTC().Format(time.RFC3339Nano)
				observed = &value
			}
			if !window.ResetAt.IsZero() {
				value := window.ResetAt.UTC().Format(time.RFC3339Nano)
				reset = &value
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO runner_provider_usage_windows
(runner_id, backend, kind, percent_used, reset_at, observed_at) VALUES (?, ?, ?, ?, ?, ?)`, runner, report.Backend, window.Kind, window.PercentUsed, reset, observed); err != nil {
				return err
			}
		}
	}
	return nil
}

func readProviderUsage(ctx context.Context, query nativeQueryer, runner string) (map[string][]providercapacity.UsageWindow, error) {
	result := make(map[string][]providercapacity.UsageWindow)
	rows, err := query.QueryContext(ctx, `SELECT backend, kind, percent_used, reset_at, observed_at
 FROM runner_provider_usage_windows WHERE runner_id = ? ORDER BY backend, kind`, runner)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var backend string
		var window providercapacity.UsageWindow
		var percent sql.NullFloat64
		var reset, observed sql.NullString
		if err := rows.Scan(&backend, &window.Kind, &percent, &reset, &observed); err != nil {
			return result, err
		}
		if percent.Valid {
			window.PercentUsed = &percent.Float64
		}
		if reset.Valid {
			window.ResetAt, err = parseTimeValue(reset.String)
			if err != nil {
				return result, err
			}
		}
		if observed.Valid {
			window.ObservedAt, err = parseTimeValue(observed.String)
			if err != nil {
				return result, err
			}
		}
		result[backend] = append(result[backend], window)
	}
	return result, rows.Err()
}
