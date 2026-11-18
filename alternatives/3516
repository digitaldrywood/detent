package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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
type WorkspaceRelayOwnerInput = pgquery.WorkspaceRelayOwnerParams
type FinishRelayActionRunInput = pgquery.FinishRelayActionRunParams
type AttachRelayRecordingInput = pgquery.AttachRelayRecordingParams
type CreateReportedActionRunInput = pgquery.CreateReportedActionRunParams
type ApplyReportedActionRunInput = pgquery.ApplyReportedActionRunParams
type ActionRunMaintenanceRow = pgquery.ScanActiveActionRunsRow
type ActionRunDispatchRow = pgquery.DispatchableActionRunsRow
type OrphanedActionRunInput = pgquery.FailOrphanedActionRunParams
type ActionRunCursor struct {
	CreatedAt time.Time
	ID        string
}
type RelayActionReader interface {
	actionstore.Reader
	ActionRun(context.Context, ActionRunInput) (ActionRun, error)
	ActionRuns(context.Context, ActionRunsInput) ([]ActionRun, error)
	RelaySessions(context.Context, RelaySessionsInput) ([]RelaySession, error)
	TerminalRecordings(context.Context, TerminalRecordingsInput) ([]TerminalRecording, error)
	WorkspaceRelayOwner(context.Context, WorkspaceRelayOwnerInput) (WorkspaceSession, error)
	RecentActionRuns(context.Context, tracker.ProjectID, string, *ActionRunCursor, int) ([]ActionRun, error)
	ActiveActionRunPage(context.Context, tracker.ProjectID, string, int) ([]ActionRunMaintenanceRow, error)
	DispatchableActionRunPage(context.Context, time.Time, *ActionRunCursor, int) ([]ActionRunDispatchRow, error)
	RelayTicketPage(context.Context, string, int) ([]RelayTicket, error)
	TerminalRecording(context.Context, tracker.ProjectID, string) (TerminalRecording, error)
	RecentTerminalRecordings(context.Context, tracker.ProjectID, string, string, int) ([]TerminalRecording, error)
}
type RelayActionWriter interface {
	actionstore.Writer
	RelayActionReader
	CreateRelayTicket(context.Context, CreateRelayTicketInput) (RelayTicket, error)
	RedeemRelayTicket(context.Context, RedeemRelayTicketInput) (RelayTicket, error)
	CreateRelaySession(context.Context, CreateRelaySessionInput) (RelaySession, error)
	CloseRelaySession(context.Context, CloseRelaySessionInput) error
	PutTerminalRecording(context.Context, PutTerminalRecordingInput) (TerminalRecording, error)
	DeleteActionRun(context.Context, tracker.ProjectID, string, int64) error
	CreateActionRun(context.Context, CreateActionRunInput) (ActionRun, error)
	ClaimActionRun(context.Context, ClaimActionRunInput) (ActionRun, error)
	ReportActionRun(context.Context, ReportActionRunInput) (ActionRun, error)
	FinishRelayActionRun(context.Context, FinishRelayActionRunInput) (ActionRun, error)
	FailOrphanedActionRun(context.Context, OrphanedActionRunInput) (ActionRun, error)
	CreateReportedActionRun(context.Context, CreateReportedActionRunInput) (ActionRun, error)
	ApplyReportedActionRun(context.Context, ApplyReportedActionRunInput) (ActionRun, error)
	DeleteExpiredRelayTicket(context.Context, string, time.Time, time.Time) error
	AttachRelayRecording(context.Context, AttachRelayRecordingInput) error
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
		previous, loadErr := s.queries.ActionRun(ctx, pgquery.ActionRunParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, ID: arg.ID})
		if loadErr == nil && (previous.ActionID != arg.ActionID || previous.WorkspaceID != arg.WorkspaceID || previous.CreatedBy != arg.CreatedBy) {
			return ActionRun{}, corestore.ErrUniqueViolation
		}
		return previous, mapError(ctx, loadErr)
	}
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, mapError(ctx, err))
}
func (s *session) ClaimActionRun(ctx context.Context, arg ClaimActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.StartedAt == nil || arg.StartedAt.IsZero() || arg.FencingToken < 1 {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	arg.LeaseClock = s.leaseClockValue()
	row, err := s.queries.ClaimActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}
func (s *session) ReportActionRun(ctx context.Context, arg ReportActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.UpdatedAt.IsZero() || arg.OutputSequence < 1 || arg.Status != "running" && arg.Status != "succeeded" && arg.Status != "failed" || arg.Status != "running" && arg.FinishedAt == nil || arg.OutputObjectKey != nil && (*arg.OutputObjectKey == "" || len(arg.OutputSha256) != 64) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	arg.LeaseClock = s.leaseClockValue()
	row, err := s.queries.ReportActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}

func (s *session) WorkspaceRelayOwner(ctx context.Context, arg WorkspaceRelayOwnerInput) (WorkspaceSession, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return WorkspaceSession{}, err
	}
	if arg.Now.IsZero() || arg.FencingToken < 1 {
		return WorkspaceSession{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.WorkspaceRelayOwner(ctx, arg)
	return row, mapError(ctx, err)
}
func (s *session) FinishRelayActionRun(ctx context.Context, arg FinishRelayActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.FinishedAt.IsZero() || arg.ExpectedRevision < 1 || arg.ExpectedOutputSequence < 0 || arg.FencingToken < 1 || arg.Claimant == "" || !workspacesession.TerminalRunStatus(arg.Status) || arg.OutputObjectKey == nil && (arg.OutputSha256 != "" || arg.OutputBytes != 0 || arg.OutputContentBytes != 0) || arg.OutputObjectKey != nil && (*arg.OutputObjectKey == "" || len(arg.OutputSha256) != 64 || arg.OutputBytes <= 0 || arg.OutputContentBytes <= 0 || arg.OutputContentBytes > workspacesession.MaxExecOutputBytes) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	if arg.Reason == "" {
		if !arg.ExitCode.Valid || workspacesession.RunStatusForExit(int(arg.ExitCode.Int32)) != arg.Status {
			return ActionRun{}, corestore.ErrInvalidPage
		}
	} else if arg.Status != workspacesession.RunFailed || arg.ExitCode.Valid || arg.Reason != workspacesession.RunReasonStreamClosed && arg.Reason != workspacesession.RunReasonWorkspaceEnd && arg.Reason != workspacesession.RunReasonLeaseLost {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	arg.LeaseClock = s.leaseClockValue()
	row, err := s.queries.FinishRelayActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}

func (s *session) RecentActionRuns(ctx context.Context, project tracker.ProjectID, action string, before *ActionRunCursor, limit int) ([]ActionRun, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	arg := pgquery.RecentActionRunsParams{OrganizationID: s.organization, ProjectID: project, ActionID: action, PageLimit: int32(limit)}
	if before != nil {
		if before.CreatedAt.IsZero() || before.ID == "" {
			return nil, corestore.ErrInvalidPage
		}
		arg.BeforeCreatedAt, arg.BeforeID = &before.CreatedAt, before.ID
	}
	keys, err := s.queries.RecentActionRuns(ctx, arg)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(keys) == 0 {
		return []ActionRun{}, nil
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	rows, err := s.queries.ActionRunRecordsByIDs(ctx, pgquery.ActionRunRecordsByIDsParams{OrganizationID: s.organization, ProjectID: project, ActionID: action, RunIds: ids})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	byID := make(map[string]ActionRun, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	ordered := make([]ActionRun, len(keys))
	for i, key := range keys {
		row, ok := byID[key.ID]
		if !ok || !row.CreatedAt.Equal(key.CreatedAt) {
			return nil, corestore.ErrRevisionConflict
		}
		ordered[i] = row
	}
	return ordered, nil
}
func (s *session) ActiveActionRunPage(ctx context.Context, project tracker.ProjectID, id string, limit int) ([]ActionRunMaintenanceRow, error) {
	if limit < 1 || limit > 500 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ScanActiveActionRuns(ctx, pgquery.ScanActiveActionRunsParams{OrganizationID: s.organization, AfterProjectID: string(project), AfterID: id, PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) DispatchableActionRunPage(ctx context.Context, before time.Time, after *ActionRunCursor, limit int) ([]ActionRunDispatchRow, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if before.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	arg := pgquery.DispatchableActionRunsParams{OrganizationID: s.organization, BeforeCreatedAt: before, PageLimit: int32(limit)}
	if after != nil {
		if after.CreatedAt.IsZero() || after.ID == "" {
			return nil, corestore.ErrInvalidPage
		}
		arg.AfterCreatedAt, arg.AfterID = &after.CreatedAt, after.ID
	}
	rows, err := s.queries.DispatchableActionRuns(ctx, arg)
	return rows, mapError(ctx, err)
}
func (s *session) RelayTicketPage(ctx context.Context, after string, limit int) ([]RelayTicket, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RelayTicketPage(ctx, pgquery.RelayTicketPageParams{OrganizationID: s.organization, AfterDigest: after, PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) DeleteExpiredRelayTicket(ctx context.Context, digest string, now, redeemedBefore time.Time) error {
	if digest == "" || now.IsZero() || redeemedBefore.IsZero() {
		return corestore.ErrInvalidPage
	}
	return mapError(ctx, s.queries.DeleteExpiredRelayTicket(ctx, pgquery.DeleteExpiredRelayTicketParams{OrganizationID: s.organization, Digest: digest, Now: now, RedeemedBefore: redeemedBefore}))
}
func (s *session) FailOrphanedActionRun(ctx context.Context, arg OrphanedActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.Now.IsZero() || arg.ExpectedRevision < 1 {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.FailOrphanedActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}

func validReportedActionRun(status, reason string, key *string, digest string, compressed, content int64) bool {
	if !workspacesession.ValidRunStatus(status) || reason != "" && reason != workspacesession.RunReasonStreamClosed && reason != workspacesession.RunReasonWorkspaceEnd && reason != workspacesession.RunReasonLeaseLost && reason != workspacesession.RunReasonKilled {
		return false
	}
	if key == nil {
		return digest == "" && compressed == 0 && content == 0
	}
	return *key != "" && len(digest) == 64 && compressed > 0 && content > 0 && content <= workspacesession.MaxExecOutputBytes
}
func (s *session) CreateReportedActionRun(ctx context.Context, arg CreateReportedActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.ID == "" || arg.CreatedAt.IsZero() || arg.FencingToken < 1 || !validReportedActionRun(arg.Status, arg.Reason, arg.OutputObjectKey, arg.OutputSha256, arg.OutputBytes, arg.OutputContentBytes) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	arg.LeaseClock = s.leaseClockValue()
	row, err := s.queries.CreateReportedActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}
func (s *session) ApplyReportedActionRun(ctx context.Context, arg ApplyReportedActionRunInput) (ActionRun, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ActionRun{}, err
	}
	if arg.UpdatedAt.IsZero() || arg.ExpectedRevision < 1 || arg.FencingToken < 1 || !validReportedActionRun(arg.Status, arg.Reason, arg.OutputObjectKey, arg.OutputSha256, arg.OutputBytes, arg.OutputContentBytes) {
		return ActionRun{}, corestore.ErrInvalidPage
	}
	arg.LeaseClock = s.leaseClockValue()
	row, err := s.queries.ApplyReportedActionRun(ctx, arg)
	return s.actionRunMutation(row.ProjectActionRun, row.SubjectWorkItemID, updated(ctx, err))
}

func (s *session) TerminalRecording(ctx context.Context, project tracker.ProjectID, id string) (TerminalRecording, error) {
	row, err := s.queries.TerminalRecording(ctx, pgquery.TerminalRecordingParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return row, mapError(ctx, err)
}
func (s *session) RecentTerminalRecordings(ctx context.Context, project tracker.ProjectID, workspace, relay string, limit int) ([]TerminalRecording, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if relay != "" {
		rows, err := s.queries.RecentRelayTerminalRecordings(ctx, pgquery.RecentRelayTerminalRecordingsParams{OrganizationID: s.organization, ProjectID: project, WorkspaceID: workspace, RelaySessionID: relay, PageLimit: int32(limit)})
		return rows, mapError(ctx, err)
	}
	rows, err := s.queries.RecentTerminalRecordings(ctx, pgquery.RecentTerminalRecordingsParams{OrganizationID: s.organization, ProjectID: project, WorkspaceID: workspace, PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) AttachRelayRecording(ctx context.Context, arg AttachRelayRecordingInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.RecordingID == "" || arg.RecordingArtifact == "" {
		return corestore.ErrInvalidPage
	}
	return mapError(ctx, s.queries.AttachRelayRecording(ctx, arg))
}

func (s *session) actionRunMutation(row ActionRun, subject string, err error) (ActionRun, error) {
	if err != nil {
		return ActionRun{}, err
	}
	revision := row.Revision
	if err := s.recordBoardDelta(row.ProjectID, viewstore.BoardDelta{WorkItemID: tracker.NativeWorkItemID(subject), Topic: "action_run", EntityID: row.ID, EntityRevision: &revision, Kind: "upsert", Status: row.Status, RelatedID: row.WorkspaceID}); err != nil {
		return ActionRun{}, err
	}
	return row, nil
}

func (s *session) DeleteActionRun(ctx context.Context, project tracker.ProjectID, id string, revision int64) error {
	if id == "" || revision < 1 {
		return corestore.ErrInvalidPage
	}
	var current int64
	var subject string
	err := s.db.QueryRow(ctx, `DELETE FROM project_action_runs a USING workspace_sessions w WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id=$3 AND a.revision=$4 AND w.organization_id=a.organization_id AND w.project_id=a.project_id AND w.id=a.workspace_id RETURNING a.revision,w.subject_work_item_id`, s.organization, project, id, revision).Scan(&current, &subject)
	if err := updated(ctx, err); err != nil {
		return err
	}
	return s.recordBoardDelta(project, viewstore.BoardDelta{WorkItemID: tracker.NativeWorkItemID(subject), Topic: "action_run", EntityID: id, EntityRevision: &current, Kind: "delete"})
}

func (s *session) leaseClockValue() *time.Time {
	if s.leaseClock == nil {
		return nil
	}
	return new(s.leaseClock().UTC())
}
