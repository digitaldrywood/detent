package attemptworkers

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Read(ctx context.Context, repository tenantstore.Repository, scope hubscope.NativeScope, machine tracker.MachineID, after []string, clock func() time.Time) (runnerauth.AttemptWorkerInventory, error) {
	if len(after) != 1 || after[0] != "" && !hubapi.ValidID(after[0], "attempt") {
		return runnerauth.AttemptWorkerInventory{}, hubapi.NativePublicInvalid("Invalid attempt worker cursor")
	}
	credential := scope.Credential
	if scope.Project == "" || credential.ID == "" || credential.Runner.RunnerID == "" || credential.Runner.OrganizationID != scope.Organization || credential.Runner.MachineID != machine {
		return runnerauth.AttemptWorkerInventory{}, hubapi.NativeNotFound()
	}
	var page runnerauth.AttemptWorkerPage
	snapshot, ok := repository.(tenantstore.SnapshotRepository)
	if !ok {
		return runnerauth.AttemptWorkerInventory{}, hubapi.NativeRunEventStorageUnavailable()
	}
	if err := snapshot.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		now := clock().UTC()
		identity, err := AuthorizeRunnerProjectOperation(ctx, r, scope, credential.Runner.RunnerID, runnerauth.Heartbeat, now)
		if err != nil {
			return err
		}
		reader, ok := r.(runnerstore.AttemptWorkerReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		page, err = reader.RunnerAttemptWorkers(ctx, identity.RunnerID, identity.MachineID, scope.Project, after[0], runnerauth.AttemptWorkerPageSize, now)
		return err
	}); err != nil {
		return runnerauth.AttemptWorkerInventory{}, err
	}
	return runnerauth.AttemptWorkerInventory{RunnerID: credential.Runner.RunnerID, Page: &page}, nil
}

func ValidateAcknowledgment(request tracker.NativeRunEvent) error {
	data := request.Data
	identity := tracker.NativeRunData{AttemptID: data.AttemptID, LeaseID: data.LeaseID, FencingToken: data.FencingToken}
	if request.Type != "run.finished" || request.Worker != nil || len(request.PriorEvents) != 0 || request.WorkerCleanup == nil ||
		!request.WorkerCleanup.Valid() || !hubapi.ValidID(data.AttemptID, "attempt") || data.LeaseID == "" || data.FencingToken <= 0 || !reflect.DeepEqual(data, identity) {
		return hubapi.NativeInvalid("Worker cleanup requires only the exact attempt, lease, fence and retired worker")
	}
	return nil
}

func Acknowledge(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, request tracker.NativeRunEvent, clock func() time.Time, write func(context.Context, hubscope.NativeScope, func(context.Context, tenantstore.Writer) error) error) error {
	if scope.Credential.Runner.RunnerID == "" || scope.Credential.EntryKey != nil {
		return hubscope.RunnerUnauthorized()
	}
	return write(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		identity, err := AuthorizeRunnerProjectOperation(ctx, w, scope, scope.Credential.Runner.RunnerID, runnerauth.Events, clock().UTC())
		if err != nil {
			return err
		}
		writer, ok := w.(leasestore.AttemptWorkerCleanupWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		return writer.AcknowledgeAttemptWorker(ctx, leasestore.AttemptWorkerCleanup{
			ProjectID: scope.Project, WorkItemID: item, AttemptID: request.Data.AttemptID, RunnerID: identity.RunnerID, MachineID: identity.MachineID,
			Fence: leasestore.Fence{LeaseID: string(request.Data.LeaseID), FencingToken: int64(request.Data.FencingToken)}, Worker: *request.WorkerCleanup,
		})
	})
}

func AuthorizeRunnerProjectOperation(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, runner string, operation string, now time.Time) (runnerstore.Identity, error) {
	identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if identity.OrganizationID != scope.Organization || identity.RunnerID != runner || identity.TokenID != scope.Credential.ID || identity.MachineID != scope.Credential.Runner.MachineID || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, operation) {
		return runnerstore.Identity{}, hubscope.RunnerUnauthorized()
	}
	credential, err := r.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) {
		return runnerstore.Identity{}, hubscope.RunnerUnauthorized()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if !allowed {
		return runnerstore.Identity{}, hubapi.NativeNotFound()
	}
	return identity, nil
}
