package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeBoardRank(ctx context.Context, q nativeQueryer, project, state string) (int, error) {
	var rank sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT (SELECT board_rank FROM workflow_states WHERE project_id=p.id AND source_name=?) FROM projects p WHERE p.id=?`, state, project).Scan(&rank); err != nil {
		return 0, err
	}
	if rank.Valid {
		return int(rank.Int64), nil
	}
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT states_json FROM projects WHERE id=?", project).Scan(&raw); err != nil {
		return 0, err
	}
	var states []tracker.NativeState
	if err := json.Unmarshal([]byte(raw), &states); err != nil {
		return 0, err
	}
	for i, s := range states {
		if s.Name == state {
			return i, nil
		}
	}
	return 2147483647, nil
}
