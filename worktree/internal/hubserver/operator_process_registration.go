package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
)

func registerSessionWorkerProcess(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, record runnersession.Record, snapshot runnersession.Snapshot, history attachment.ArchiveReference, fence leasestore.Fence) error {
	if snapshot.Worker == nil || snapshot.Worker.PID == 0 {
		return nil
	}
	writer, ok := w.(interface {
		RegisterSessionWorkerProcess(context.Context, postgres.ProcessReceipt, time.Time) (postgres.ProcessReceipt, error)
	})
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	clock, ok := w.(runnersession.Reader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	at, err := clock.RunnerSessionTime(ctx)
	if err != nil {
		return err
	}
	process, runner := snapshot.Worker, scope.Credential.Runner
	_, err = writer.RegisterSessionWorkerProcess(ctx, postgres.ProcessReceipt{
		OrganizationID: scope.Organization, ProjectID: scope.Project, SessionID: record.ID, Role: "agent",
		RunnerID: runner.RunnerID, MachineID: string(runner.MachineID), LeaseID: fence.LeaseID, FencingToken: fence.FencingToken,
		AuthorityHash: snapshot.Authority, Pid: int32(process.PID), GroupID: int32(process.GroupID), StartedAt: process.StartedAt, StartedAtNs: process.StartedAt.UnixNano(),
		CleanupRoot: process.CleanupRoot, CleanupPath: process.CleanupPath, HistoryRevision: record.Revision,
		RegistrationObjectKey: history.Key, RegistrationSha256: history.SHA256, RegistrationBytes: history.CompressedSize, RegistrationContentBytes: history.UncompressedSize,
	}, at)
	return err
}
