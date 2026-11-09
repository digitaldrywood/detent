package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func terminalFromRecord(row postgres.TerminalRecording) terminalRecordingRecord {
	return terminalRecordingRecord{TerminalRecording: TerminalRecording{ID: row.ID, WorkspaceID: row.WorkspaceID, StreamID: row.StreamID, RelaySessionID: row.RelaySessionID, PrincipalID: row.PrincipalID, Subject: row.Subject, Isolation: row.Isolation, SupportReason: row.SupportReason, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Cols: int(row.TerminalCols), Rows: int(row.TerminalRows), Bytes: row.CastContentBytes, Truncated: row.Truncated}, OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID)}
}
func (s *Service) readTerminalRecordingRecord(ctx context.Context, scope nativeScope, workspace, id string) (terminalRecordingRecord, error) {
	var stored postgres.TerminalRecording
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		if _, err := workspaceForActor(ctx, r, scope, workspace); err != nil {
			return err
		}
		var err error
		stored, err = r.TerminalRecordingByID(ctx, postgres.TerminalRecordingInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: workspace, ID: id})
		if err != nil {
			return err
		}
		if !terminalRecordingReadable(scope, terminalFromRecord(stored)) {
			return nativeNotFound()
		}
		return nil
	})
	if err != nil {
		return terminalRecordingRecord{}, err
	}
	record := terminalFromRecord(stored)
	raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.organization), workspaceArchive(stored.CastObjectKey, stored.CastSha256, stored.CastBytes, stored.CastContentBytes))
	if err != nil {
		return record, err
	}
	record.Cast = string(raw)
	return record, nil
}
func (s *Service) readTerminalRecordingRecords(ctx context.Context, scope nativeScope, workspace, relay string, limit int) ([]TerminalRecording, error) {
	var records []TerminalRecording
	err := s.workspaceRead(ctx, scope, func(r workspaceReader) error {
		if _, err := workspaceForActor(ctx, r, scope, workspace); err != nil {
			return err
		}
		input := postgres.ReadableTerminalRecordingsInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: workspace, RelaySessionID: relay, PageLimit: int32(workspaceListLimit)}
		if scope.credential.Hosted == nil {
			input.ReadAll = true
		} else {
			input.Subject = scope.credential.Hosted.Subject
			input.ReadAll = scope.credential.HostedRole == "owner"
			input.ReadNonUser = scope.credential.HostedRole == "admin"
		}
		rows, err := r.RecentReadableTerminalRecordings(ctx, input)
		if err != nil {
			return err
		}
		for _, row := range rows {
			record := terminalFromRecord(row)
			if !terminalRecordingReadable(scope, record) || relay != "" && relay != row.RelaySessionID {
				continue
			}
			records = append(records, record.resource())
			if limit > 0 && len(records) >= limit {
				break
			}
		}
		return nil
	})
	if records == nil {
		records = []TerminalRecording{}
	}
	return records, err
}
func (w *workspaceService) finishTerminalRecordingRecord(ctx context.Context, state *terminalStreamState) error {
	scope := nativeScope{organization: tracker.OrganizationID(state.organizationID), project: tracker.ProjectID(state.projectID)}
	cast := []byte(state.recording.Cast())
	ref, err := w.server.runDetailStore().StoreDetail(ctx, state.organizationID, cast)
	if err != nil {
		return err
	}
	return w.server.workspaceWrite(ctx, scope, func(writer workspaceWriter, now time.Time) error {
		if err := keepWorkspaceArchive(ctx, writer, scope, ref, "workspace-terminal-recording", state.id, now); err != nil {
			return err
		}
		_, err := writer.PutTerminalRecording(ctx, postgres.PutTerminalRecordingInput{OrganizationID: scope.organization, ProjectID: scope.project, WorkspaceID: state.workspaceID, ID: state.id, RelaySessionID: state.relaySessionID, StreamID: state.streamID, PrincipalID: state.principalID, Subject: state.subject, Isolation: state.isolation, SupportReason: state.supportReason, StartedAt: state.startedAt, FinishedAt: &now, TerminalCols: int32(state.cols), TerminalRows: int32(state.rows), CastObjectKey: ref.Key, CastSha256: ref.SHA256, CastBytes: ref.CompressedSize, CastContentBytes: ref.UncompressedSize, Truncated: state.recording.Truncated(), UpdatedAt: now, ExpectedRevision: 0})
		if err != nil {
			return err
		}
		return writer.ReferenceRelayRecording(ctx, postgres.ReferenceRelayRecordingInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: state.relaySessionID, RecordingArtifact: relaySessionRecordingReference(state)})
	})
}
