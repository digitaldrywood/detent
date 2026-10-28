package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLocalAttemptMatches(ctx context.Context, q nativeQueryer, scope nativeScope, item string, local int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND hot_projection_version=1 AND local_attempt_id=?
UNION SELECT subject_id FROM native_archive_sections WHERE organization_id=? AND project_id=? AND work_item_id=? AND kind='run' AND local_attempt_id=? LIMIT 2`, scope.organization, scope.project, item, local, scope.organization, scope.project, item, local)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	matches := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		matches = append(matches, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(matches) == 2 {
		return matches, nil
	}
	rows, err = q.QueryContext(ctx, `SELECT id,data_json FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND hot_projection_version=0`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		if data.Runtime != nil && data.Runtime.LocalAttemptID == local {
			duplicate := false
			for _, match := range matches {
				if match == id {
					duplicate = true
				}
			}
			if !duplicate {
				matches = append(matches, id)
			}
			if len(matches) == 2 {
				return matches, nil
			}
		}
	}
	return matches, rows.Err()
}
