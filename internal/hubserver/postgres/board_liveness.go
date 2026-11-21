package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) BoardCardLiveness(ctx context.Context, project tracker.ProjectID, items []tracker.NativeWorkItemID, now time.Time) (map[tracker.NativeWorkItemID]viewstore.BoardLiveness, error) {
	query := "WITH " + boardviewsql.ItemLiveness("$1", "$2::text", "$3", "$4", "$5") + `
 SELECT work_item_id,coalesce(card_attempt_id,''),coalesce(card_execution_id,''),lease_expires_at,attempt_until,execution_until
 FROM board_liveness`
	rows, err := s.db.Query(ctx, query, s.organization, project, now, int64(runnerauth.HeartbeatTimeout/time.Second), items)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make(map[tracker.NativeWorkItemID]viewstore.BoardLiveness, len(items))
	for rows.Next() {
		var item tracker.NativeWorkItemID
		var value viewstore.BoardLiveness
		if err := rows.Scan(&item, &value.AttemptID, &value.ExecutionID, &value.LeaseExpiresAt, &value.AttemptUntil, &value.ExecutionUntil); err != nil {
			return nil, mapError(ctx, err)
		}
		result[item] = value
	}
	return result, mapError(ctx, rows.Err())
}
