package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SlackIntegration = pgquery.SlackIntegration
type SlackDelivery = pgquery.HealthSlackDelivery
type SlackDeliveryInput = pgquery.SlackDeliveryParams
type PendingSlackDeliveriesInput = pgquery.PendingSlackDeliveriesParams
type CreateSlackIntegrationInput = pgquery.CreateSlackIntegrationParams
type UpdateSlackIntegrationInput = pgquery.UpdateSlackIntegrationParams
type RecordSlackStatusInput = pgquery.RecordSlackStatusParams
type EnqueueSlackDeliveryInput = pgquery.EnqueueSlackDeliveryParams
type ClaimSlackDeliveryInput = pgquery.ClaimSlackDeliveryParams
type FinishSlackDeliveryInput = pgquery.FinishSlackDeliveryParams
type SlackReader interface {
	SlackIntegration(context.Context) (SlackIntegration, error)
	AccessSecret(context.Context, tracker.ProjectID, string) (accessstore.Secret, error)
	SlackDelivery(context.Context, SlackDeliveryInput) (SlackDelivery, error)
	PendingSlackDeliveries(context.Context, PendingSlackDeliveriesInput) ([]SlackDelivery, error)
}
type SlackWriter interface {
	SlackReader
	StoreAccessSecret(context.Context, accessstore.Secret, accessstore.SecretAudit) error
	CreateSlackIntegration(context.Context, CreateSlackIntegrationInput) (SlackIntegration, error)
	UpdateSlackIntegration(context.Context, UpdateSlackIntegrationInput) (SlackIntegration, error)
	RecordSlackStatus(context.Context, RecordSlackStatusInput) error
	EnqueueSlackDelivery(context.Context, EnqueueSlackDeliveryInput) (SlackDelivery, error)
	ClaimSlackDelivery(context.Context, ClaimSlackDeliveryInput) (SlackDelivery, error)
	FinishSlackDelivery(context.Context, FinishSlackDeliveryInput) (SlackDelivery, error)
}

func (r *Repository) ReadSlack(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(SlackReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteSlack(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(SlackWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) SlackDelivery(ctx context.Context, arg SlackDeliveryInput) (SlackDelivery, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackDelivery{}, err
	}

	row, err := s.queries.SlackDelivery(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) PendingSlackDeliveries(ctx context.Context, arg PendingSlackDeliveriesInput) ([]SlackDelivery, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	if arg.NextAttemptAt.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	row, err := s.queries.PendingSlackDeliveries(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CreateSlackIntegration(ctx context.Context, arg CreateSlackIntegrationInput) (SlackIntegration, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackIntegration{}, err
	}
	if arg.UpdatedAt.IsZero() {
		return SlackIntegration{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.CreateSlackIntegration(ctx, arg)

	return row, updated(ctx, err)
}
func (s *session) UpdateSlackIntegration(ctx context.Context, arg UpdateSlackIntegrationInput) (SlackIntegration, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackIntegration{}, err
	}
	if arg.UpdatedAt.IsZero() || arg.Revision < 1 {
		return SlackIntegration{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.UpdateSlackIntegration(ctx, arg)

	return row, updated(ctx, err)
}
func (s *session) RecordSlackStatus(ctx context.Context, arg RecordSlackStatusInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.At.IsZero() {
		return corestore.ErrInvalidPage
	}
	count, err := s.queries.RecordSlackStatus(ctx, arg)
	return affected(ctx, count, err)
}
func (s *session) EnqueueSlackDelivery(ctx context.Context, arg EnqueueSlackDeliveryInput) (SlackDelivery, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackDelivery{}, err
	}
	if arg.ID == "" || arg.QueuedAt.IsZero() || arg.EventAt.IsZero() {
		return SlackDelivery{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.EnqueueSlackDelivery(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.SlackDelivery(ctx, pgquery.SlackDeliveryParams{OrganizationID: s.organization, ID: arg.ID})
		if err == nil && (row.FindingID != arg.FindingID || row.Event != arg.Event || !row.EventAt.Equal(arg.EventAt)) {
			return SlackDelivery{}, corestore.ErrUniqueViolation
		}
	}
	return row, mapError(ctx, err)
}
func (s *session) ClaimSlackDelivery(ctx context.Context, arg ClaimSlackDeliveryInput) (SlackDelivery, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackDelivery{}, err
	}
	if arg.Now.IsZero() || arg.ExpectedAttempts < 0 || !arg.NextAttemptAt.After(arg.Now) {
		return SlackDelivery{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.ClaimSlackDelivery(ctx, arg)

	return row, updated(ctx, err)
}
func (s *session) FinishSlackDelivery(ctx context.Context, arg FinishSlackDeliveryInput) (SlackDelivery, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return SlackDelivery{}, err
	}
	if arg.At.IsZero() || arg.NextAttemptAt.IsZero() || arg.ExpectedAttempts < 1 || arg.Outcome == "committed" && arg.ExternalReceipt == "" {
		return SlackDelivery{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.FinishSlackDelivery(ctx, arg)

	return row, updated(ctx, err)
}

func (s *session) SlackIntegration(ctx context.Context) (SlackIntegration, error) {
	row, err := s.queries.SlackIntegration(ctx, s.organization)
	return row, mapError(ctx, err)
}
