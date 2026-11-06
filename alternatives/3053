package hubserver

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type tenantRunnerAuthority struct {
	now func() time.Time
}

func (a tenantRunnerAuthority) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
	if a.now == nil || worker.Organization == "" || worker.Project == "" || worker.Runner.RunnerID == "" || !worker.Runner.Valid() {
		return conversationWorkerAuthorityUnavailable()
	}
	identity, err := reader.RunnerIdentityByCredential(ctx, worker.Credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return accessstore.ErrDenied
		}
		return err
	}
	if identity.OrganizationID != worker.Organization || identity.RunnerID != worker.Runner.RunnerID || identity.MachineID != worker.Runner.MachineID || identity.Scope != worker.Runner.Scope || !slices.Equal(identity.Operations, worker.Runner.Operations) || identity.RemovedAt != nil {
		return accessstore.ErrDenied
	}
	credential, err := reader.AccessCredential(ctx, identity.TokenID)
	if err != nil {
		return err
	}
	now := a.now()
	if credential.OrganizationID != worker.Organization || credential.ID != worker.Credential.ID || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(worker.Credential.Hash, now) {
		return accessstore.ErrDenied
	}
	if !slices.Contains(identity.Operations, runnerauth.Claim) {
		return accessstore.ErrDenied
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, worker.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return nativeNotFound()
	}
	return nil
}

var _ ConversationWorkerAuthority = tenantRunnerAuthority{}
