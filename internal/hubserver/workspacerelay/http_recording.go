package workspacerelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Recording terminal streams from the frames the hub relays.
//
// The hub forwards the terminal channel like any other, and it also reads it.
// That is the same arrangement the project actions contract makes for an action run, and for
// the same reason: the record cannot be the client's, because the client is a
// tab that may be closed at any moment and because a recording assembled
// anywhere but here would be a recording of something other than what actually
// crossed the relay.

// beginTerminalRecording starts a recording for one terminal open, or answers
// nil when workspaces.terminal.record is off.
//
// The isolation stored is the level this organization's setting names, which is
// the level the runner was told to serve. A recording's audience is decided
// from it for the life of the row, so a setting changed afterwards cannot widen
// who may read a recording already made.
func (w *HTTP) beginTerminalRecording(
	record workspacehub.Record,
	connection *Connection,
	stream *Stream,
	open workspacesession.TerminalOpen,
) *TerminalStream {
	if !w.TerminalConfig().RecordEnabled() {
		return nil
	}
	now := w.now()
	cols, rows, err := workspacesession.NormalizeTerminalSize(open.Cols, open.Rows)
	if err != nil {
		// An illegal window is refused before it reaches a runner, so this is
		// only reached by an open the hub could not parse at all. The recording
		// still starts, at the conventional size, because a session that
		// happened is a session worth having a record of.
		cols, rows = workspacesession.DefaultTerminalCols, workspacesession.DefaultTerminalRows
	}
	return &TerminalStream{
		ID: hubapi.NewID("termrec"), WorkspaceID: record.ID,
		OrganizationID: string(record.OrganizationID), ProjectID: string(record.ProjectID),
		RelaySessionID: connection.AuditID, StreamID: stream.ID,
		PrincipalID: connection.PrincipalID, Subject: connection.Subject,
		Isolation: w.TerminalConfig().Isolation, SupportReason: connection.SupportReason,
		StartedAt: now, Cols: cols, Rows: rows,
		Recording: workspacesession.NewRecording(cols, rows, now),
	}
}

// recordPersonTerminalFrame records what a person did on a terminal stream.
//
// Input is recorded and marked rather than dropped. The terminal contract says outright
// that recording cannot remove what the person typed and that the setting page
// says so; a recording that silently held only the shell's half would be a
// worse answer than one that says which half is which, because a reader would
// not know they were reading half.
func (w *HTTP) recordPersonTerminalFrame(connection *Connection, stream *Stream, frame workspacesession.Frame) {
	if !w.TerminalConfig().RecordEnabled() || stream.Channel != workspacesession.ChannelTerminal {
		return
	}
	switch frame.Type {
	case workspacesession.TypeTerminalInput:
		var payload workspacesession.TerminalInput
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return
		}
		data := payload.Data
		if payload.Encoding == workspacesession.EncodingBase64 {
			decoded, err := base64.StdEncoding.DecodeString(payload.Data)
			if err != nil {
				return
			}
			data = string(decoded)
		}
		w.Relay.AppendTerminalEvent(connection.WorkspaceID, stream.ID, workspacesession.AsciicastInput, data, w.now())
	case workspacesession.TypeTerminalResize:
		var payload workspacesession.TerminalResize
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return
		}
		size, err := workspacesession.ValidateTerminalResize(payload)
		if err != nil {
			return
		}
		w.Relay.ResizeTerminalRecording(connection.WorkspaceID, stream.ID, size.Cols, size.Rows, w.now())
	}
}

// recordRunnerTerminalFrame records what the shell wrote.
func (w *HTTP) recordRunnerTerminalFrame(connection *Connection, stream *Stream, frame workspacesession.Frame) {
	if frame.Type != workspacesession.TypeTerminalOutput {
		return
	}
	var payload workspacesession.TerminalOutput
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return
	}
	// A terminal's output is always base64 on the wire, because a PTY's bytes
	// are text interleaved with escape sequences and a span cut at a frame
	// boundary routinely ends inside one. The recording holds what the shell
	// wrote rather than the envelope it travelled in.
	data := payload.Data
	if payload.Encoding == workspacesession.EncodingBase64 {
		decoded, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil {
			return
		}
		data = string(decoded)
	}
	w.Relay.AppendTerminalEvent(connection.WorkspaceID, stream.ID, workspacesession.AsciicastOutput, data, w.now())
}

// finishTerminalRecordings writes every recording it is given.
//
// It drops the caller's cancellation and keeps only its
// values: the events that end a terminal stream -- a socket closing, a
// workspace ending, the hub shutting down -- are the same events that cancel
// the contexts they arrive on, so a write that honoured the cancellation would
// be abandoned exactly when it mattered and the recording would be lost.
func (w *HTTP) FinishTerminalRecordings(ctx context.Context, states []*TerminalStream) {
	for _, state := range states {
		if state == nil {
			continue
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
		w.FinishTerminalRecording(writeCtx, state)
		cancel()
	}
}

// finishTerminalRecording writes one recording and points the relay session's
// recording_artifact at where it may be read.
func (w *HTTP) FinishTerminalRecording(ctx context.Context, state *TerminalStream) {
	now := w.now()
	organization := tracker.OrganizationID(state.OrganizationID)
	ref, err := w.PrepareRelayOutput(ctx, organization, []byte(state.Recording.Cast()))
	if err != nil {
		w.Logger.Warn("workspace.terminal_recording_not_written", "workspace_id", state.WorkspaceID, "stream", state.StreamID, "error", err)
		return
	}
	var recorded postgres.TerminalRecording
	err = w.AccessWrite(ctx, organization, func(writer tenantstore.Writer) error {
		owner, ok := writer.(postgres.RelayActionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := writer.LockArchiveAccounting(ctx); err != nil {
			return err
		}
		current, err := owner.TerminalRecording(ctx, tracker.ProjectID(state.ProjectID), state.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		var previous *attachment.ArchiveReference
		if err == nil {
			if current.CastContentBytes > ref.UncompressedSize || current.FinishedAt != nil && current.FinishedAt.After(now) {
				recorded = current
				return nil
			}
			if current.CastObjectKey != ref.Key {
				archive, ok := writer.(hubrecords.AttemptArchiveReader)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				object, err := archive.ArchiveObjectByKey(ctx, current.CastObjectKey)
				if err != nil {
					return err
				}
				if object.Reference.SHA256 != current.CastSha256 || object.Reference.CompressedSize != current.CastBytes || object.Reference.UncompressedSize != current.CastContentBytes {
					return artifact.ErrIntegrity
				}
				previous = &object.Reference
			}
		}
		recorded, err = owner.PutTerminalRecording(ctx, postgres.PutTerminalRecordingInput{ExpectedRevision: current.Revision, OrganizationID: organization, ProjectID: tracker.ProjectID(state.ProjectID), WorkspaceID: state.WorkspaceID, ID: state.ID, RelaySessionID: state.RelaySessionID, StreamID: state.StreamID, PrincipalID: state.PrincipalID, Subject: state.Subject, Isolation: state.Isolation, SupportReason: state.SupportReason, StartedAt: state.StartedAt, FinishedAt: &now, TerminalCols: int32(state.Cols), TerminalRows: int32(state.Rows), CastObjectKey: ref.Key, CastSha256: ref.SHA256, CastBytes: ref.CompressedSize, CastContentBytes: ref.UncompressedSize, Truncated: state.Recording.Truncated(), UpdatedAt: now})
		if err != nil {
			return err
		}
		if err := RetainRelayArchive(ctx, writer, organization, tracker.ProjectID(state.ProjectID), state.ID, "workspace-terminal-recording", ref, now); err != nil {
			return err
		}
		if previous != nil {
			object, err := writer.ArchiveObjectByReference(ctx, *previous)
			if err != nil {
				return err
			}
			if err := writer.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "workspace-terminal-recording", ID: actionstore.ArchiveOwnerID(tracker.ProjectID(state.ProjectID), state.ID), CreatedAt: now}); err != nil {
				return err
			}
		}
		if state.RelaySessionID == "" {
			return nil
		}
		return owner.AttachRelayRecording(ctx, postgres.AttachRelayRecordingInput{OrganizationID: organization, ProjectID: tracker.ProjectID(state.ProjectID), WorkspaceID: state.WorkspaceID, RelaySessionID: state.RelaySessionID, RecordingID: state.ID, RecordingArtifact: relaySessionRecordingReference(state)})
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return
	}
	if err != nil {
		w.Logger.Warn("workspace.terminal_recording_not_written", "workspace_id", state.WorkspaceID, "stream", state.StreamID, "error", err)
		return
	}
	w.Logger.Info("workspace.terminal_recorded", "workspace_id", state.WorkspaceID, "stream", state.StreamID, "recording_id", state.ID, "bytes", recorded.CastContentBytes, "truncated", recorded.Truncated, "isolation", recorded.Isolation)
}

// relaySessionRecordingReference is the path a relay session row points at.
func relaySessionRecordingReference(state *TerminalStream) string {
	return "/api/v2/organizations/" + state.OrganizationID + "/projects/" + state.ProjectID +
		"/workspaces/" + state.WorkspaceID + "/terminal-recordings?relay_session=" + state.RelaySessionID
}
