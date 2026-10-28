package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errRunnerIdentityChild = errors.New("runner identity child row")

type runnerIdentityBatchItem struct {
	runner  runnerauth.Runner
	reports []providercapacity.Report
	checks  string
}

type runnerIdentityBatchRow struct {
	rows                  *sql.Rows
	ordinal               int
	kind, runner, key     string
	text, reset, observed sql.NullString
	integer               sql.NullInt64
	real                  sql.NullFloat64
}

func (row *runnerIdentityBatchRow) Scan(destinations ...any) error {
	values := append([]any{&row.ordinal}, destinations...)
	values = append(values, &row.kind, &row.runner, &row.key, &row.text, &row.integer, &row.real, &row.reset, &row.observed)
	if err := row.rows.Scan(values...); err != nil {
		return err
	}
	if row.kind != "" {
		return errRunnerIdentityChild
	}
	return nil
}

func readRunnerIdentityBatch(ctx context.Context, q nativeQueryer, statement string, args []any, now time.Time, checks bool) ([]runnerIdentityBatchItem, error) {
	query := `WITH selected AS MATERIALIZED (` + statement + `), numbered AS (SELECT row_number() OVER() AS ordinal,s.* FROM selected s), facts AS (
SELECT 'project' AS kind,r.id AS runner_id,g.project_id AS key,NULL AS text,NULL AS integer,NULL AS real,NULL AS reset,NULL AS observed FROM selected r JOIN token_grants g ON g.token_id=r.token_id
UNION ALL SELECT 'capacity',r.id,c.project_id,NULL,c.capacity_limit,NULL,NULL,NULL FROM selected r JOIN runner_project_capacity_limits c ON c.runner_id=r.id AND c.organization_id=r.organization_id
UNION ALL SELECT 'billing',r.id,b.backend,b.billing,NULL,NULL,NULL,NULL FROM selected r JOIN runner_provider_billing b ON b.runner_id=r.id
UNION ALL SELECT 'usage',r.id,w.backend,w.kind,NULL,w.percent_used,w.reset_at,w.observed_at FROM selected r JOIN runner_provider_usage_windows w ON w.runner_id=r.id
)
SELECT r.*,'' AS kind,'' AS runner_id,'' AS key,NULL AS text,NULL AS integer,NULL AS real,NULL AS reset,NULL AS observed FROM numbered r
UNION ALL SELECT r.*,f.kind,f.runner_id,f.key,f.text,f.integer,f.real,f.reset,f.observed FROM numbered r JOIN facts f ON f.runner_id=r.id
ORDER BY ordinal,kind,key,text`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []runnerIdentityBatchItem{}
	indices := make(map[string]int)
	billings := make(map[string]map[string]string)
	windows := make(map[string]map[string][]providercapacity.UsageWindow)
	for rows.Next() {
		row := runnerIdentityBatchRow{rows: rows}
		var item runnerIdentityBatchItem
		var extra []any
		if checks {
			extra = append(extra, &item.checks)
		}
		item.runner, item.reports, _, err = scanRunnerIdentity(&row, func() time.Time { return now }, extra...)
		if err == nil {
			item.runner.ProjectIDs = []tracker.ProjectID{}
			item.runner.ProjectCapacityLimits = make(map[tracker.ProjectID]int)
			indices[item.runner.RunnerID] = len(items)
			items = append(items, item)
			continue
		}
		if !errors.Is(err, errRunnerIdentityChild) {
			return nil, err
		}
		index, ok := indices[row.runner]
		if !ok {
			return nil, errors.New("runner child precedes its identity")
		}
		runner := &items[index].runner
		switch row.kind {
		case "project":
			runner.ProjectIDs = append(runner.ProjectIDs, tracker.ProjectID(row.key))
		case "capacity":
			runner.ProjectCapacityLimits[tracker.ProjectID(row.key)] = int(row.integer.Int64)
		case "billing":
			if billings[row.runner] == nil {
				billings[row.runner] = make(map[string]string)
			}
			billings[row.runner][row.key] = row.text.String
		case "usage":
			window := providercapacity.UsageWindow{Kind: row.text.String}
			if row.real.Valid {
				window.PercentUsed = &row.real.Float64
			}
			if row.reset.Valid {
				window.ResetAt, err = parseTimeValue(row.reset.String)
				if err != nil {
					return nil, err
				}
			}
			if row.observed.Valid {
				window.ObservedAt, err = parseTimeValue(row.observed.String)
				if err != nil {
					return nil, err
				}
			}
			if windows[row.runner] == nil {
				windows[row.runner] = make(map[string][]providercapacity.UsageWindow)
			}
			windows[row.runner][row.key] = append(windows[row.runner][row.key], window)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		item := &items[i]
		item.runner.Routing = item.runner.Normalized()
		for j := range item.reports {
			report := &item.reports[j]
			report.Billing = (providercapacity.Report{Billing: billings[item.runner.RunnerID][report.Backend]}).BillingBasis()
			report.UsageWindows = append(report.UsageWindows, windows[item.runner.RunnerID][report.Backend]...)
		}
	}
	return items, nil
}
