package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (s *session) ReadRunnerCapacityFacts(ctx context.Context, query reportstore.RunnerCapacityQuery) (reportstore.RunnerCapacityFacts, error) {
	out := reportstore.RunnerCapacityFacts{}
	if query.RunnerID == "" || query.MachineID == "" || query.From.IsZero() || !query.From.Before(query.To) || query.To.Sub(query.From) > 720*time.Hour || query.Limit < 1 || query.Limit > reportstore.RunnerCapacityFactLimit {
		return out, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT l.lease_id,l.acquired_at,least(coalesce(l.released_at,l.expires_at),l.expires_at)
 FROM leases l JOIN LATERAL (
 SELECT 1 FROM runner_lease_assignments a
 WHERE a.organization_id=l.organization_id AND a.lease_id=l.lease_id AND a.runner_id=$3 LIMIT 1
 ) assigned ON true
 WHERE l.organization_id=$1 AND l.machine_id=$2 AND l.acquired_at<$5
 AND l.expires_at>$4 AND (l.released_at IS NULL OR l.released_at>$4)
 ORDER BY l.expires_at,l.lease_id LIMIT $6`, s.organization, query.MachineID, query.RunnerID, query.From.UTC(), query.To.UTC(), query.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var fact reportstore.RunnerOccupancy
		if err := rows.Scan(&fact.LeaseID, &fact.From, &fact.To); err != nil {
			return out, mapError(ctx, err)
		}
		out.Occupancy = append(out.Occupancy, fact)
	}
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	rows.Close()
	out.OccupancyPartial = len(out.Occupancy) > query.Limit
	out.Occupancy = out.Occupancy[:min(len(out.Occupancy), query.Limit)]
	leaseIDs := make([]string, len(out.Occupancy))
	for i, lease := range out.Occupancy {
		leaseIDs[i] = lease.LeaseID
	}
	if len(leaseIDs) == 0 {
		return out, nil
	}
	rows, err = s.db.Query(ctx, `SELECT a.id,a.started_at,coalesce(f.process_evidence_status,'unknown'),
 coalesce(f.process_peak_memory_bytes,0),coalesce(f.process_user_cpu_seconds,0),coalesce(f.process_system_cpu_seconds,0),coalesce(f.process_wall_seconds,0),
 coalesce(e.type='run.finished' AND e.attempt_id=a.id AND e.run_id=a.run_id
 AND e.project_id=i.project_id AND e.work_item_id=i.native_id
 AND f.run_status=a.status AND f.run_sequence>0 AND f.run_sequence<=a.sequence
 AND f.process_lease_id=a.lease_id AND f.process_fencing_token=a.fencing_token
 AND (f.process_runner_id='' OR f.process_runner_id=$5)
 AND (f.process_machine_id='' OR f.process_machine_id=$6),false)
 FROM unnest($2::text[]) keys(lease_id)
 JOIN LATERAL (SELECT id,issue_id,lease_id,run_id,fencing_token,sequence,status,started_at,updated_at FROM native_attempts a WHERE a.organization_id=$1 AND a.lease_id=keys.lease_id LIMIT 1) a ON true
 JOIN LATERAL (SELECT project_id,native_id FROM work_item_identities i WHERE i.organization_id=$1 AND i.id=a.issue_id LIMIT 1) i ON true
 LEFT JOIN LATERAL (SELECT outcome_event_id FROM report_attempt_facts p WHERE p.organization_id=$1 AND p.attempt_id=a.id LIMIT 1) p ON true
 LEFT JOIN LATERAL (SELECT event_id,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,run_status,run_sequence,process_lease_id,process_fencing_token,process_runner_id,process_machine_id FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=p.outcome_event_id LIMIT 1) f ON true
 LEFT JOIN LATERAL (SELECT type,attempt_id,run_id,project_id,work_item_id FROM collaboration_events e WHERE e.organization_id=$1 AND e.id=f.event_id LIMIT 1) e ON true
 WHERE a.status IN ('succeeded','failed','cancelled') AND a.started_at>=$3 AND a.updated_at<=$4
 ORDER BY a.started_at DESC,a.id DESC LIMIT $7`, s.organization, leaseIDs, query.From.UTC(), query.To.UTC(), query.RunnerID, query.MachineID, query.Limit+1)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var fact reportstore.RunnerProcessUsage
		var status string
		var verified bool
		if err := rows.Scan(&fact.AttemptID, &fact.StartedAt, &status, &fact.Usage.PeakMemoryBytes, &fact.Usage.UserCPUSeconds, &fact.Usage.SystemCPUSeconds, &fact.Usage.WallSeconds, &verified); err != nil {
			return out, mapError(ctx, err)
		}
		out.EligibleRuns++
		if out.EligibleRuns > query.Limit {
			out.RunsPartial = true
			break
		}
		if !verified || status == "unknown" {
			out.UnknownRuns++
			out.RunsPartial = true
			continue
		}
		if status == "usable" {
			out.Runs = append(out.Runs, fact)
		}
	}
	if err := rows.Err(); err != nil {
		return out, mapError(ctx, err)
	}
	out.RunsPartial = out.RunsPartial || out.OccupancyPartial
	return out, nil
}
