package hubserver

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	Shipped   []nativeShippedIssue
	Cancelled int
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
		if state == nativeCancelledState {
			out.Cancelled++
			continue
		}
		at, err := time.Parse(nativeBoardTimeLayout, closed)
		if err != nil {
			return out, fmt.Errorf("decode closed population time: %w", err)
		}
		out.Shipped = append(out.Shipped, nativeShippedIssue{ID: id, ClosedAt: at})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, fmt.Errorf("read closed population: %w", err)
	}
	return out, nil
}

func readNativeShippedSpend(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, to time.Time) (float64, error) {
	var cost float64
	err := forShippedBatches(ids, func(batch string) error {
		var part float64
		if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u
WHERE u.organization_id=? AND u.period<? AND u.project_id=?
AND u.attempt_id IN (SELECT a.id FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id IN (SELECT value FROM json_each(?)))`,
			scope.organization, to.UTC().Format(usagePeriodLayout), scope.project, scope.organization, scope.project, batch).Scan(&part); err != nil {
			return fmt.Errorf("read shipped spend: %w", err)
		}
		cost += part
		return nil
	})
	return cost, err
}

func readProjectSpend(ctx context.Context, q nativeQueryer, scope nativeScope, from, to time.Time) (float64, int, error) {
	var cost float64
	var rows int
	if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(cost_estimate),0),count(*) FROM attempt_usage WHERE organization_id=? AND period>=? AND period<? AND project_id=?`,
		scope.organization, from.UTC().Format(usagePeriodLayout), to.UTC().Format(usagePeriodLayout), scope.project).Scan(&cost, &rows); err != nil {
		return 0, 0, fmt.Errorf("read project spend: %w", err)
	}
	return cost, rows, nil
}
