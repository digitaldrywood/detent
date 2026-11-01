package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type heartbeatPosition struct {
	Sequence int64     `json:"sequence"`
	Observed time.Time `json:"observed_at"`
}

func readHeartbeatChanges(ctx context.Context, tx *sql.Tx, scope nativeScope, cursor nativeCursor, key []byte, reset bool, digest, policyID string, now time.Time) (*runnerauth.HeartbeatChanges, error) {
	changes := &runnerauth.HeartbeatChanges{Reset: reset, CapabilitiesDigest: digest, PolicyID: policyID, Items: []runnerauth.HeartbeatItem{}}
	var previous heartbeatPosition
	if !reset && (json.Unmarshal([]byte(cursor.After), &previous) != nil || previous.Sequence < 0 || previous.Observed.IsZero() || previous.Observed.After(now)) {
		changes.Reset = true
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM collaboration_events WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&sequence); err != nil {
		return nil, err
	}
	if !changes.Reset && previous.Sequence > 0 {
		var retained bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND rowid=?)", scope.organization, scope.project, previous.Sequence).Scan(&retained); err != nil {
			return nil, err
		}
		changes.Reset = !retained || previous.Sequence > sequence
	}
	if !changes.Reset {
		rows, err := tx.QueryContext(ctx, `SELECT native_id, revision, last_activity_at FROM issues
WHERE organization_id=? AND project_id=? AND (native_id IN (
 SELECT work_item_id FROM collaboration_events WHERE organization_id=? AND project_id=? AND rowid>? AND rowid<=?
) OR rtrim(last_activity_at,'Z')>rtrim(?,'Z')) ORDER BY native_id LIMIT 201`, scope.organization, scope.project, scope.organization, scope.project, previous.Sequence, sequence, formatHubTime(previous.Observed))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var item runnerauth.HeartbeatItem
			var activity string
			if err := rows.Scan(&item.ID, &item.Revision, &activity); err != nil {
				return nil, err
			}
			item.LastActivityAt, err = parseTimeValue(activity)
			if err != nil {
				return nil, err
			}
			changes.Items = append(changes.Items, item)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(changes.Items) > 200 {
			changes.Reset, changes.Items = true, []runnerauth.HeartbeatItem{}
		}
	}
	ids, err := nativeCandidateIDs(ctx, tx, claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), AvailableAt: now, Limit: 1}, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	changes.Claimable = len(ids) > 0
	position, err := json.Marshal(heartbeatPosition{Sequence: sequence, Observed: now})
	if err != nil {
		return nil, err
	}
	cursor.After, cursor.Expires = string(position), now.Add(time.Hour).Unix()
	changes.Cursor, err = encodeNativeCursor(cursor, key)
	return changes, err
}

func capabilitiesDigest(capabilities nativeCapabilitiesResponse) (string, error) {
	raw, err := json.Marshal(capabilities)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
