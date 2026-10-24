package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/chat"
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
	var agent sql.NullString
	err := s.database.reader.QueryRowContext(ctx, `SELECT json_extract(data_json, '$.operator_action.action.agent') FROM conversation_messages
WHERE conversation_id=? AND json_extract(data_json, '$.operator_action.action.id')=? ORDER BY rowid DESC LIMIT 1`, action.ConversationID, action.ID).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) || err == nil && agent.String == "" {
		return coordinatorAgentName, nil
	}
	if err != nil {
		return "", err
	}
	return agent.String, nil
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
