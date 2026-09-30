package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Recording terminal streams from the frames the hub relays (decisions section
// 18.3).
//
// The hub forwards the terminal channel like any other, and it also reads it.
// That is the same arrangement section 18.12 makes for an action run, and for
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
func (w *workspaceService) beginTerminalRecording(
	record workspaceRecord,
	connection *relayConnection,
	stream *relayStream,
	open workspacesession.TerminalOpen,
) *terminalStreamState {
	if !w.config.Terminal.RecordEnabled() {
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
	return &terminalStreamState{
		id: newNativeID("termrec"), workspaceID: record.ID,
		organizationID: string(record.OrganizationID), projectID: string(record.ProjectID),
		relaySessionID: connection.auditID, streamID: stream.id,
		principalID: connection.principalID, subject: connection.subject,
		isolation: w.config.Terminal.Isolation, supportReason: connection.supportReason,
		startedAt: now, cols: cols, rows: rows,
		recording: workspacesession.NewRecording(cols, rows, now),
	}
}

// recordPersonTerminalFrame records what a person did on a terminal stream.
//
// Input is recorded and marked rather than dropped. Section 18.3 says outright
// that recording cannot remove what the person typed and that the setting page
// says so; a recording that silently held only the shell's half would be a
// worse answer than one that says which half is which, because a reader would
// not know they were reading half.
func (w *workspaceService) recordPersonTerminalFrame(connection *relayConnection, stream *relayStream, frame workspacesession.Frame) {
	if !w.config.Terminal.RecordEnabled() || stream.channel != workspacesession.ChannelTerminal {
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
		w.relay.appendTerminalEvent(connection.workspaceID, stream.id, workspacesession.AsciicastInput, data, w.now())
	case workspacesession.TypeTerminalResize:
		var payload workspacesession.TerminalResize
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return
		}
		size, err := workspacesession.ValidateTerminalResize(payload)
		if err != nil {
			return
		}
		w.relay.resizeTerminalRecording(connection.workspaceID, stream.id, size.Cols, size.Rows, w.now())
	}
}

// recordRunnerTerminalFrame records what the shell wrote.
func (w *workspaceService) recordRunnerTerminalFrame(connection *relayConnection, stream *relayStream, frame workspacesession.Frame) {
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
	w.relay.appendTerminalEvent(connection.workspaceID, stream.id, workspacesession.AsciicastOutput, data, w.now())
}

// finishTerminalRecordings writes every recording it is given.
//
// It drops the caller's cancellation and keeps only its
// values: the events that end a terminal stream -- a socket closing, a
// workspace ending, the hub shutting down -- are the same events that cancel
// the contexts they arrive on, so a write that honoured the cancellation would
// be abandoned exactly when it mattered and the recording would be lost.
func (w *workspaceService) finishTerminalRecordings(ctx context.Context, states []*terminalStreamState) {
	for _, state := range states {
		if state == nil {
			continue
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayWriteTimeout)
		w.finishTerminalRecording(writeCtx, state)
		cancel()
	}
}

// finishTerminalRecording writes one recording and points the relay session's
// recording_artifact at where it may be read.
func (w *workspaceService) finishTerminalRecording(ctx context.Context, state *terminalStreamState) {
	now := w.now()
	record := terminalRecordingRecord{
		TerminalRecording: TerminalRecording{
			ID: state.id, WorkspaceID: state.workspaceID, StreamID: state.streamID,
			RelaySessionID: state.relaySessionID, PrincipalID: state.principalID,
			Subject: state.subject, Isolation: state.isolation, SupportReason: state.supportReason,
			StartedAt: state.startedAt, FinishedAt: &now, Cols: state.cols, Rows: state.rows,
			Bytes: int64(state.recording.Bytes()), Truncated: state.recording.Truncated(),
		},
		OrganizationID: state.organizationID, ProjectID: state.projectID,
		Cast: state.recording.Cast(),
	}
	if err := writeTerminalRecording(ctx, w.server.database.db, record, now); err != nil {
		w.logger.Warn("workspace.terminal_recording_not_written", "workspace_id", state.workspaceID,
			"stream", state.streamID, "error", err)
		return
	}
	w.logger.Info("workspace.terminal_recorded", "workspace_id", state.workspaceID,
		"stream", state.streamID, "recording_id", state.id, "bytes", record.Bytes,
		"truncated", record.Truncated, "isolation", record.Isolation)
	// The relay session row references the recording, which is the reference
	// section 18.3 asks for. It is written once and left alone: a connection
	// that opened several terminals gets one reference to the listing that
	// covers all of them, because one column cannot name several recordings.
	if state.relaySessionID == "" {
		return
	}
	if _, err := w.server.database.db.ExecContext(ctx,
		`UPDATE workspace_relay_sessions SET recording_artifact = ? WHERE id = ? AND recording_artifact = ''`,
		relaySessionRecordingReference(state), state.relaySessionID); err != nil {
		w.logger.Warn("workspace.relay_recording_not_referenced",
			"relay_session_id", state.relaySessionID, "error", err)
	}
}

// relaySessionRecordingReference is the path a relay session row points at.
func relaySessionRecordingReference(state *terminalStreamState) string {
	return "/api/v2/organizations/" + state.organizationID + "/projects/" + state.projectID +
		"/workspaces/" + state.workspaceID + "/terminal-recordings?relay_session=" + state.relaySessionID
}
