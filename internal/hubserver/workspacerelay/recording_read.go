package workspacerelay

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/workspacesession"

	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

// Terminal recordings.
//
// workspaces.terminal.record is on by default and stores each stream as an
// asciicast v2 artifact referenced from the relay session row. The hub records
// it from the frames it relays, exactly as it records an action run from the
// exec frames it relays: it is forwarding them already, and a
// recording assembled anywhere else would be a recording of something other
// than what actually crossed the relay.
//
// The audience is the part that makes this its own resource rather than a
// column. The terminal contract: "the person who ran the session plus owners and admins,
// never the issue's readers", and "a `user`-isolation recording is readable by
// owners only". A recording can carry what the runner account can see, which is
// wider than the issue, so it is read through a gate of its own and never
// through the issue's read rule.
//
// Two things the terminal contract asks for are not here, and both are recorded rather
// than quietly skipped. Secret scrubbing -- "known secret values from the
// project's secret store are scrubbed from recordings on write, best
// effort" -- has nothing to scrub against: the secret store is the browser preview rule's
// and is deferred to sequencing step 3, so there is no set of
// known values in this build. The scrub goes in when the store does, on write,
// where the contract puts it. And the recording is capped at
// MaxRecordingBytes with a truncated flag, which the terminal contract does not specify;
// a terminal has no cap on the wire and must not have one, so the cap lives on
// the stored copy and a reader is told when the copy is shorter than the
// session was.

// listWorkspaceTerminalRecordings implements
// GET {nativeBase}/workspaces/:workspace/terminal-recordings.
func (w *HTTP) ListWorkspaceTerminalRecordings(c echo.Context) error {
	recordings, err := w.ReadRecordings(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("workspace"), c.QueryParam("relay_session"), 0)
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"recordings": recordings})
}

// getWorkspaceTerminalRecording implements
// GET {nativeBase}/workspaces/:workspace/terminal-recordings/:recording.
//
// It answers the asciicast document rather than JSON, because the bytes are a
// recording: a reader plays them, saves them or pipes them into asciinema, and
// wrapping them in an envelope would only mean every client unwrapped them
// again. That is the same judgement the action run output endpoint makes about
// a log.
func (w *HTTP) GetWorkspaceTerminalRecording(c echo.Context) error {
	recording, err := w.ReadRecording(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("workspace"), c.Param("recording"))
	if err != nil {
		return w.NativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/x-asciicast; charset=utf-8", []byte(recording.Cast))
}

func terminalRecordingFromPostgres(row postgres.TerminalRecording) RecordingRecord {
	return RecordingRecord{TerminalRecording: TerminalRecording{ID: row.ID, WorkspaceID: row.WorkspaceID, StreamID: row.StreamID, RelaySessionID: row.RelaySessionID, PrincipalID: row.PrincipalID, Subject: row.Subject, Isolation: row.Isolation, SupportReason: row.SupportReason, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Cols: int(row.TerminalCols), Rows: int(row.TerminalRows), Bytes: row.CastContentBytes, Truncated: row.Truncated}, OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID)}
}

func (w *HTTP) ReadRecording(ctx context.Context, scope hubscope.NativeScope, workspaceID, id string) (RecordingRecord, error) {
	err := w.RequireWorkspaces()
	if err != nil {
		return RecordingRecord{}, err
	}
	var row postgres.TerminalRecording
	var recording RecordingRecord
	err = w.AccessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if _, err := w.ReadRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		row, err = owner.TerminalRecording(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if row.WorkspaceID != workspaceID {
			return hubapi.NativeNotFound()
		}
		recording = terminalRecordingFromPostgres(row)
		if !RecordingReadable(hubscope.AuthCredential(scope.Credential), recording) {
			return hubapi.NativeNotFound()
		}
		return nil
	})
	if err != nil {
		return RecordingRecord{}, hubapi.ProjectRecordError(err)
	}
	ref := attachment.ArchiveReference{Key: row.CastObjectKey, SHA256: row.CastSha256, CompressedSize: row.CastBytes, UncompressedSize: row.CastContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	if ref.Validate(string(scope.Organization)) != nil || ref.UncompressedSize > workspacesession.MaxRecordingBytes {
		return RecordingRecord{}, artifact.ErrIntegrity
	}
	ref, err = w.RelayArchiveReference(ctx, scope.Organization, ref)
	if err != nil {
		return RecordingRecord{}, err
	}
	raw, err := hubcontent.ReadChangeContent(w.ContentContext(ctx), string(scope.Organization), ref)
	if err != nil {
		return RecordingRecord{}, err
	}
	recording.Cast = string(raw)
	return recording, nil
}
func (w *HTTP) ReadRecordings(ctx context.Context, scope hubscope.NativeScope, workspaceID, relaySessionID string, limit int) ([]TerminalRecording, error) {
	err := w.RequireWorkspaces()
	if err != nil {
		return nil, err
	}
	var rows []postgres.TerminalRecording
	err = w.AccessRead(ctx, scope.Organization, func(reader tenantstore.Reader) error {
		if _, err := w.ReadRelayWorkspaceForActor(ctx, reader, scope, workspaceID); err != nil {
			return err
		}
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		rows, err = owner.RecentTerminalRecordings(ctx, scope.Project, workspaceID, relaySessionID, RecordingPage)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	result := []TerminalRecording{}
	for _, row := range rows {
		record := terminalRecordingFromPostgres(row)
		if RecordingReadable(hubscope.AuthCredential(scope.Credential), record) {
			result = append(result, record.Resource())
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}
