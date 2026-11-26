package runnerrecords

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

const automaticOfferSlots = 8

const automaticOfferSQL = `(d.automatic_offer_slot IS NOT NULL)`

const currentAutomaticOfferSQL = `(d.automatic_offer_slot IS NULL
 OR (d.automatic_organization_revision=COALESCE(o.revision,0)
 AND d.automatic_runner_revision=CASE WHEN COALESCE(o.version,'')='' THEN COALESCE(t.revision,0) ELSE 0 END
 AND d.expected_build_revision=b.revision))`

func (s *Store) EnsureAutomaticRunnerOffer(ctx context.Context, record runnerstore.UpdateRequestRecord, policy runnerstore.AutomaticOfferPolicy) (string, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return "", err
	}
	request := record.Request
	var requestID string
	var nextSlot int16
	err := s.db.QueryRow(ctx, `SELECT COALESCE(max(request_id) FILTER (WHERE expected_build_revision=$6),''), COALESCE(max(automatic_offer_slot)+1,0)
 FROM runner_update_requests WHERE organization_id=$1 AND runner_id=$2 AND version=$3
 AND automatic_organization_revision=$4 AND automatic_runner_revision=$5 AND automatic_offer_slot IS NOT NULL`,
		s.organization, record.RunnerID, request.Version, policy.OrganizationRevision, policy.RunnerRevision, request.ExpectedBuildRevision).Scan(&requestID, &nextSlot)
	if err != nil {
		return "", s.mapError(ctx, err)
	}
	if requestID != "" || nextSlot >= automaticOfferSlots {
		return requestID, nil
	}
	if err := s.createRunnerUpdateRequest(ctx, record, policy, &nextSlot); err != nil {
		return "", err
	}
	return request.ID, nil
}

func (s *Store) InvalidateAutomaticRunnerOffer(ctx context.Context, id, requestID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM runner_update_desired desired USING runner_update_requests d
 WHERE desired.organization_id=$1 AND desired.runner_id=$2 AND desired.request_id=$3
 AND d.organization_id=desired.organization_id AND d.request_id=desired.request_id AND `+automaticOfferSQL,
		s.organization, id, requestID)
	return s.mapError(ctx, err)
}
