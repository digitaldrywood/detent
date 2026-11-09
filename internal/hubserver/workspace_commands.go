package hubserver

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeOperation(scope nativeScope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + suffix
}

func (s *Service) commandCreateWorkspace(ctx context.Context, scope nativeScope, request workspaceRequest) (json.RawMessage, error) {

	return s.createWorkspaceRecord(ctx, scope, request)

}

func (s *Service) commandCreateAction(ctx context.Context, scope nativeScope, request projectActionRequest) (json.RawMessage, error) {

	return s.mutateActionRecord(ctx, scope, "POST", "", request.Mutation, request)

}
func (s *Service) commandPatchAction(ctx context.Context, scope nativeScope, id string, request projectActionPatch) (json.RawMessage, error) {

	return s.mutateActionRecord(ctx, scope, "PATCH", id, request.Mutation, request)

}
func (s *Service) commandRunAction(ctx context.Context, scope nativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {

	return s.createActionRunRecord(ctx, scope, id, request)

}
func (s *Service) commandDeleteAction(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {

	return s.mutateActionRecord(ctx, scope, "DELETE", id, tracker.Mutation{}, nil)

}

func (s *Service) readAttachment(ctx context.Context, scope nativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	return s.readAttachmentPG(ctx, scope, conversationID, id)
}

func (s *Service) readRecording(ctx context.Context, scope nativeScope, workspaceID, id string) (terminalRecordingRecord, error) {

	return s.readTerminalRecordingRecord(ctx, scope, workspaceID, id)

}
func (s *Service) readRecordings(ctx context.Context, scope nativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {

	return s.readTerminalRecordingRecords(ctx, scope, workspaceID, relaySessionID, limit)

}

// runner grants are required for headless execution too. A token retains the
// dashboard's existing non-person restrictions on interactive terminals.
func (s *Service) requireOperatorRunners(ctx context.Context, _ nativeQueryer, scope nativeScope) error {
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin {
		return nativeNotFound()
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	return s.accessRead(ctx, scope.organization, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		grant, err := reader.AccessProjectGrant(ctx, scope.credential.Hosted.Subject, scope.project)
		if err != nil {
			return err
		}
		if !grant.ManageRunner {
			return nativeNotFound()
		}
		return nil
	})
}
