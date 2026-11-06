package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coordinatorAgentName = "Luna"

func coordinatorAgent(model string) string {
	if model == "" {
		return coordinatorAgentName
	}
	return coordinatorAgentName + " (" + model + ")"
}

// coordinatorProposalAgent reads the agent from the proposal the coordinator
// stored, never from the submitted action, so a client cannot relabel who
// acted.
func (s *Service) coordinatorProposalAgent(ctx context.Context, action chat.Action) (string, error) {
	agent := coordinatorAgentName
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(action.OrganizationID), corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: action.ConversationID, OperatorActionID: action.ID, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Items) > 0 && page.Items[0].OperatorAgent != nil && *page.Items[0].OperatorAgent != "" {
			agent = *page.Items[0].OperatorAgent
		}
		return nil
	})
	return agent, err
}

// withCoordinatorAgent attributes application writes made under ctx to the
// agent acting for the credential's principal.
func withCoordinatorAgent(ctx context.Context, agent string) context.Context {
	if resolve, ok := ctx.Value(hubOperatorResolverKey{}).(func(context.Context) (apiCredential, error)); ok {
		ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(ctx context.Context) (apiCredential, error) {
			credential, err := resolve(ctx)
			credential.Agent = agent
			return credential, err
		})
	}
	if resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error)); ok {
		ctx = context.WithValue(ctx, nativeOperatorScopeKey{}, func(ctx context.Context) (nativeScope, error) {
			scope, err := resolve(ctx)
			scope.credential.Agent = agent
			return scope, err
		})
	}
	return ctx
}
