package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerAdmissionObservations(ctx context.Context, project tracker.ProjectID, runners []string) (map[string]tracker.NativeAdmissionObservation, error) {
	if project == "" || len(runners) > 100 {
		return nil, corestore.ErrInvalidPage
	}
	result := make(map[string]tracker.NativeAdmissionObservation, len(runners))
	rows, err := s.db.Query(ctx, `SELECT runner_id,runner_revision,received_at,observed_at,policy_id,last_refresh_at,refresh_duration
FROM runner_native_admission_observations WHERE organization_id=$1 AND project_id=$2 AND runner_id=ANY($3::text[])`, s.organization, project, runners)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for rows.Next() {
		var id string
		var observation tracker.NativeAdmissionObservation
		if err := rows.Scan(&id, &observation.RunnerRevision, &observation.ReceivedAt, &observation.Context.ObservedAt, &observation.Context.PolicyID, &observation.Context.LastRefreshAt, &observation.Context.RefreshDuration); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		result[id] = observation
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, mapError(ctx, err)
	}
	rows, err = s.db.Query(ctx, `SELECT runner_id,kind,value FROM runner_native_admission_selectors
WHERE organization_id=$1 AND project_id=$2 AND runner_id=ANY($3::text[]) ORDER BY runner_id,kind,position LIMIT $4`, s.organization, project, runners, 160*len(runners)+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	counts := make(map[string]int, len(runners))
	for rows.Next() {
		var id, kind, value string
		if err := rows.Scan(&id, &kind, &value); err != nil {
			return nil, mapError(ctx, err)
		}
		observation, found := result[id]
		counts[id]++
		if !found || counts[id] > 160 {
			return nil, corestore.ErrInvalidPage
		}
		switch kind {
		case "workflow":
			observation.Context.WorkflowStates = append(observation.Context.WorkflowStates, value)
		case "author":
			observation.Context.Authors = append(observation.Context.Authors, value)
		case "assignee":
			observation.Context.Assignees = append(observation.Context.Assignees, value)
		case "include":
			observation.Context.LabelInclude = append(observation.Context.LabelInclude, value)
		case "exclude":
			observation.Context.LabelExclude = append(observation.Context.LabelExclude, value)
		default:
			return nil, corestore.ErrInvalidPage
		}
		result[id] = observation
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	for _, observation := range result {
		if observation.Context.Validate() != nil {
			return nil, corestore.ErrInvalidPage
		}
	}
	return result, nil
}
