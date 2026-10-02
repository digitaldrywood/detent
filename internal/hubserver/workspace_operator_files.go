package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) readWorkspaceFilesTool(ctx context.Context, scope nativeScope, name string, request workspaceToolRequest) (json.RawMessage, error) {
	relative, err := workspacesession.NormalizePath(request.Path)
	if err != nil || request.Path == "/" || name == "workspace_file_read" && relative == "" {
		return nil, operatortool.ErrInvalidArguments
	}
	w, err := s.requireWorkspaces()
	if err != nil {
		return nil, err
	}
	record, err := w.readWorkspaceForActor(ctx, s.database.db, scope, request.WorkspaceID)
	if err != nil {
		return nil, err
	}
	result := func(status, code string, value any) (json.RawMessage, error) {
		return json.Marshal(struct {
			Status      string `json:"status"`
			Code        string `json:"code,omitempty"`
			WorkspaceID string `json:"workspace_id"`
			State       string `json:"state"`
			Result      any    `json:"result,omitempty"`
		}{status, code, record.ID, string(record.State), value})
	}
	if !workspacesession.Bound(record.State) {
		return result("unavailable", workspacesession.CodeWorkspaceClosed, nil)
	}
	if !w.channelPermitted(record, workspacesession.ChannelFiles) {
		return result("unsupported_transport", workspacesession.CodeForbidden, nil)
	}
	runner := w.relay.runnerFor(record.ID)
	if runner == nil {
		return result("unavailable", workspacesession.CodeStaleExecution, nil)
	}
	ctx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	identity := operatortool.ConnectionIdentity(ctx)
	connection := &relayConnection{
		id: newNativeID("relayperson"), workspaceID: record.ID,
		principalID: identity.PrincipalID, sessionID: identity.SessionID,
		sessionHash: scope.credential.SessionHash, hostedRole: scope.credential.HostedRole,
		out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}),
	}
	principal := relayPrincipal{credential: scope.credential, principalID: identity.PrincipalID, sessionID: identity.SessionID, subject: scope.credential.Name}
	if scope.credential.Hosted != nil {
		connection.subject = scope.credential.Hosted.Subject
		principal.subject = connection.subject
		principal.supportReason = scope.credential.Hosted.SupportReason
	}
	connection.actor = s.relayActor(ctx, connection, principal)
	if err := w.openRelayAudit(ctx, record, connection, principal); err != nil {
		return nil, err
	}
	defer func() {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayWriteTimeout)
		defer cancel()
		w.closeRelayAudit(auditCtx, connection, "person_closed")
	}()
	if err := w.relay.attachPerson(connection); err != nil {
		return nil, errWorkspaceOperationUnavailable
	}
	defer func() {
		if stream := w.relay.channelStream(connection, workspacesession.ChannelFiles); stream != nil {
			w.relay.closeStream(ctx, record.ID, stream.id)
			runner.send(workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeClose})
		}
		connection.close("person_closed")
		w.relay.detachPerson(ctx, connection, w.now())
	}()
	length := request.Length
	if length == 0 {
		length = 32768
	}
	files := workspacesession.FilesRequest{Path: relative, Cursor: request.Cursor, Offset: int64(request.Offset), Length: int64(length), ShowIgnored: request.ShowIgnored}
	payload, err := workspacesession.Encode(files)
	if err != nil {
		return nil, err
	}
	frameType, responseType := workspacesession.TypeFilesList, workspacesession.TypeFilesListed
	if name == "workspace_file_read" {
		frameType, responseType = workspacesession.TypeFilesRead, workspacesession.TypeFilesContent
	}
	w.handlePersonFrame(ctx, connection, record, workspacesession.Frame{Channel: workspacesession.ChannelFiles, Type: frameType, Payload: payload})
	select {
	case <-ctx.Done():
		return nil, errWorkspaceOperationUnavailable
	case <-runner.done:
		return result("unavailable", workspacesession.CodeStaleExecution, nil)
	case <-connection.done:
		return nil, errWorkspaceOperationUnavailable
	case response := <-connection.out:
		ctx, err = operatortool.AuthorizeCurrent(ctx, workspaceRequirement(name, request, false))
		if err != nil {
			return nil, operatortool.ErrAccessDenied
		}
		if err := w.revalidatePersonLocally(ctx, record, connection); err != nil {
			return result("unavailable", workspacesession.CodeStaleExecution, nil)
		}
		live, err := readWorkspaceByID(ctx, s.database.db, record.ID)
		if err != nil || live.LeaseID != record.LeaseID || live.FencingToken != record.FencingToken || live.RunnerID != record.RunnerID || live.AttemptID != record.AttemptID || live.SubjectWorkItemID != record.SubjectWorkItemID || !w.channelPermitted(live, workspacesession.ChannelFiles) {
			return result("unavailable", workspacesession.CodeStaleExecution, nil)
		}
		frame := response.frame
		if frame.Type == workspacesession.TypeError {
			var refusal workspacesession.ErrorPayload
			if json.Unmarshal(frame.Payload, &refusal) != nil {
				return nil, errWorkspaceOperationUnavailable
			}
			switch refusal.Code {
			case workspacesession.CodeNotFound, workspacesession.CodeForbidden, workspacesession.CodeDenied, workspacesession.CodeTooLarge,
				workspacesession.CodeStaleExecution, workspacesession.CodeWorkspaceClosed, workspacesession.CodeStreamLimit, workspacesession.CodeRelayBusy:
			default:
				return nil, errWorkspaceOperationUnavailable
			}
			return result("unavailable", refusal.Code, nil)
		}
		if response.final != "" || frame.Channel != workspacesession.ChannelFiles || frame.Type != responseType {
			return nil, errWorkspaceOperationUnavailable
		}
		if name == "workspace_file_list" {
			var listed workspacesession.FilesListed
			if json.Unmarshal(frame.Payload, &listed) != nil || listed.Path != relative || len(listed.Entries) > workspacesession.DirectoryPage || len(listed.NextCursor) > workspacesession.MaxPathBytes {
				return nil, errWorkspaceOperationUnavailable
			}
			for _, entry := range listed.Entries {
				clean, err := workspacesession.NormalizePath(entry.Name)
				if err != nil || clean == "" || clean != entry.Name || strings.Contains(clean, "/") {
					return nil, errWorkspaceOperationUnavailable
				}
			}
			return result("available", "", listed)
		}
		var content workspacesession.FilesContent
		if json.Unmarshal(frame.Payload, &content) != nil || content.Path != relative || content.Offset != files.Offset || content.Size < content.Offset {
			return nil, errWorkspaceOperationUnavailable
		}
		data := []byte(content.Data)
		switch content.Encoding {
		case "":
		case "base64":
			data, err = base64.StdEncoding.DecodeString(content.Data)
		default:
			return nil, errWorkspaceOperationUnavailable
		}
		if err != nil || len(data) > length || int64(len(data)) > content.Size-content.Offset {
			return nil, errWorkspaceOperationUnavailable
		}
		return result("available", "", content)
	}
}
