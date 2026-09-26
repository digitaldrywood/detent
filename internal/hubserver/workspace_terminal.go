package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Terminal recordings (decisions section 18.3).
//
// workspaces.terminal.record is on by default and stores each stream as an
// asciicast v2 artifact referenced from the relay session row. The hub records
// it from the frames it relays, exactly as it records an action run from the
// exec frames it relays (section 18.12): it is forwarding them already, and a
// recording assembled anywhere else would be a recording of something other
// than what actually crossed the relay.
//
// The audience is the part that makes this its own resource rather than a
// column. Section 18.3: "the person who ran the session plus owners and admins,
// never the issue's readers", and "a `user`-isolation recording is readable by
// owners only". A recording can carry what the runner account can see, which is
// wider than the issue, so it is read through a gate of its own and never
// through the issue's read rule.
//
// Two things section 18.3 asks for are not here, and both are recorded rather
// than quietly skipped. Secret scrubbing -- "known secret values from the
// project's secret store (18.7) are scrubbed from recordings on write, best
// effort" -- has nothing to scrub against: the secret store is section 18.7's
// and is deferred to sequencing step 3 (section 18.11), so there is no set of
// known values in this build. The scrub goes in when the store does, on write,
// where the contract puts it. And the recording is capped at
// MaxRecordingBytes with a truncated flag, which section 18.3 does not specify;
// a terminal has no cap on the wire and must not have one, so the cap lives on
// the stored copy and a reader is told when the copy is shorter than the
// session was.

// TerminalRecording is one recorded terminal stream.
type TerminalRecording struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	StreamID       string     `json:"stream_id"`
	RelaySessionID string     `json:"relay_session_id"`
	PrincipalID    string     `json:"principal_id"`
	Subject        string     `json:"subject"`
	Isolation      string     `json:"isolation"`
	SupportReason  string     `json:"support_reason,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Cols           int        `json:"cols"`
	Rows           int        `json:"rows"`
	// Artifact names where the bytes may be read, the way a run's
	// output_artifact does (section 18.12). It is a path rather than the
	// document, because a listing that carried every recording whole would be a
	// listing nobody could page.
	Artifact  string `json:"artifact"`
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// terminalRecordingRecord is one stored row, cast included.
type terminalRecordingRecord struct {
	TerminalRecording
	OrganizationID string
	ProjectID      string
	Cast           string
}

// resource projects a row onto what a client reads.
func (r terminalRecordingRecord) resource() TerminalRecording {
	recording := r.TerminalRecording
	recording.Artifact = terminalRecordingPath(r)
	return recording
}

// terminalRecordingPath is what Artifact holds: the path of the endpoint that
// serves this recording's bytes, so the receipt and the route can never name
// different places. It stays empty until there is something to read, because a
// client handed a link expects bytes behind it.
func terminalRecordingPath(r terminalRecordingRecord) string {
	if r.Bytes <= 0 {
		return ""
	}
	return "/api/v2/organizations/" + r.OrganizationID + "/projects/" + r.ProjectID +
		"/workspaces/" + r.WorkspaceID + "/terminal-recordings/" + r.ID
}

// terminalRecordingColumns is the row every read selects.
const terminalRecordingColumns = `id, workspace_id, organization_id, project_id, relay_session_id, stream_id,
 principal_id, subject, isolation, support_reason, started_at, finished_at, terminal_cols, terminal_rows, cast_text, cast_bytes, truncated`

// scanTerminalRecording reads one row.
func scanTerminalRecording(scan func(...any) error) (terminalRecordingRecord, error) {
	var record terminalRecordingRecord
	var started string
	var finished sql.NullString
	if err := scan(&record.ID, &record.WorkspaceID, &record.OrganizationID, &record.ProjectID,
		&record.RelaySessionID, &record.StreamID, &record.PrincipalID, &record.Subject,
		&record.Isolation, &record.SupportReason, &started, &finished,
		&record.Cols, &record.Rows, &record.Cast, &record.Bytes, &record.Truncated); err != nil {
		return terminalRecordingRecord{}, err
	}
	parsed, err := parseTimeValue(started)
	if err != nil {
		return terminalRecordingRecord{}, fmt.Errorf("decode terminal recording started_at: %w", err)
	}
	record.StartedAt = parsed
	if finished.Valid {
		parsed, err := parseTimeValue(finished.String)
		if err != nil {
			return terminalRecordingRecord{}, fmt.Errorf("decode terminal recording finished_at: %w", err)
		}
		record.FinishedAt = &parsed
	}
	return record, nil
}

// writeTerminalRecording stores one finished recording.
//
// It is an upsert on the stream rather than an insert, because two teardown
// paths may reach one stream -- the person's socket closing while the workspace
// ends -- and the second must correct the row rather than fail on the unique
// index or leave two half-recordings of one shell.
func writeTerminalRecording(ctx context.Context, exec nativeExecer, record terminalRecordingRecord, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO workspace_terminal_recordings
 (id, workspace_id, organization_id, project_id, relay_session_id, stream_id, principal_id, subject,
  isolation, support_reason, started_at, finished_at, terminal_cols, terminal_rows, cast_text, cast_bytes, truncated,
  created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, stream_id) DO UPDATE SET
 finished_at = excluded.finished_at, cast_text = excluded.cast_text,
 cast_bytes = excluded.cast_bytes, truncated = excluded.truncated,
 terminal_cols = excluded.terminal_cols, terminal_rows = excluded.terminal_rows,
 updated_at = excluded.updated_at`,
		record.ID, record.WorkspaceID, record.OrganizationID, record.ProjectID, record.RelaySessionID,
		record.StreamID, record.PrincipalID, record.Subject, record.Isolation, record.SupportReason,
		formatHubTime(record.StartedAt), nullTime(record.FinishedAt), record.Cols, record.Rows,
		record.Cast, record.Bytes, record.Truncated, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return fmt.Errorf("write terminal recording: %w", err)
	}
	return nil
}

// readTerminalRecordings lists a workspace's recordings, newest first,
// optionally narrowed to one relay session.
func readTerminalRecordings(ctx context.Context, query nativeQueryer, workspaceID, relaySessionID string) ([]terminalRecordingRecord, error) {
	statement := `SELECT ` + terminalRecordingColumns + ` FROM workspace_terminal_recordings
WHERE workspace_id = ?`
	arguments := []any{workspaceID}
	if relaySessionID != "" {
		statement += ` AND relay_session_id = ?`
		arguments = append(arguments, relaySessionID)
	}
	statement += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	arguments = append(arguments, terminalRecordingPage)

	rows, err := query.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query terminal recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	recordings := []terminalRecordingRecord{}
	for rows.Next() {
		record, err := scanTerminalRecording(rows.Scan)
		if err != nil {
			return nil, err
		}
		recordings = append(recordings, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate terminal recordings: %w", err)
	}
	return recordings, nil
}

// readTerminalRecordingByID reads one recording inside a workspace.
func readTerminalRecordingByID(ctx context.Context, query nativeQueryer, workspaceID, id string) (terminalRecordingRecord, error) {
	row := query.QueryRowContext(ctx, `SELECT `+terminalRecordingColumns+` FROM workspace_terminal_recordings
WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	return scanTerminalRecording(row.Scan)
}

// terminalRecordingPage bounds one listing.
const terminalRecordingPage = 50

// terminalRecordingReadable is section 18.3's audience, and it is deliberately
// narrower than the issue's.
//
// The person who ran the session may always read their own: they were there,
// and every byte in it either crossed their screen or came off their keyboard.
// Owners and admins may read a container-isolation recording, which is the
// level the setting recommends and the one a member may use. A user-isolation
// recording is owners only, because at that level the shell ran as the runner's
// own account with its credential store and its other checkouts in reach, so
// the recording can carry more than any other artifact in the product.
//
// Nothing widens it. A viewer never had a terminal at all, and an issue's
// readers are not an audience for this whatever their grant says.
func terminalRecordingReadable(scope nativeScope, record terminalRecordingRecord) bool {
	role := scope.credential.HostedRole
	if scope.credential.Hosted == nil {
		// A token principal has no membership and no role to narrow against. It
		// is an organization-level credential -- the operator or admin token
		// that administers this hub -- so it reads at the owner's level, which
		// is the widest this audience ever goes. Narrowing it further would not
		// protect anything: the same token can read every other resource in the
		// organization, and a recording it could not read would simply be a
		// resource with no administrator.
		return true
	}
	if scope.credential.Hosted.Subject != "" && scope.credential.Hosted.Subject == record.Subject {
		return true
	}
	if record.Isolation == workspacesession.IsolationUser {
		return role == "owner"
	}
	return role == "owner" || role == "admin"
}

// listWorkspaceTerminalRecordings implements
// GET {nativeBase}/workspaces/:workspace/terminal-recordings.
func (s *Service) listWorkspaceTerminalRecordings(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	record, err := service.readWorkspaceForActor(ctx, s.database.db, scope, c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	stored, err := readTerminalRecordings(ctx, s.database.db, record.ID, c.QueryParam("relay_session"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	recordings := []TerminalRecording{}
	for _, item := range stored {
		if !terminalRecordingReadable(scope, item) {
			// A recording this actor may not read is absent rather than
			// refused: telling somebody a recording exists that they may not
			// see is itself a disclosure about who was in the worktree.
			continue
		}
		recordings = append(recordings, item.resource())
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
func (s *Service) getWorkspaceTerminalRecording(c echo.Context) error {
	service, err := s.requireWorkspaces()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	record, err := service.readWorkspaceForActor(ctx, s.database.db, scope, c.Param("workspace"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	recording, err := readTerminalRecordingByID(ctx, s.database.db, record.ID, c.Param("recording"))
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !terminalRecordingReadable(scope, recording) {
		// Not found rather than forbidden, for the reason the listing omits it.
		return s.nativeAPIError(c, nativeNotFound())
	}
	return c.Blob(http.StatusOK, "application/x-asciicast; charset=utf-8", []byte(recording.Cast))
}

// nativeExecer is the write half of nativeQueryer. A recording is written
// outside the hub's transaction helper, the way an audit row is: both have to
// survive a torn connection, and a record that vanished with a rolled-back
// write would not be a record.
type nativeExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
