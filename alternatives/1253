package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
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
	rows, err := s.database.reader.QueryContext(ctx, `SELECT operator_action_agent,operator_action_id,query_metadata_version,data_json FROM conversation_messages WHERE conversation_id=? AND (operator_action_id=? OR query_metadata_version=0) ORDER BY rowid DESC LIMIT ?`, action.ConversationID, action.ID, storageConversionRows+1)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	processed := 0
	for rows.Next() {
		processed++
		if processed > storageConversionRows {
			return "", errors.New("conversation action metadata conversion is pending")
		}
		var agent, id sql.NullString
		var version int
		var raw string
		if err := rows.Scan(&agent, &id, &version, &raw); err != nil {
			return "", err
		}
		if version == 0 {
			var data queryMessageData
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				return "", err
			}
			if data.OperatorAction == nil || data.OperatorAction.Action == nil || data.OperatorAction.Action.ID == nil || *data.OperatorAction.Action.ID != action.ID {
				continue
			}
			if data.OperatorAction.Action.Agent != nil {
				agent.String = *data.OperatorAction.Action.Agent
			}
		}
		if agent.String == "" {
			return coordinatorAgentName, nil
		}
		return agent.String, nil
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return coordinatorAgentName, nil
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
