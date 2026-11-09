package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type ActionRun = pgquery.ProjectActionRun
type RelayTicket = pgquery.WorkspaceRelayTicket
type RelaySession = pgquery.WorkspaceRelaySession
type TerminalRecording = pgquery.WorkspaceTerminalRecording
type ActionRunInput = pgquery.ActionRunParams
type ActionRunsInput = pgquery.ActionRunsParams
type RelaySessionsInput = pgquery.RelaySessionsParams
type TerminalRecordingsInput = pgquery.TerminalRecordingsParams
type CreateRelayTicketInput = pgquery.CreateRelayTicketParams
type RedeemRelayTicketInput = pgquery.RedeemRelayTicketParams
type CreateRelaySessionInput = pgquery.CreateRelaySessionParams
type CloseRelaySessionInput = pgquery.CloseRelaySessionParams
type PutTerminalRecordingInput = pgquery.PutTerminalRecordingParams
type CreateActionRunInput = pgquery.CreateActionRunParams
type ClaimActionRunInput = pgquery.ClaimActionRunParams
type ReportActionRunInput = pgquery.ReportActionRunParams
type RelayActionReader interface {
	actionstore.Reader
	ActionRun(context.Context, ActionRunInput) (ActionRun, error)
	ActionRuns(context.Context, ActionRunsInput) ([]ActionRun, error)
	RelaySessions(context.Context, RelaySessionsInput) ([]RelaySession, error)
	TerminalRecordings(context.Context, TerminalRecordingsInput) ([]TerminalRecording, error)
}
type RelayActionWriter interface {
	actionstore.Writer
	RelayActionReader
	CreateRelayTicket(context.Context, CreateRelayTicketInput) (RelayTicket, error)
	RedeemRelayTicket(context.Context, RedeemRelayTicketInput) (RelayTicket, error)
	CreateRelaySession(context.Context, CreateRelaySessionInput) (RelaySession, error)
	CloseRelaySession(context.Context, CloseRelaySessionInput) error
	PutTerminalRecording(context.Context, PutTerminalRecordingInput) (TerminalRecording, error)
	CreateActionRun(context.Context, CreateActionRunInput) (ActionRun, error)
	ClaimActionRun(context.Context, ClaimActionRunInput) (ActionRun, error)
	ReportActionRun(context.Context, ReportActionRunInput) (ActionRun, error)
}

func (r *Repository) ReadRelayAction(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(RelayActionReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteRelayAction(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(RelayActionWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) ActionRun(ctx context.Context, arg ActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}

	row, err := s.queries.ActionRun(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) ActionRuns(ctx context.Context, arg ActionRunsInput) ([]ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.ActionRuns(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) RelaySessions(ctx context.Context, arg RelaySessionsInput) ([]RelaySession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.RelaySessions(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) TerminalRecordings(ctx context.Context, arg TerminalRecordingsInput) ([]TerminalRecording, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.TerminalRecordings(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) CreateRelayTicket(ctx context.Context, arg CreateRelayTicketInput) (RelayTicket, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return RelayTicket{}, err
	}
	if arg.CreatedAt.IsZero() || !arg.ExpiresAt.After(arg.CreatedAt) || arg.PrincipalID == "" || arg.SessionID == "" || arg.Subject == "" || arg.Digest == "" {
		return RelayTicket{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.CreateRelayTicket(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) RedeemRelayTicket(ctx context.Context, arg RedeemRelayTicketInput) (RelayTicket, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return RelayTicket{}, err
	}
	if arg.Now.IsZero() {
		return RelayTicket{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.RedeemRelayTicket(ctx, arg)
	return row, updated(ctx, err)
}
func (s *session) CreateRelaySession(ctx context.Context, arg CreateRelaySessionInput) (RelaySession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return RelaySession{}, err
	}
	if arg.OpenedAt.IsZero() || arg.ID == "" || arg.ConnectionID == "" || arg.PrincipalID == "" {
		return RelaySession{}, corestore.ErrInvalidPage
	}
	for _, channel := range arg.Channels {
		if !workspacesession.ValidChannel(channel) {
			return RelaySession{}, corestore.ErrInvalidPage
		}
	}
	row, err := s.queries.CreateRelaySession(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) CloseRelaySession(ctx context.Context, arg CloseRelaySessionInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.ClosedAt == nil || arg.ClosedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	count, err := s.queries.CloseRelaySession(ctx, arg)
	return affected(ctx, count, err)
}
func (s *session) PutTerminalRecording(ctx context.Context, arg PutTerminalRecordingInput) (TerminalRecording, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return TerminalRecording{}, err
	}
	if arg.ExpectedRevision < 0 || arg.StartedAt.IsZero() || arg.UpdatedAt.IsZero() || arg.CastObjectKey == "" || len(arg.CastSha256) != 64 {
		return TerminalRecording{}, corestore.ErrInvalidPage
	}
	current, loadErr := s.queries.TerminalRecording(ctx, pgquery.TerminalRecordingParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ID: arg.ID})
	if loadErr != nil && !errors.Is(loadErr, pgx.ErrNoRows) {
		return TerminalRecording{}, mapError(ctx, loadErr)
	}
	if errors.Is(loadErr, pgx.ErrNoRows) && arg.ExpectedRevision != 0 || loadErr == nil && current.Revision != arg.ExpectedRevision {
		return TerminalRecording{}, corestore.ErrRevisionConflict
	}
	row, err := s.queries.PutTerminalRecording(ctx, arg)
	return row, updated(ctx, err)
}
func (s *session) CreateActionRun(ctx context.Context, arg CreateActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.ID == "" || arg.CreatedAt.IsZero() {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.CreateActionRun(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.ActionRun(ctx, pgquery.ActionRunParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ID: arg.ID})
		if err == nil && (row.ActionID != arg.ActionID || row.WorkspaceID != arg.WorkspaceID || row.CreatedBy != arg.CreatedBy) {
			return ActionRun{}, corestore.ErrUniqueViolation
		}
	}
	return row, mapError(ctx, err)
}
func (s *session) ClaimActionRun(ctx context.Context, arg ClaimActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.StartedAt.IsZero() || arg.FencingToken < 1 {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.ClaimActionRun(ctx, arg)
	return row, updated(ctx, err)
}
func (s *session) ReportActionRun(ctx context.Context, arg ReportActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.UpdatedAt.IsZero() || arg.OutputSequence < 1 || arg.Status != "running" && arg.Status != "succeeded" && arg.Status != "failed" || arg.Status != "running" && arg.FinishedAt == nil || arg.OutputObjectKey != nil && (*arg.OutputObjectKey == "" || len(arg.OutputSha256) != 64) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.ReportActionRun(ctx, arg)
	return row, updated(ctx, err)
}
