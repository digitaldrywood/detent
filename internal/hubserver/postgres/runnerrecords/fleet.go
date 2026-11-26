package runnerrecords

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

func (s *Store) FilterFleetOffers(ctx context.Context, records []runnerstore.FleetRecord, indices map[string]int, ids []string) error {
	rows, err := s.db.Query(ctx, `SELECT r.runner_id,COALESCE(NULLIF(o.version,''),NULLIF(t.version,''),''),
 EXISTS(SELECT 1 FROM runner_lifecycle f WHERE f.organization_id=r.organization_id AND f.version=d.version AND f.rollout_revision=COALESCE(o.revision,0) AND `+failedRolloutSQL+`),
 EXISTS(SELECT 1 FROM runner_lifecycle f WHERE f.organization_id=r.organization_id AND f.version=u.version AND f.rollout_revision=COALESCE(o.revision,0) AND `+failedRolloutSQL+`),
 COALESCE(u.version,''),COALESCE(`+currentHoldOfferSQL+`,false)
 FROM runner_identities r
 LEFT JOIN runner_update_targets o ON o.organization_id=r.organization_id AND o.runner_id=''
 LEFT JOIN runner_update_targets t ON t.organization_id=r.organization_id AND t.runner_id=r.runner_id
 LEFT JOIN runner_update_desired desired ON desired.organization_id=r.organization_id AND desired.runner_id=r.runner_id
 LEFT JOIN runner_update_requests d ON d.organization_id=desired.organization_id AND d.request_id=desired.request_id
 LEFT JOIN runner_urgent_updates urgent ON urgent.organization_id=r.organization_id
 LEFT JOIN runner_update_requests u ON u.organization_id=urgent.organization_id AND u.request_id=urgent.request_id
 WHERE r.organization_id=$1 AND r.runner_id=ANY($2)`, s.organization, ids)
	if err != nil {
		return s.mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, held, urgentVersion string
		var failed, urgentFailed, currentHoldOffer bool
		if err := rows.Scan(&id, &held, &failed, &urgentFailed, &urgentVersion, &currentHoldOffer); err != nil {
			return s.mapError(ctx, err)
		}
		record := &records[indices[id]]
		state := &record.Update
		state.UrgentPolicyChecked = true
		state.UrgentAllowed = !urgentFailed && (held == "" || held == urgentVersion)
		if state.Desired != nil && (failed || !currentHoldOffer || held != "" && held != state.Desired.Version) {
			state.Desired = nil
		}
		state.TargetHeld = held != ""
		state.TargetVersion = held
		if held == "" && state.Desired != nil {
			state.TargetVersion = state.Desired.Version
		}
		if state.TargetVersion == "" && record.Lifecycle != nil {
			state.TargetVersion = record.Lifecycle.TargetVersion
		}
	}
	return s.mapError(ctx, rows.Err())
}
