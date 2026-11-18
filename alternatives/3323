package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProcessReceipt = pgquery.RunnerProcessReceipt
type OperatorProcess = pgquery.OperatorWorkerProcessesRow
type OperatorProcessList = pgquery.OperatorWorkerProcessesParams
type ProcessReap struct {
	Owner          ProcessReceipt
	Classification string
	Outcome        string
	Reason         string
	At             time.Time
	Receipt        attachment.ArchiveReference
}
type OperatorProcessReader interface {
	runnersession.Reader
	ListActiveWorkerProcesses(context.Context, OperatorProcessList) ([]OperatorProcess, error)
}
type OperatorProcessWriter interface {
	OperatorProcessReader
	runnersession.Writer
	RegisterWorkerProcess(context.Context, ProcessReceipt, time.Time) (ProcessReceipt, error)
	AdoptWorkerProcessReceipt(context.Context, ProcessReceipt, time.Time) (ProcessReceipt, error)
	MarkSessionWorkerProcessReaped(context.Context, ProcessReap) (ProcessReceipt, error)
}

func (r *Repository) ReadOperatorProcesses(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(OperatorProcessReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteOperatorProcesses(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(OperatorProcessWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) ListActiveWorkerProcesses(ctx context.Context, arg OperatorProcessList) ([]OperatorProcess, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if arg.ProjectID == "" || arg.RunnerID == "" || arg.EnrollmentID == "" || arg.MachineID == "" || arg.Now.IsZero() || arg.AfterSession < 0 {
		return nil, corestore.ErrInvalidPage
	}
	if err := checkPage(int(arg.PageLimit)); err != nil {
		return nil, err
	}
	rows, err := s.queries.OperatorWorkerProcesses(ctx, arg)
	return rows, mapError(ctx, err)
}
func sameProcessIdentity(left, right ProcessReceipt) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.SessionID == right.SessionID && left.Role == right.Role && left.RunnerID == right.RunnerID && left.EnrollmentID == right.EnrollmentID && left.MachineID == right.MachineID && left.LeaseID == right.LeaseID && left.FencingToken == right.FencingToken && left.AuthorityHash == right.AuthorityHash && left.Pid == right.Pid && left.GroupID == right.GroupID && left.StartedAtNs == right.StartedAtNs
}
func (s *session) RegisterWorkerProcess(ctx context.Context, row ProcessReceipt, at time.Time) (ProcessReceipt, error) {
	return s.registerProcessReceipt(ctx, row, at, false)
}
func (s *session) AdoptWorkerProcessReceipt(ctx context.Context, row ProcessReceipt, at time.Time) (ProcessReceipt, error) {
	return s.registerProcessReceipt(ctx, row, at, true)
}
func (s *session) registerProcessReceipt(ctx context.Context, row ProcessReceipt, at time.Time, recovery bool) (ProcessReceipt, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return ProcessReceipt{}, err
	}
	if row.StartedAtNs == 0 && !row.StartedAt.IsZero() {
		row.StartedAtNs = row.StartedAt.UnixNano()
	}
	if row.SessionID < 1 || row.ProjectID == "" || row.Role != "agent" && row.Role != "host_validation" || row.RunnerID == "" || row.EnrollmentID == "" || row.MachineID == "" || row.LeaseID == "" || row.FencingToken < 1 || row.Pid < 1 || row.GroupID < 0 || row.StartedAt.IsZero() || row.StartedAtNs < 1 || row.StartedAt.UnixNano() != row.StartedAtNs || row.HistoryRevision < 1 || at.IsZero() || !artifact.ValidHash(row.AuthorityHash, 64) || eventstore.Reference(row.RegistrationObjectKey, row.RegistrationSha256, row.RegistrationBytes, row.RegistrationContentBytes).Validate(string(s.organization)) != nil {
		return ProcessReceipt{}, corestore.ErrInvalidPage
	}
	source, err := s.queries.ValidateWorkerProcessSource(ctx, pgquery.ValidateWorkerProcessSourceParams{OrganizationID: s.organization, ProjectID: row.ProjectID, SessionID: row.SessionID, HistoryRevision: row.HistoryRevision, RegistrationObjectKey: row.RegistrationObjectKey, RegistrationSha256: row.RegistrationSha256, RegistrationBytes: row.RegistrationBytes, RegistrationContentBytes: row.RegistrationContentBytes})
	if err != nil {
		return ProcessReceipt{}, mapError(ctx, err)
	}
	if source.RunnerID != row.RunnerID || source.EnrollmentID != row.EnrollmentID || source.MachineID != row.MachineID || source.LeaseID != row.LeaseID || source.FencingToken != row.FencingToken {
		return ProcessReceipt{}, corestore.ErrRevisionConflict
	}
	current, err := s.queries.OperatorWorkerProcess(ctx, pgquery.OperatorWorkerProcessParams{OrganizationID: s.organization, ProjectID: row.ProjectID, SessionID: row.SessionID, Role: row.Role})
	if err == nil {
		if sameProcessIdentity(current, row) && current.RegistrationSha256 == row.RegistrationSha256 {
			return current, nil
		}
		if current.ReapedAt == nil {
			return ProcessReceipt{}, corestore.ErrRevisionConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ProcessReceipt{}, mapError(ctx, err)
	}
	live := source.CompletedAt == nil && source.ReleasedAt == nil && source.ExpiresAt.After(at) && source.Status == "running"
	if !recovery && !live {
		return ProcessReceipt{}, corestore.ErrRevisionConflict
	}
	if recovery && (live || row.Role != "agent" || source.WorkerPid != row.Pid || source.WorkerGroupID != row.GroupID || source.WorkerStartedAt == nil || !source.WorkerStartedAt.Equal(row.StartedAt.Truncate(time.Microsecond))) {
		return ProcessReceipt{}, corestore.ErrRevisionConflict
	}
	row.UpdatedAt = at
	registered, err := s.queries.RegisterWorkerProcess(ctx, pgquery.RegisterWorkerProcessParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, SessionID: row.SessionID, Role: row.Role, RunnerID: row.RunnerID, EnrollmentID: row.EnrollmentID, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, AuthorityHash: row.AuthorityHash, Pid: row.Pid, GroupID: row.GroupID, StartedAt: row.StartedAt, StartedAtNs: row.StartedAtNs, CleanupRoot: row.CleanupRoot, CleanupPath: row.CleanupPath, HistoryRevision: row.HistoryRevision, RegistrationObjectKey: row.RegistrationObjectKey, RegistrationSha256: row.RegistrationSha256, RegistrationBytes: row.RegistrationBytes, RegistrationContentBytes: row.RegistrationContentBytes, UpdatedAt: row.UpdatedAt})
	if err != nil {
		return ProcessReceipt{}, updated(ctx, err)
	}
	if row.Role == "agent" {
		count, err := s.queries.ProjectWorkerProcessRegistration(ctx, pgquery.ProjectWorkerProcessRegistrationParams{OrganizationID: s.organization, ProjectID: row.ProjectID, ID: row.SessionID, WorkerPid: row.Pid, WorkerGroupID: row.GroupID, WorkerStartedAt: new(row.StartedAt)})
		if err := affected(ctx, count, err); err != nil {
			return ProcessReceipt{}, err
		}
	}
	return registered, nil
}
func (s *session) MarkSessionWorkerProcessReaped(ctx context.Context, arg ProcessReap) (ProcessReceipt, error) {
	owner := arg.Owner
	if err := s.checkOrganization(owner.OrganizationID); err != nil {
		return ProcessReceipt{}, err
	}
	if arg.Classification != "committed" && arg.Classification != "rejected" && arg.Classification != "uncertain" || arg.At.IsZero() || strings.TrimSpace(arg.Outcome) == "" || arg.Receipt.Validate(string(s.organization)) != nil || owner.OwnerGeneration < 1 || owner.StartedAtNs < 1 {
		return ProcessReceipt{}, corestore.ErrInvalidPage
	}
	current, err := s.queries.OperatorWorkerProcess(ctx, pgquery.OperatorWorkerProcessParams{OrganizationID: s.organization, ProjectID: owner.ProjectID, SessionID: owner.SessionID, Role: owner.Role})
	if err != nil {
		return ProcessReceipt{}, mapError(ctx, err)
	}
	if !sameProcessIdentity(current, owner) || current.OwnerGeneration != owner.OwnerGeneration {
		return ProcessReceipt{}, corestore.ErrRevisionConflict
	}
	if current.ReapedAt != nil {
		if arg.Classification != "committed" || current.ReapSha256 != arg.Receipt.SHA256 || current.ReapOutcome != arg.Outcome {
			return ProcessReceipt{}, corestore.ErrRevisionConflict
		}
		return current, nil
	}
	result, err := s.queries.MarkWorkerProcessReaped(ctx, pgquery.MarkWorkerProcessReapedParams{OrganizationID: s.organization, ProjectID: owner.ProjectID, SessionID: owner.SessionID, Role: owner.Role, OwnerGeneration: owner.OwnerGeneration, AuthorityHash: owner.AuthorityHash, RunnerID: owner.RunnerID, EnrollmentID: owner.EnrollmentID, MachineID: owner.MachineID, LeaseID: owner.LeaseID, FencingToken: owner.FencingToken, Pid: owner.Pid, GroupID: owner.GroupID, StartedAt: time.Unix(0, owner.StartedAtNs).UTC(), StartedAtNs: owner.StartedAtNs, ReapClass: arg.Classification, At: arg.At, ReapOutcome: arg.Outcome, ReapReason: arg.Reason, ReapObjectKey: new(arg.Receipt.Key), ReapSha256: arg.Receipt.SHA256, ReapBytes: arg.Receipt.CompressedSize, ReapContentBytes: arg.Receipt.UncompressedSize})
	if err != nil {
		return ProcessReceipt{}, updated(ctx, err)
	}
	if owner.Role == "agent" && arg.Classification == "committed" {
		count, err := s.queries.ProjectWorkerProcessReap(ctx, pgquery.ProjectWorkerProcessReapParams{OrganizationID: s.organization, ProjectID: owner.ProjectID, ID: owner.SessionID, WorkerPid: owner.Pid, WorkerGroupID: owner.GroupID, WorkerStartedAt: new(time.Unix(0, owner.StartedAtNs).UTC()), WorkerReapedAt: new(arg.At)})
		if err := affected(ctx, count, err); err != nil {
			return ProcessReceipt{}, err
		}
	}
	return result, nil
}
