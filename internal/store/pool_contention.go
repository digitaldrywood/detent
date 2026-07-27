package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const poolCapacityWaitReason = "global_capacity_full"

type PoolContentionQuery struct {
	Since          time.Time
	ProjectClasses map[string]string
}

type CrossClassPoolContention struct {
	Pool         string
	WaitingClass string
	HoldingClass string
	WaitCount    int
}

type poolContentionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type poolCapacitySnapshot struct {
	Pool    string   `json:"pool"`
	Holders []string `json:"holders"`
}

func QueryCrossClassPoolContention(
	ctx context.Context,
	db poolContentionQueryer,
	query PoolContentionQuery,
) ([]CrossClassPoolContention, error) {
	if db == nil {
		return nil, errors.New("pool contention database is required")
	}
	if query.Since.IsZero() {
		return nil, errors.New("pool contention start time is required")
	}

	since := query.Since.UTC().Truncate(time.Second).Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
SELECT project_id, capacity_snapshot_json
FROM work_attempts
WHERE wait_reason = ?
  AND COALESCE(NULLIF(heartbeat_at, ''), NULLIF(completed_at, ''), started_at) >= ?
UNION ALL
SELECT project_id, capacity_snapshot_json
FROM scheduler_decisions
WHERE wait_reason = ?
  AND decision_at >= ?`,
		poolCapacityWaitReason, since, poolCapacityWaitReason, since,
	)
	if err != nil {
		return nil, fmt.Errorf("querying pool capacity waits: %w", err)
	}
	defer rows.Close()

	counts := map[string]CrossClassPoolContention{}
	for rows.Next() {
		var projectID string
		var snapshotJSON string
		if err := rows.Scan(&projectID, &snapshotJSON); err != nil {
			return nil, fmt.Errorf("scanning pool capacity wait: %w", err)
		}
		waitingClass := strings.TrimSpace(query.ProjectClasses[strings.TrimSpace(projectID)])
		if waitingClass == "" {
			continue
		}
		var snapshot poolCapacitySnapshot
		if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
			return nil, fmt.Errorf("decode pool capacity snapshot for project %s: %w", projectID, err)
		}
		pool := strings.TrimSpace(snapshot.Pool)
		if pool == "" {
			pool = "default"
		}
		holderClasses := make(map[string]struct{}, len(snapshot.Holders))
		for _, holderID := range snapshot.Holders {
			holderClass := strings.TrimSpace(query.ProjectClasses[strings.TrimSpace(holderID)])
			if holderClass == "" || holderClass == waitingClass {
				continue
			}
			holderClasses[holderClass] = struct{}{}
		}
		for holdingClass := range holderClasses {
			key := strings.Join([]string{pool, waitingClass, holdingClass}, "\x00")
			contention := counts[key]
			contention.Pool = pool
			contention.WaitingClass = waitingClass
			contention.HoldingClass = holdingClass
			contention.WaitCount++
			counts[key] = contention
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pool capacity waits: %w", err)
	}

	contention := make([]CrossClassPoolContention, 0, len(counts))
	for _, item := range counts {
		contention = append(contention, item)
	}
	sort.Slice(contention, func(i, j int) bool {
		if contention[i].Pool != contention[j].Pool {
			return contention[i].Pool < contention[j].Pool
		}
		if contention[i].WaitingClass != contention[j].WaitingClass {
			return contention[i].WaitingClass < contention[j].WaitingClass
		}
		return contention[i].HoldingClass < contention[j].HoldingClass
	})
	return contention, nil
}
