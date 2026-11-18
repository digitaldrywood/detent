package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) RegisterSessionWorkerProcess(ctx context.Context, receipt ProcessReceipt, at time.Time) (ProcessReceipt, error) {
	if err := s.checkOrganization(receipt.OrganizationID); err != nil {
		return ProcessReceipt{}, err
	}
	if receipt.Role != "agent" {
		return ProcessReceipt{}, corestore.ErrInvalidPage
	}
	owner, err := s.runnerIdentityRow(ctx, `runner_id=$2`, receipt.RunnerID)
	if err != nil {
		return ProcessReceipt{}, err
	}
	if string(owner.MachineID) != receipt.MachineID {
		return ProcessReceipt{}, corestore.ErrRevisionConflict
	}
	receipt.EnrollmentID = owner.EnrollmentID
	registered, err := s.RegisterWorkerProcess(ctx, receipt, at)
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		return registered, err
	}
	registered, err = s.queries.ReplaceSessionWorkerProcess(ctx, pgquery.ReplaceSessionWorkerProcessParams{
		OrganizationID: receipt.OrganizationID, ProjectID: receipt.ProjectID, SessionID: receipt.SessionID,
		RunnerID: receipt.RunnerID, EnrollmentID: receipt.EnrollmentID, MachineID: receipt.MachineID,
		LeaseID: receipt.LeaseID, FencingToken: receipt.FencingToken, AuthorityHash: receipt.AuthorityHash,
		Pid: receipt.Pid, GroupID: receipt.GroupID, StartedAt: receipt.StartedAt, StartedAtNs: receipt.StartedAtNs,
		CleanupRoot: receipt.CleanupRoot, CleanupPath: receipt.CleanupPath, HistoryRevision: receipt.HistoryRevision,
		RegistrationObjectKey: receipt.RegistrationObjectKey, RegistrationSha256: receipt.RegistrationSha256,
		RegistrationBytes: receipt.RegistrationBytes, RegistrationContentBytes: receipt.RegistrationContentBytes, At: at,
	})
	return registered, updated(ctx, err)
}
