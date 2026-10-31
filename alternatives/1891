package hubserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	nativeShippedState    = "Done"
	nativeCancelledState  = "Cancelled"
	nativeBoardTimeLayout = "2006-01-02T15:04:05.000000000Z"
)

type nativeShippedIssue struct {
	ID       string
	ClosedAt time.Time
}

type nativeClosedPopulation struct {
	Shipped     []nativeShippedIssue
	Cancelled   int
	CancelledAt []time.Time
}

func (p nativeClosedPopulation) shippedIDs() []string {
	ids := make([]string, 0, len(p.Shipped))
	for _, issue := range p.Shipped {
		ids = append(ids, issue.ID)
	}
	return ids
}

func nativeBoardTime(at time.Time) string {
	return at.UTC().Format(nativeBoardTimeLayout)
}

// readNativeClosedPopulation is the one definition of shipped work: issues
// whose current state is Done and whose closed_at falls in [from, to),
// archived or not. Cancelled closes in the same window are counted apart.
// Callers choose the window: the board counter rolls back from now, while
// Reports ends at the last complete hour. A zero from reads all history.
// filter narrows the population with a predicate over native_board_items b.
func readNativeClosedPopulation(ctx context.Context, q nativeQueryer, scope nativeScope, filter string, filterArgs []any, from, to time.Time) (nativeClosedPopulation, error) {
	out := nativeClosedPopulation{Shipped: []nativeShippedIssue{}}
	statement := `SELECT b.work_item_id,b.state,b.closed_at FROM native_board_items b
WHERE b.organization_id=? AND b.project_id=? AND b.archived IN (0,1) AND b.terminal=1 AND b.closed_at<>'' AND b.closed_at>=? AND b.closed_at<? AND b.state IN (?,?)`
	args := []any{scope.organization, scope.project, nativeBoardTime(from), nativeBoardTime(to), nativeShippedState, nativeCancelledState}
	if filter != "" {
		statement += " AND " + filter
		args = append(args, filterArgs...)
	}
	issues, issueArgs := reportsIssueClause(ctx, "b.work_item_id")
	statement += issues
	args = append(args, issueArgs...)
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return out, fmt.Errorf("read closed population: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state, closed string
		if err := rows.Scan(&id, &state, &closed); err != nil {
			return out, fmt.Errorf("read closed population: %w", err)
		}
		at, err := time.Parse(nativeBoardTimeLayout, closed)
		if err != nil {
			return out, fmt.Errorf("decode closed population time: %w", err)
		}
		if state == nativeCancelledState {
			out.Cancelled++
			out.CancelledAt = append(out.CancelledAt, at)
			continue
		}
		out.Shipped = append(out.Shipped, nativeShippedIssue{ID: id, ClosedAt: at})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, fmt.Errorf("read closed population: %w", err)
	}
	return out, nil
}

var nativeShippedWindowSpans = []struct {
	name string
	span time.Duration
}{{"1h", time.Hour}, {nativeDefaultShippedWindow, 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}}

const nativeDefaultShippedWindow = "24h"

// nativeShippedWindow names the board counter window. Windows the board no
// longer offers fall back to the default so saved views keep working.
func nativeShippedWindow(value string) (string, error) {
	switch value {
	case "1h", "7d":
		return value, nil
	case "", nativeDefaultShippedWindow, "48h", "14d", "all":
		return nativeDefaultShippedWindow, nil
	default:
		return "", nativePublicInvalid("completed_window supports 1h,24h,7d")
	}
}

// readNativeShippedWindows counts shipped and cancelled work for every board
// counter window ending at now, from one read of the widest window.
func readNativeShippedWindows(ctx context.Context, q nativeQueryer, scope nativeScope, filter string, filterArgs []any, now time.Time) ([]tracker.NativeShippedWindow, error) {
	widest := nativeShippedWindowSpans[len(nativeShippedWindowSpans)-1].span
	population, err := readNativeClosedPopulation(ctx, q, scope, filter, filterArgs, now.Add(-widest), now)
	if err != nil {
		return nil, err
	}
	windows := make([]tracker.NativeShippedWindow, 0, len(nativeShippedWindowSpans))
	for _, span := range nativeShippedWindowSpans {
		from := now.Add(-span.span)
		window := tracker.NativeShippedWindow{Window: span.name}
		for _, issue := range population.Shipped {
			if !issue.ClosedAt.Before(from) {
				window.Shipped++
			}
		}
		for _, at := range population.CancelledAt {
			if !at.Before(from) {
				window.Cancelled++
			}
		}
		windows = append(windows, window)
	}
	return windows, nil
}

func applyNativeShippedWindows(summary *tracker.NativeWorkSummary, windows []tracker.NativeShippedWindow, selected string) {
	summary.ShippedWindows = windows
	for _, window := range windows {
		if window.Window == selected {
			summary.Completed, summary.Cancelled = window.Shipped, window.Cancelled
		}
	}
}

// readNativeShippedSpend returns the lifetime spend of the shipped issues up
// to `to`, in total and per issue; an issue with no usage spent nothing.
func readNativeShippedSpend(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, to time.Time) (float64, map[string]float64, error) {
	var cost float64
	perIssue := make(map[string]float64, len(ids))
	for _, id := range ids {
		perIssue[id] = 0
	}
	err := forShippedBatches(ids, func(batch sqlParameterRows) error {
		rows, err := querySQL(ctx, q, `SELECT a.work_item_id,coalesce(sum(u.cost_estimate),0) FROM sql_rows(?) w
CROSS JOIN native_attempts a ON a.organization_id=? AND a.project_id=? AND a.work_item_id=w.value
CROSS JOIN attempt_usage u ON u.attempt_id=a.id AND u.organization_id=? AND u.project_id=? AND +u.period<? GROUP BY a.work_item_id`,
			batch, scope.organization, scope.project, scope.organization, scope.project, to.UTC().Format(usagePeriodLayout))
		if err != nil {
			return fmt.Errorf("read shipped spend: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var part float64
			if err := rows.Scan(&id, &part); err != nil {
				return fmt.Errorf("read shipped spend: %w", err)
			}
			perIssue[id] += part
			cost += part
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read shipped spend: %w", err)
		}
		return nil
	})
	return cost, perIssue, err
}

func readProjectSpend(ctx context.Context, q nativeQueryer, scope nativeScope, from, to time.Time) (float64, int, error) {
	var cost float64
	var rows int
	statement := `SELECT coalesce(sum(cost_estimate),0),count(*) FROM attempt_usage WHERE organization_id=? AND period>=? AND period<? AND project_id=?`
	args := []any{scope.organization, from.UTC().Format(usagePeriodLayout), to.UTC().Format(usagePeriodLayout), scope.project}
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return 0, 0, err
	}
	if scoped != "" {
		q, err = prepareReportsQuery(ctx, q, scope, "report_attempts")
		if err != nil {
			return 0, 0, err
		}
		statement += ` AND attempt_id IN (SELECT a.id FROM report_attempts a WHERE a.organization_id=? AND a.project_id=?` + scoped + `)`
		args = append(append(args, scope.organization, scope.project), scopedArgs...)
	}
	if err := q.QueryRowContext(ctx, statement, args...).Scan(&cost, &rows); err != nil {
		return 0, 0, fmt.Errorf("read project spend: %w", err)
	}
	return cost, rows, nil
}
