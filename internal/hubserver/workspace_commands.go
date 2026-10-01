package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeOperation(scope nativeScope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + suffix
}

func (s *Service) commandCreateWorkspace(ctx context.Context, scope nativeScope, request workspaceRequest) (json.RawMessage, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	request, err = normalizeWorkspaceRequest(request)
	if err != nil {
		return nil, err
	}
	var created workspaceRecord
	value, err := s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/workspaces"), Feature: "collaboration"}, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		record, err := service.openWorkspace(ctx, tx, scope, request, now)
		if err != nil {
			return nil, err
		}
		created = record
		return record.resource(), nil
	})
	if created.ID != "" {
		service.committed(ctx, created)
	}
	return value, err
}

func (s *Service) commandCreateAction(ctx context.Context, scope nativeScope, request projectActionRequest) (json.RawMessage, error) {
	return s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/actions"), Feature: "collaboration"}, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.applyCreateProjectAction(ctx, tx, scope, request, now)
	})
}
func (s *Service) commandPatchAction(ctx context.Context, scope nativeScope, id string, request projectActionPatch) (json.RawMessage, error) {
	return s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: nativeOperation(scope, "PATCH", "/actions/"+url.PathEscape(id)), Feature: "collaboration"}, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.applyPatchProjectAction(ctx, tx, scope, id, request, now)
	})
}
func (s *Service) commandRunAction(ctx context.Context, scope nativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {
	return s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/actions/"+url.PathEscape(id)+"/runs"), Feature: "collaboration"}, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.applyCreateProjectActionRun(ctx, tx, scope, id, request, now)
	})
}
func (s *Service) commandDeleteAction(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	err := s.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
		if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
			return err
		}
		return deleteProjectActionRow(ctx, tx, scope, id)
	})
	return json.RawMessage(`{"deleted":true}`), err
}

func (s *Service) readAttachment(ctx context.Context, scope nativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	record, err := s.conversations.loadConversation(ctx, s.database.db, scope, conversationID)
	if err != nil {
		return conversationAttachmentRecord{}, nil, err
	}
	if err := conversation.ValidateAttachmentID(id); err != nil {
		return conversationAttachmentRecord{}, nil, nativeNotFound()
	}
	attachment, err := s.conversations.store.readAttachment(ctx, s.database.db, record.ID, id)
	if err != nil {
		return conversationAttachmentRecord{}, nil, translateConversationError(err)
	}
	content, err := s.conversations.store.readAttachmentContent(ctx, s.database.db, attachment.ArtifactRef)
	return attachment, content, translateConversationError(err)
}

func (s *Service) readRecording(ctx context.Context, scope nativeScope, workspaceID, id string) (terminalRecordingRecord, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	workspace, err := service.readWorkspaceForActor(ctx, s.database.db, scope, workspaceID)
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	recording, err := readTerminalRecordingByID(ctx, s.database.db, workspace.ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return terminalRecordingRecord{}, nativeNotFound()
	}
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	if !terminalRecordingReadable(scope, recording) {
		return terminalRecordingRecord{}, nativeNotFound()
	}
	return recording, nil
}
func (s *Service) readRecordings(ctx context.Context, scope nativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {
	service, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	workspace, err := service.readWorkspaceForActor(ctx, s.database.db, scope, workspaceID)
	if err != nil {
		return nil, err
	}
	stored, err := readTerminalRecordings(ctx, s.database.db, workspace.ID, relaySessionID)
	if err != nil {
		return nil, err
	}
	result := []TerminalRecording{}
	for _, record := range stored {
		if terminalRecordingReadable(scope, record) {
			result = append(result, record.resource())
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

// runner grants are required for headless execution too. A token retains the
// dashboard's existing non-person restrictions on interactive terminals.
func (s *Service) requireOperatorRunners(ctx context.Context, query nativeQueryer, scope nativeScope) error {
	if scope.credential.Hosted != nil && !hostedRunnerGrants(ctx, query, string(scope.organization), scope.credential.Hosted.Subject, []tracker.ProjectID{scope.project}) {
		return nativeNotFound()
	}
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin {
		return nativeNotFound()
	}
	return nil
}
