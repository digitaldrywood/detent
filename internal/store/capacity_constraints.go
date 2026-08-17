package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type CapacityConstraintReason string

const (
	CapacityConstraintSampleInterval                              = 5 * time.Minute
	CapacityConstraintPool               CapacityConstraintReason = "pool_waits"
	CapacityConstraintProject            CapacityConstraintReason = "project_capacity_full"
	CapacityConstraintLane               CapacityConstraintReason = "lane_capacity_full"
	CapacityConstraintWorkerHost         CapacityConstraintReason = "worker_host_capacity_full"
	CapacityConstraintRateWindow         CapacityConstraintReason = "provider_rate_window_backpressure"
	CapacityConstraintTrackerUnavailable CapacityConstraintReason = "tracker_unavailable"
	CapacityConstraintForgeUnavailable   CapacityConstraintReason = "forge_unavailable"
	CapacityConstraintCIUnavailable      CapacityConstraintReason = "ci_unavailable"
)

type CapacityConstraintQuery struct {
	Since          time.Time
	ProjectClasses map[string]string
}

type CapacityConstraintWait struct {
	ProjectID     string
	WorkloadClass string
	Pool          string
	Lane          string
	Reason        CapacityConstraintReason
	WaitCount     int
}

func QueryCapacityConstraintWaits(
	ctx context.Context,
	db poolContentionQueryer,
	query CapacityConstraintQuery,
) ([]CapacityConstraintWait, error) {
	if db == nil {
		return nil, errors.New("capacity constraint database is required")
	}
	if query.Since.IsZero() {
		return nil, errors.New("capacity constraint start time is required")
	}

	since := query.Since.UTC().Truncate(time.Second).Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
SELECT project_id, COALESCE(lane, ''), wait_reason, decision_at, capacity_snapshot_json
FROM scheduler_decisions
WHERE result = ?
  AND wait_reason IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  AND decision_at >= ?`,
		string(SchedulerDecisionResultSkipped),
		poolCapacityWaitReason,
		poolHigherPriorityProjectWaitReason,
		poolHigherPriorityStateWaitReason,
		poolSelectedProjectWaitReason,
		string(CapacityConstraintProject),
		string(CapacityConstraintLane),
		string(CapacityConstraintWorkerHost),
		"worker_host_unavailable",
		string(CapacityConstraintRateWindow),
		string(CapacityConstraintTrackerUnavailable),
		string(CapacityConstraintForgeUnavailable),
		string(CapacityConstraintCIUnavailable),
		"local_slot_unavailable",
		since,
	)
	if err != nil {
		return nil, fmt.Errorf("querying capacity constraint waits: %w", err)
	}
	defer rows.Close()

	counts := map[string]CapacityConstraintWait{}
	samples := map[string]struct{}{}
	for rows.Next() {
		var projectID string
		var lane string
		var waitReason string
		var decisionAt string
		var snapshotJSON string
		if err := rows.Scan(&projectID, &lane, &waitReason, &decisionAt, &snapshotJSON); err != nil {
			return nil, fmt.Errorf("scanning capacity constraint wait: %w", err)
		}
		projectID = strings.TrimSpace(projectID)
		class := strings.TrimSpace(query.ProjectClasses[projectID])
		if class == "" {
			continue
		}

		var snapshot poolCapacitySnapshot
		if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
			return nil, fmt.Errorf("decode capacity snapshot for project %s: %w", projectID, err)
		}
		reason, ok := capacityConstraintReason(waitReason, snapshot.GlobalAvailable)
		if !ok {
			continue
		}
		pool := strings.TrimSpace(snapshot.Pool)
		if pool == "" {
			pool = "default"
		}
		sampledAt, err := time.Parse(time.RFC3339, strings.TrimSpace(decisionAt))
		if err != nil {
			return nil, fmt.Errorf("parse capacity constraint decision time for project %s: %w", projectID, err)
		}
		lane = strings.TrimSpace(lane)
		key := strings.Join([]string{projectID, class, pool, lane, string(reason)}, "\x00")
		sampleKey := key + "\x00" + sampledAt.UTC().Truncate(CapacityConstraintSampleInterval).Format(time.RFC3339)
		if _, ok := samples[sampleKey]; ok {
			continue
		}
		samples[sampleKey] = struct{}{}
		wait := counts[key]
		wait.ProjectID = projectID
		wait.WorkloadClass = class
		wait.Pool = pool
		wait.Lane = lane
		wait.Reason = reason
		wait.WaitCount++
		counts[key] = wait
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capacity constraint waits: %w", err)
	}

	waits := make([]CapacityConstraintWait, 0, len(counts))
	for _, wait := range counts {
		waits = append(waits, wait)
	}
	sort.Slice(waits, func(i, j int) bool {
		if waits[i].ProjectID != waits[j].ProjectID {
			return waits[i].ProjectID < waits[j].ProjectID
		}
		if waits[i].Reason != waits[j].Reason {
			return waits[i].Reason < waits[j].Reason
		}
		if waits[i].Lane != waits[j].Lane {
			return waits[i].Lane < waits[j].Lane
		}
		return waits[i].Pool < waits[j].Pool
	})
	return waits, nil
}

func capacityConstraintReason(waitReason string, globalAvailable *int) (CapacityConstraintReason, bool) {
	switch strings.TrimSpace(waitReason) {
	case poolCapacityWaitReason,
		poolHigherPriorityProjectWaitReason,
		poolHigherPriorityStateWaitReason,
		poolSelectedProjectWaitReason:
		if globalAvailable == nil || *globalAvailable != 0 {
			return "", false
		}
		return CapacityConstraintPool, true
	case string(CapacityConstraintProject):
		return CapacityConstraintProject, true
	case string(CapacityConstraintLane), "local_slot_unavailable":
		return CapacityConstraintLane, true
	case string(CapacityConstraintWorkerHost), "worker_host_unavailable":
		return CapacityConstraintWorkerHost, true
	case string(CapacityConstraintRateWindow):
		return CapacityConstraintRateWindow, true
	case string(CapacityConstraintTrackerUnavailable):
		return CapacityConstraintTrackerUnavailable, true
	case string(CapacityConstraintForgeUnavailable):
		return CapacityConstraintForgeUnavailable, true
	case string(CapacityConstraintCIUnavailable):
		return CapacityConstraintCIUnavailable, true
	default:
		return "", false
	}
}
