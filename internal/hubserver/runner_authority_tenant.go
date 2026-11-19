package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type tenantRunnerAuthority struct {
	now func() time.Time
}

func (a tenantRunnerAuthority) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
	return runnerfleet.VerifyConversationWorker(ctx, reader, worker, a.now)
}

var _ ConversationWorkerAuthority = tenantRunnerAuthority{}
