package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversationWorkerIdentity struct {
	Credential   accessstore.Credential
	Runner       runnerauth.Identity
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
}

type ConversationWorkerAuthority interface {
	VerifyConversationWorker(context.Context, tenantstore.Reader, ConversationWorkerIdentity) error
}
