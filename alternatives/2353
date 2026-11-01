package hubserver

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func writeProviderBilling(ctx context.Context, tx *sql.Tx, runner string, reports []providercapacity.Report) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM runner_provider_billing WHERE runner_id = ?", runner); err != nil {
		return err
	}
	for _, report := range reports {
		if _, err := tx.ExecContext(ctx, "INSERT INTO runner_provider_billing (runner_id, backend, billing) VALUES (?, ?, ?)", runner, report.Backend, report.BillingBasis()); err != nil {
			return err
		}
	}
	return nil
}

func readProviderBilling(ctx context.Context, query nativeQueryer, runner string) (map[string]string, error) {
	result := make(map[string]string)
	rows, err := query.QueryContext(ctx, "SELECT backend, billing FROM runner_provider_billing WHERE runner_id = ?", runner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var backend, billing string
		if err := rows.Scan(&backend, &billing); err != nil {
			return nil, err
		}
		result[backend] = billing
	}
	return result, rows.Err()
}
