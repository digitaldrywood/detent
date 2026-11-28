package workspacerelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (w *HTTP) ReadOperatorFiles(ctx context.Context, scope hubscope.NativeScope, name string, request operatortool.WorkspaceArguments) (json.RawMessage, error) {
	relative, err := workspacesession.NormalizePath(request.Path)
	if err != nil || request.Path == "/" || name == "workspace_file_read" && relative == "" {
		return nil, operatortool.ErrInvalidArguments
	}
	if err := w.RequireWorkspaces(); err != nil {
		return nil, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.RequestBudget)
		defer cancel()
	}
	record, err := w.ReadOperatorWorkspace(ctx, scope, request.WorkspaceID)
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
		}{status, code, record.ID, record.State, value})
	}
	if !workspacesession.Bound(record.State) {
		return result("unavailable", workspacesession.CodeWorkspaceClosed, nil)
	}
	if !w.ChannelPermitted(record, workspacesession.ChannelFiles) {
		return result("unsupported_transport", workspacesession.CodeForbidden, nil)
	}
	runner := w.Relay.RunnerFor(record.ID)
	if runner == nil {
		return result("unavailable", workspacesession.CodeStaleExecution, nil)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	connection := &Connection{
		ID: hubapi.NewID("relayperson"), WorkspaceID: record.ID,
		PrincipalID: identity.PrincipalID, SessionID: identity.SessionID,
		SessionHash: scope.Credential.SessionHash, HostedRole: scope.Credential.HostedRole,
		Out: make(chan Outbound, WriteQueue), Done: make(chan struct{}),
	}
	principal := Principal{Credential: scope.Credential, PrincipalID: identity.PrincipalID, SessionID: identity.SessionID, Subject: scope.Credential.Name}
	if scope.Credential.Hosted != nil {
		connection.Subject = scope.Credential.Hosted.Subject
		principal.Subject = connection.Subject
		principal.SupportReason = scope.Credential.Hosted.SupportReason
	}
	connection.Actor = w.RelayActor(ctx, scope.Organization, connection, principal)
	if err := w.OpenRelayAudit(ctx, record, connection, principal); err != nil {
		return nil, err
	}
	defer w.CloseRelayAudit(ctx, record, connection, "person_closed")
	if err := w.Relay.AttachPerson(connection); err != nil {
		return nil, workspacehub.ErrOperationUnavailable
	}
	defer func() {
		if stream := w.Relay.ChannelStream(connection, workspacesession.ChannelFiles); stream != nil {
			w.Relay.CloseStream(ctx, record.ID, stream.ID)
			runner.Send(workspacesession.Frame{Channel: stream.Channel, Stream: stream.ID, Type: workspacesession.TypeClose})
		}
		connection.Close("person_closed")
		w.Relay.DetachPerson(ctx, connection, w.now())
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
	w.HandlePersonFrame(ctx, connection, record, workspacesession.Frame{Channel: workspacesession.ChannelFiles, Type: frameType, Payload: payload})
	select {
	case <-ctx.Done():
		return nil, workspacehub.ErrOperationUnavailable
	case <-runner.Done:
		return result("unavailable", workspacesession.CodeStaleExecution, nil)
	case <-connection.Done:
		return nil, workspacehub.ErrOperationUnavailable
	case response := <-connection.Out:
		if ctx.Err() != nil {
			return nil, workspacehub.ErrOperationUnavailable
		}
		ctx, err = operatortool.AuthorizeCurrent(ctx, workspacehub.OperatorRequirement(name, request, false))
		if err != nil {
			return nil, operatortool.ErrAccessDenied
		}
		if err := w.RevalidatePersonLocally(ctx, record, connection); err != nil {
			return result("unavailable", workspacesession.CodeStaleExecution, nil)
		}
		live, err := w.ReadOperatorWorkspace(ctx, scope, record.ID)
		if err != nil || live.LeaseID != record.LeaseID || live.FencingToken != record.FencingToken || live.RunnerID != record.RunnerID || live.AttemptID != record.AttemptID || live.SubjectWorkItemID != record.SubjectWorkItemID || !w.ChannelPermitted(live, workspacesession.ChannelFiles) {
			return result("unavailable", workspacesession.CodeStaleExecution, nil)
		}
		frame := response.Frame
		if frame.Type == workspacesession.TypeError {
			var refusal workspacesession.ErrorPayload
			if json.Unmarshal(frame.Payload, &refusal) != nil {
				return nil, workspacehub.ErrOperationUnavailable
			}
			switch refusal.Code {
			case workspacesession.CodeNotFound, workspacesession.CodeForbidden, workspacesession.CodeDenied, workspacesession.CodeTooLarge,
				workspacesession.CodeStaleExecution, workspacesession.CodeWorkspaceClosed, workspacesession.CodeStreamLimit, workspacesession.CodeRelayBusy:
			default:
				return nil, workspacehub.ErrOperationUnavailable
			}
			return result("unavailable", refusal.Code, nil)
		}
		if response.Final != "" || frame.Channel != workspacesession.ChannelFiles || frame.Type != responseType {
			return nil, workspacehub.ErrOperationUnavailable
		}
		if name == "workspace_file_list" {
			var listed workspacesession.FilesListed
			if json.Unmarshal(frame.Payload, &listed) != nil || listed.Path != relative || len(listed.Entries) > workspacesession.DirectoryPage || len(listed.NextCursor) > workspacesession.MaxPathBytes {
				return nil, workspacehub.ErrOperationUnavailable
			}
			for _, entry := range listed.Entries {
				clean, err := workspacesession.NormalizePath(entry.Name)
				if err != nil || clean == "" || clean != entry.Name || strings.Contains(clean, "/") {
					return nil, workspacehub.ErrOperationUnavailable
				}
			}
			return result("available", "", listed)
		}
		var content workspacesession.FilesContent
		if json.Unmarshal(frame.Payload, &content) != nil || content.Path != relative || content.Offset != files.Offset || content.Size < content.Offset {
			return nil, workspacehub.ErrOperationUnavailable
		}
		data := []byte(content.Data)
		switch content.Encoding {
		case "":
		case "base64":
			data, err = base64.StdEncoding.DecodeString(content.Data)
		default:
			return nil, workspacehub.ErrOperationUnavailable
		}
		if err != nil || len(data) > length || int64(len(data)) > content.Size-content.Offset {
			return nil, workspacehub.ErrOperationUnavailable
		}
		return result("available", "", content)
	}
}
