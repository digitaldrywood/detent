package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CreateReportedActionRunInput = pgquery.CreateReportedActionRunParams

type FinishActionRunInput = pgquery.FinishActionRunParams
type ReporterActionRunsInput = pgquery.RecentReporterActionRunsParams
type TerminalRecordingInput = pgquery.TerminalRecordingByIDParams
type ReadableTerminalRecordingsInput = pgquery.RecentReadableTerminalRecordingsParams
type ReferenceRelayRecordingInput = pgquery.ReferenceRelayRecordingParams
type DeletedProjectAction = pgquery.DeletedProjectActionsRow

type RelayActionLifecycleReader interface {
	RelayActionReader
	RecentActionRuns(context.Context, ActionRunsInput) ([]ActionRun, error)
	RecentReporterActionRuns(context.Context, ReporterActionRunsInput) ([]ActionRun, error)
	RecentReadableTerminalRecordings(context.Context, ReadableTerminalRecordingsInput) ([]TerminalRecording, error)
	RecentRelaySessions(context.Context, RelaySessionsInput) ([]RelaySession, error)
	ActionRunByID(context.Context, string) (ActionRun, error)
	ActiveActionRunsAfter(context.Context, string, int) ([]ActionRun, error)
	TerminalRecordingByID(context.Context, TerminalRecordingInput) (TerminalRecording, error)
	DeletedProjectActions(context.Context, int) ([]DeletedProjectAction, error)
}
type RelayActionLifecycleWriter interface {
	RelayActionLifecycleReader
	RelayActionWriter
	CreateReportedActionRun(context.Context, CreateReportedActionRunInput) (ActionRun, error)
	FinishActionRun(context.Context, FinishActionRunInput) (ActionRun, error)
	PruneRelayTickets(context.Context, time.Time, int) error
	ReferenceRelayRecording(context.Context, ReferenceRelayRecordingInput) error
	PurgeDeletedActionRun(context.Context, tracker.ProjectID, string) (ActionRun, error)
	PurgeEmptyDeletedAction(context.Context, tracker.ProjectID, string) error
}

func (s *session) DeletedProjectActions(ctx context.Context, limit int) ([]DeletedProjectAction, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.DeletedProjectActions(ctx, pgquery.DeletedProjectActionsParams{OrganizationID: s.organization, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) PurgeDeletedActionRun(ctx context.Context, project tracker.ProjectID, id string) (ActionRun, error) {
	row, err := s.queries.PurgeDeletedActionRun(ctx, pgquery.PurgeDeletedActionRunParams{OrganizationID: s.organization, ProjectID: project, ActionID: id})
	return row, mapError(ctx, err)
}

func (s *session) PurgeEmptyDeletedAction(ctx context.Context, project tracker.ProjectID, id string) error {
	_, err := s.queries.PurgeEmptyDeletedAction(ctx, pgquery.PurgeEmptyDeletedActionParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return mapError(ctx, err)
}

func (s *session) ActionRunByID(ctx context.Context, id string) (ActionRun, error) {
	row, err := s.queries.ActionRunByID(ctx, pgquery.ActionRunByIDParams{OrganizationID: s.organization, ID: id})
	return row, mapError(ctx, err)
}
func (s *session) ActiveActionRunsAfter(ctx context.Context, after string, limit int) ([]ActionRun, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.ActiveActionRunsAfter(ctx, pgquery.ActiveActionRunsAfterParams{OrganizationID: s.organization, ID: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) FinishActionRun(ctx context.Context, arg FinishActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.Status != "succeeded" && arg.Status != "failed" || (arg.FinishedAt == nil || arg.FinishedAt.IsZero()) || arg.OutputObjectKey != nil && (len(arg.OutputSha256) != 64 || arg.OutputContentBytes > 1048576) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.FinishActionRun(ctx, arg)
	return row, updated(ctx, err)
}
func (s *session) PruneRelayTickets(ctx context.Context, now time.Time, limit int) error {
	if err := checkPage(limit); err != nil {
		return err
	}
	_, err := s.queries.PruneRelayTickets(ctx, pgquery.PruneRelayTicketsParams{OrganizationID: s.organization, ExpiresAt: now, Limit: int32(limit)})
	return mapError(ctx, err)
}
func (s *session) TerminalRecordingByID(ctx context.Context, arg TerminalRecordingInput) (TerminalRecording, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return TerminalRecording{}, err
	}
	row, err := s.queries.TerminalRecordingByID(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) ReferenceRelayRecording(ctx context.Context, arg ReferenceRelayRecordingInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	_, err := s.queries.ReferenceRelayRecording(ctx, arg)
	return mapError(ctx, err)
}

func (s *session) CreateReportedActionRun(ctx context.Context, arg CreateReportedActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.ID == "" || arg.CreatedAt.IsZero() || arg.OutputSequence < 1 || arg.Status != "running" && arg.Status != "succeeded" && arg.Status != "failed" || arg.Status != "running" && arg.FinishedAt == nil {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	now, err := s.leaseTime(ctx)
	if err != nil {
		return ActionRun{}, err
	}
	arg.Now = now
	row, err := s.queries.CreateReportedActionRun(ctx, arg)
	return row, updated(ctx, err)
}

func (s *session) RecentActionRuns(ctx context.Context, arg ActionRunsInput) ([]ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecentActionRuns(ctx, pgquery.RecentActionRunsParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ActionID: arg.ActionID, Limit: arg.Limit})
	return rows, mapError(ctx, err)
}

func (s *session) RecentReporterActionRuns(ctx context.Context, arg ReporterActionRunsInput) ([]ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.PageLimit)); err != nil {
		return nil, err
	}
	if arg.PrincipalID == "" {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecentReporterActionRuns(ctx, arg)
	return rows, mapError(ctx, err)
}

func (s *session) RecentReadableTerminalRecordings(ctx context.Context, arg ReadableTerminalRecordingsInput) ([]TerminalRecording, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.PageLimit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecentReadableTerminalRecordings(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	recordings := make([]TerminalRecording, len(rows))
	for index, row := range rows {
		recordings[index] = TerminalRecording(row)
	}
	return recordings, nil
}

func (s *session) RecentRelaySessions(ctx context.Context, arg RelaySessionsInput) ([]RelaySession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecentRelaySessions(ctx, pgquery.RecentRelaySessionsParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, WorkspaceID: arg.WorkspaceID, Limit: arg.Limit})
	return rows, mapError(ctx, err)
}
