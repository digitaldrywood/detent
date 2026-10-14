package hubserver

import (
	"context"
	"encoding/json"
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

func nativeShippedBoardFilter(window bool) string {
	filter := " AND b.terminal=1 AND b.state=?"
	if window {
		filter += " AND b.closed_at<>'' AND b.closed_at>=? AND b.closed_at<?"
	}
	return filter
}

func nativeShippedBoardArgs(from, to time.Time) []any {
	return []any{nativeShippedState, nativeBoardTime(from), nativeBoardTime(to)}
}

func readNativeClosedPopulation(ctx context.Context, q nativeQueryer, scope nativeScope, from, to time.Time) (nativeClosedPopulation, error) {
	out := nativeClosedPopulation{Shipped: []nativeShippedIssue{}}
	rows, err := q.QueryContext(ctx, `SELECT b.work_item_id,b.state,b.closed_at FROM native_board_items b
WHERE b.organization_id=? AND b.project_id=? AND b.archived IN (0,1) AND b.terminal=1 AND b.closed_at<>'' AND b.closed_at>=? AND b.closed_at<? AND b.state IN (?,?)`,
		scope.organization, scope.project, nativeBoardTime(from), nativeBoardTime(to), nativeShippedState, nativeCancelledState)
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

func readNativeShippedSpend(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, from, to time.Time) (float64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("encode shipped issues: %w", err)
	}
	var cost float64
	err = q.QueryRowContext(ctx, `SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u
WHERE u.organization_id=? AND u.period>=? AND u.period<? AND u.project_id=?
AND u.attempt_id IN (SELECT a.id FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id IN (SELECT value FROM json_each(?)))`,
		scope.organization, from.UTC().Format(usagePeriodLayout), to.UTC().Format(usagePeriodLayout), scope.project, scope.organization, scope.project, string(encoded)).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("read shipped spend: %w", err)
	}
	return cost, nil
}
