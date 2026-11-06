package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectIntakeBatch(ctx context.Context, project tracker.ProjectID) (projectstore.IntakeBatch, error) {
	row, err := s.queries.ProjectIntakeBatch(ctx, pgquery.ProjectIntakeBatchParams{OrganizationID: s.organization, ProjectID: project})
	return projectstore.IntakeBatch{ProjectID: row.ProjectID, ID: row.ID, Revision: row.Revision, Status: row.BatchStatus, HubOwned: row.HubOwned, Payload: eventstore.Receipt{OrganizationID: row.OrganizationID, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}}, mapError(ctx, err)
}

func (s *session) SaveProjectIntakeBatch(ctx context.Context, row projectstore.IntakeBatch, expected int64) error {
	if err := s.checkOrganization(row.Payload.OrganizationID); err != nil {
		return err
	}
	if row.ProjectID == "" || row.ID == "" || row.Status == "" || expected < 0 || row.Revision != expected+1 {
		return eventstore.ErrInvalidRecord
	}
	payload := row.Payload
	if err := validRecordPayload(s.organization, payload.PayloadObjectKey, payload.PayloadSHA256, payload.PayloadBytes, payload.PayloadContentBytes); err != nil {
		return err
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: row.ProjectID}); err != nil {
		return mapError(ctx, err)
	}
	count, err := s.queries.SaveProjectIntakeBatch(ctx, pgquery.SaveProjectIntakeBatchParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ID: row.ID, Revision: row.Revision, BatchStatus: row.Status, HubOwned: row.HubOwned, PayloadObjectKey: payload.PayloadObjectKey, PayloadSHA256: payload.PayloadSHA256, PayloadBytes: payload.PayloadBytes, PayloadContentBytes: payload.PayloadContentBytes, ExpectedRevision: expected})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
