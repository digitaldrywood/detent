package hubserver

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeArchiveContextKey struct{}

type nativeArchiveRead struct {
	service     *Service
	scope       nativeScope
	presign     bool
	webResponse bool
	descriptor  *tracker.ArchiveDescriptor
	section     *attachment.ArchiveSection
	first       int64
	last        int64
	err         error
}

type nativeArchiveError struct {
	Archive *tracker.ArchiveDescriptor `json:"archive"`
}

func (e *nativeArchiveError) Error() string { return "archive section " + e.Archive.Availability }

func (s *Service) archiveContext(ctx context.Context, scope nativeScope, presign bool) context.Context {
	if ctx.Value(nativeArchiveContextKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, nativeArchiveContextKey{}, &nativeArchiveRead{service: s, scope: scope, presign: presign})
}

func (s *Service) archiveRequestContext(c echo.Context, scope nativeScope) context.Context {
	ctx := s.archiveContext(c.Request().Context(), scope, c.QueryParam("archive_url") == "true")
	if state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead); ok {
		state.webResponse = c.Request().Header.Get("X-Detent-Archive-Read") == "1"
	}
	return ctx
}

func nativeArchiveDescriptor(ctx context.Context) *tracker.ArchiveDescriptor {
	if state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead); ok && !state.webResponse {
		return state.descriptor
	}
	return nil
}

func withNativeArchive(raw []byte, descriptor *tracker.ArchiveDescriptor) ([]byte, error) {
	if descriptor == nil {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("archive response must be an object")
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	object["archive"] = encoded
	return json.Marshal(object)
}

func nativeArchiveJSON(c echo.Context, status int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw, err = withNativeArchive(raw, nativeArchiveDescriptor(c.Request().Context()))
	if err != nil {
		return err
	}
	return c.JSONBlob(status, raw)
}

func archiveQuery(ctx context.Context, query nativeQueryer, item, kind, subject string, sequence int64) (nativeQueryer, error) {
	if explanationRead(ctx) {
		return query, nil
	}
	state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead)
	if !ok || kind == "history" && state.webResponse {
		return query, nil
	}
	if _, ok := query.(*nativeArchiveQuery); ok {
		return query, nil
	}
	section, found, err := state.resolve(ctx, query, item, kind, subject, sequence)
	if err != nil || !found {
		return query, err
	}
	return newNativeArchiveQuery(ctx, query, &section, subject, state.first, state.last)
}

func (r *nativeArchiveRead) resolve(ctx context.Context, query nativeQueryer, item, kind, subject string, sequence int64) (attachment.ArchiveSection, bool, error) {
	s, scope := r.service, r.scope
	if _, _, err := readNativeIssue(ctx, query, scope, item); err != nil {
		return attachment.ArchiveSection{}, false, err
	}
	var id string
	var first, last int64
	ref, err := scanNativeArchiveSection(query.QueryRowContext(ctx, `SELECT n.section_id,n.first_sequence,n.last_sequence,
	coalesce(a.object_key,''),coalesce(a.sha256,''),coalesce(a.size,0),coalesce(a.uncompressed_size,0),coalesce(a.codec,''),coalesce(a.schema_version,0),coalesce(a.kind,''),coalesce(a.retention_class,'')
FROM native_archive_sections n LEFT JOIN attachments a ON a.id=n.attachment_id AND a.organization_id=n.organization_id
AND EXISTS (SELECT 1 FROM attachment_archive_references ar WHERE ar.attachment_id=a.id AND ar.organization_id=n.organization_id AND ar.project_id=n.project_id AND ar.work_item_id=n.work_item_id)
WHERE n.organization_id=? AND n.project_id=? AND n.work_item_id=? AND n.kind=? AND n.subject_id=?
AND (?=0 OR n.first_sequence<=?) ORDER BY n.first_sequence DESC LIMIT 1`, scope.organization, scope.project, item, kind, subject, sequence, sequence), &id, &first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return attachment.ArchiveSection{}, false, nil
	}
	if err != nil {
		return attachment.ArchiveSection{}, false, err
	}
	if (kind == "revision" || kind == "history") && sequence > last {
		return attachment.ArchiveSection{}, false, nil
	}
	if r.descriptor != nil {
		if r.section == nil || r.section.SectionID != id || r.section.WorkItemID != item || r.section.ProjectID != string(scope.project) || r.section.OrganizationID != string(scope.organization) || r.descriptor.SHA256 != ref.SHA256 {
			if r.err != nil {
				return attachment.ArchiveSection{}, false, r.err
			}
			return attachment.ArchiveSection{}, false, &nativeArchiveError{Archive: &tracker.ArchiveDescriptor{Availability: "section_required", SectionIDs: []string{id}}}
		}
		return *r.section, true, r.err
	}
	r.descriptor = &tracker.ArchiveDescriptor{Availability: "unavailable", SectionIDs: []string{id}, SHA256: ref.SHA256}
	fail := func(availability string) (attachment.ArchiveSection, bool, error) {
		r.descriptor.Availability = availability
		r.err = &nativeArchiveError{Archive: r.descriptor}
		return attachment.ArchiveSection{}, false, r.err
	}
	if ref.Validate(string(scope.organization)) != nil || ref.UncompressedSize > attachment.MaxSectionBytes || ref.SchemaVersion != 1 || id == "" || (sequence > 0 && kind != "conversation" && sequence > last) {
		return fail("invalid")
	}
	reader := s.config.ArchiveReader
	if reader == nil && s.hostedShared() {
		reader = s.config.Hosted.SharedEntry.ReadArchive
	}
	if reader == nil {
		return fail("unavailable")
	}
	unrestricted := scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly && scope.credential.Hosted == nil
	unrestricted = unrestricted || scope.credential.Hosted != nil && scope.credential.EntryKey == nil && scope.credential.HostedKeyScope == "" && (scope.credential.HostedRole == "owner" || scope.credential.HostedRole == "admin")
	presign := r.presign && unrestricted
	response, err := readRuntimeObject(ctx, nativeRuntimeObjectKey{kind: "archive", organization: string(scope.organization), reference: ref, presign: presign}, func() (entryauthority.ArchiveResponse, error) {
		return reader(ctx, ref, presign)
	})
	var hydration *nativeRuntimeHydration
	if errors.As(err, &hydration) {
		r.descriptor = nil
		return attachment.ArchiveSection{}, false, err
	}
	if err != nil {
		return fail("unavailable")
	}
	section, err := attachment.DecodeArchiveSection(string(scope.organization), ref, response.Data)
	if err != nil || section.ProjectID != string(scope.project) || section.WorkItemID != item || section.SectionID != id {
		return fail("invalid")
	}
	if err := validateNativeArchiveSection(section, kind, subject, first, last); err != nil {
		return fail("invalid")
	}
	r.section = &section
	r.first = first
	r.last = last
	r.descriptor.Availability = "available"
	if presign && response.Access != nil && response.Access.ArchiveReference == ref && response.Access.ExpiresAt.After(s.config.now()) && !response.Access.ExpiresAt.After(s.config.now().Add(attachment.ArchiveReadLifetime)) {
		r.descriptor.URL, r.descriptor.ExpiresAt = response.Access.URL, &response.Access.ExpiresAt
	}
	return *r.section, true, nil
}

func scanNativeArchiveSection(row interface{ Scan(...any) error }, id *string, first, last *int64) (attachment.ArchiveReference, error) {
	var ref attachment.ArchiveReference
	err := row.Scan(id, first, last, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention)
	return ref, err
}

func nativeArchiveTables(kind string) []string {
	switch kind {
	case "run":
		return []string{"native_attempts", "native_attempt_events", "attempt_diffs", "attempt_diff_files"}
	case "change":
		return []string{"change_versions", "change_evidence"}
	case "revision":
		return []string{"collaboration_versions"}
	case "conversation":
		return []string{"conversation_messages", "conversation_events"}
	case "history":
		return []string{"collaboration_events"}
	default:
		return nil
	}
}

func validateNativeArchiveSection(section attachment.ArchiveSection, kind, subject string, first, last int64) error {
	seen := map[string]bool{}
	parents := map[string]bool{}
	records := 0
	for _, table := range section.Tables {
		if !slices.Contains(nativeArchiveTables(kind), table.Name) || seen[table.Name] || len(table.Columns) == 0 || table.Columns[0] != "rowid" {
			return attachment.ErrInvalid
		}
		seen[table.Name] = true
		identities := map[int64]bool{}
		for _, row := range table.Rows {
			records++
			if len(row) != len(table.Columns) {
				return attachment.ErrInvalid
			}
			var rowID int64
			if json.Unmarshal(row[0], &rowID) != nil || rowID <= 0 || identities[rowID] {
				return attachment.ErrInvalid
			}
			identities[rowID] = true
			sequenceColumn := ""
			if table.Name == "collaboration_versions" {
				sequenceColumn = "revision"
			}
			if table.Name == "conversation_messages" {
				sequenceColumn = "seq"
			}
			if table.Name == "collaboration_events" {
				sequenceColumn = "sequence"
			}
			if sequenceColumn != "" {
				index := slices.Index(table.Columns, sequenceColumn)
				var sequence int64
				if index < 0 || json.Unmarshal(row[index], &sequence) != nil || sequence < first || sequence > last {
					return attachment.ErrInvalid
				}
			}
			fields := make(map[string]string, len(row))
			for i, value := range row {
				var scalar any
				if json.Unmarshal(value, &scalar) != nil {
					return attachment.ErrInvalid
				}
				switch v := scalar.(type) {
				case nil, float64:
				case string:
					fields[table.Columns[i]] = v
				default:
					return attachment.ErrInvalid
				}
			}
			for column, expected := range map[string]string{"organization_id": section.OrganizationID, "project_id": section.ProjectID, "work_item_id": section.WorkItemID} {
				if slices.Contains(table.Columns, column) && fields[column] != expected {
					return attachment.ErrInvalid
				}
			}
			var column string
			switch table.Name {
			case "native_attempts":
				column = "id"
			case "native_attempt_events", "attempt_diffs":
				column = "attempt_id"
			case "change_versions", "change_evidence":
				column = "change_id"
			case "collaboration_versions":
				column = "record_id"
			case "conversation_messages", "conversation_events":
				column = "conversation_id"
			case "collaboration_events":
				column = "work_item_id"
			case "attempt_diff_files":
				continue
			}
			if fields[column] != subject {
				return attachment.ErrInvalid
			}
			if table.Name == "attempt_diffs" {
				parents[fields["id"]] = true
			}
		}
	}
	if records == 0 {
		return attachment.ErrInvalid
	}
	for _, table := range section.Tables {
		if table.Name != "attempt_diff_files" {
			continue
		}
		index := slices.Index(table.Columns, "diff_id")
		for _, row := range table.Rows {
			var id string
			if index < 0 || json.Unmarshal(row[index], &id) != nil || !parents[id] {
				return attachment.ErrInvalid
			}
		}
	}
	return nil
}

type nativeArchiveQuery struct {
	nativeQueryer
	ctes   string
	values []any
}

func nativeArchiveColumns(ctx context.Context, query nativeQueryer, table string) ([]string, []int, error) {
	rows, err := query.QueryContext(ctx, "PRAGMA table_xinfo("+table+")")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	columns := []string{"rowid"}
	var primaryColumns []int
	for rows.Next() {
		var index, notNull, primary, hidden int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &name, &kind, &notNull, &defaultValue, &primary, &hidden); err != nil {
			return nil, nil, err
		}
		columns = append(columns, name)
		if primary > 0 {
			primaryColumns = append(primaryColumns, len(columns)-1)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	return columns, primaryColumns, nil
}

func newNativeArchiveQuery(ctx context.Context, query nativeQueryer, section *attachment.ArchiveSection, subject string, first, last int64) (*nativeArchiveQuery, error) {
	result := &nativeArchiveQuery{nativeQueryer: query}
	var ctes []string
	sqlBytes := 0
	for _, table := range section.Tables {
		columns, primaryColumns, err := nativeArchiveColumns(ctx, query, table.Name)
		if err != nil {
			return nil, err
		}
		var records []string
		for _, column := range table.Columns {
			if !slices.Contains(columns, column) {
				return nil, attachment.ErrInvalid
			}
		}
		indexes := make([]int, len(columns))
		for i, column := range columns {
			indexes[i] = slices.Index(table.Columns, column)
			if indexes[i] < 0 && !archiveProjectionColumn(table.Name, column) {
				return nil, &nativeArchiveError{Archive: &tracker.ArchiveDescriptor{Availability: "incompatible", SectionIDs: []string{section.SectionID}}}
			}
		}
		for _, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return nil, attachment.ErrInvalid
			}
			cells := make([]string, len(columns))
			for i, column := range columns {
				raw := archiveProjectionDefault(table.Name, column)
				if index := indexes[i]; index >= 0 {
					raw = row[index]
				}
				cell, err := archiveSQLScalar(raw)
				if err != nil {
					return nil, err
				}
				cells[i] = cell
			}
			record := "(" + strings.Join(cells, ",") + ")"
			sqlBytes += len(record) + 1
			if sqlBytes > maxArchiveSQLBytes {
				return nil, attachment.ErrInvalid
			}
			records = append(records, record)
		}
		relation := "VALUES " + strings.Join(records, ",")
		if len(records) == 0 {
			relation = "SELECT " + strings.Repeat("NULL,", len(columns)-1) + "NULL WHERE 0"
		}
		quoted := make([]string, len(columns))
		liveProjection := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
			liveProjection[i] = "live." + quoted[i]
		}
		name := table.Name
		if len(primaryColumns) == 0 {
			return nil, attachment.ErrInvalid
		}
		var liveIdentity, archivedIdentity []string
		for _, index := range primaryColumns {
			liveIdentity = append(liveIdentity, "live."+quoted[index])
			archivedIdentity = append(archivedIdentity, quoted[index])
		}
		conditions := []string{"(" + strings.Join(liveIdentity, ",") + ") NOT IN (SELECT " + strings.Join(archivedIdentity, ",") + " FROM archive_" + name + ")"}
		for _, field := range []struct{ column, value string }{
			{"organization_id", section.OrganizationID},
			{"project_id", section.ProjectID},
			{"work_item_id", section.WorkItemID},
		} {
			if slices.Contains(columns, field.column) {
				conditions = append(conditions, "live."+field.column+"=?")
				result.values = append(result.values, field.value)
			}
		}
		var subjectColumn string
		switch name {
		case "native_attempts":
			subjectColumn = "id"
		case "native_attempt_events", "attempt_diffs":
			subjectColumn = "attempt_id"
		case "change_versions", "change_evidence":
			subjectColumn = "change_id"
		case "collaboration_versions":
			subjectColumn = "record_id"
		case "conversation_messages", "conversation_events":
			subjectColumn = "conversation_id"
		case "collaboration_events":
			subjectColumn = "work_item_id"
		case "attempt_diff_files":
			conditions = append(conditions, "live.diff_id IN (SELECT id FROM main.attempt_diffs WHERE organization_id=? AND project_id=? AND work_item_id=? AND attempt_id=?)")
			result.values = append(result.values, section.OrganizationID, section.ProjectID, section.WorkItemID, subject)
		default:
			return nil, attachment.ErrInvalid
		}
		if subjectColumn != "" {
			conditions = append(conditions, "live."+subjectColumn+"=?")
			result.values = append(result.values, subject)
		}
		if name == "conversation_messages" && first > 0 {
			conditions = append(conditions, "live.seq>=?")
			result.values = append(result.values, first)
		}
		if name == "collaboration_events" && last > 0 {
			var next sql.NullInt64
			if err := query.QueryRowContext(ctx, `SELECT min(first_sequence) FROM native_archive_sections WHERE organization_id=? AND project_id=? AND work_item_id=? AND kind='history' AND subject_id=? AND first_sequence>?`, section.OrganizationID, section.ProjectID, section.WorkItemID, subject, last).Scan(&next); err != nil {
				return nil, err
			}
			conditions = append(conditions, "live.sequence>=?")
			result.values = append(result.values, first)
			if next.Valid {
				conditions = append(conditions, "live.sequence<?")
				result.values = append(result.values, next.Int64)
			}
		}
		ctes = append(ctes, fmt.Sprintf(`archive_%s(%s) AS MATERIALIZED (%s),
%s AS NOT MATERIALIZED (SELECT * FROM archive_%s UNION ALL SELECT %s FROM main.%s AS live WHERE %s)`, name, strings.Join(quoted, ","), relation, name, name, strings.Join(liveProjection, ","), name, strings.Join(conditions, " AND ")))
	}
	result.ctes = strings.Join(ctes, ",")
	if len(result.ctes) > maxArchiveSQLBytes {
		return nil, attachment.ErrInvalid
	}
	return result, nil
}

func (q *nativeArchiveQuery) statement(statement string, args []any) (string, []any) {
	if q.ctes == "" {
		return statement, args
	}
	statement = strings.TrimSpace(statement)
	if len(statement) >= 5 && strings.EqualFold(statement[:5], "WITH ") {
		statement = statement[5:]
		return "WITH " + q.ctes + "," + statement, append(slices.Clone(q.values), args...)
	}
	return "WITH " + q.ctes + " " + statement, append(slices.Clone(q.values), args...)
}

func (q *nativeArchiveQuery) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	statement, args = q.statement(statement, args)
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (q *nativeArchiveQuery) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	statement, args = q.statement(statement, args)
	return q.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func nativeArchiveReadError(err error) *operatortool.RequestError {
	var unavailable *nativeArchiveError
	if !errors.As(err, &unavailable) {
		return nil
	}
	return &operatortool.RequestError{Code: "archive_" + unavailable.Archive.Availability, Message: unavailable.Error(), Archive: unavailable.Archive}
}

func archiveConversationQuery(ctx context.Context, query nativeQueryer, id string, before int64) (nativeQueryer, error) {
	state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead)
	if !ok {
		return query, nil
	}
	record, err := state.service.conversations.loadConversationRow(ctx, query, state.scope, id)
	if err != nil {
		return nil, err
	}
	if record.WorkItemID == "" || record.Visibility != "shared" {
		return query, nil
	}
	if before == 1 {
		return query, nil
	}
	if before > 0 {
		before--
	}
	return archiveQuery(ctx, query, record.WorkItemID, "conversation", id, before)
}

func archiveProjectionColumn(table, column string) bool {
	return archiveProjectionDefault(table, column) != nil
}

func archiveProjectionDefault(table, column string) json.RawMessage {
	switch table {
	case "attempt_diff_files":
		switch column {
		case "conversion_state":
			return json.RawMessage(`0`)
		}
	case "change_evidence":
		switch column {
		case "conversion_state":
			return json.RawMessage(`0`)
		case "has_implementation_review", "has_validator", "has_security_audit", "security_audit_landing", "check_run_id",
			"created_at", "received_at", "escape_occurrence_id", "escape_observed_at":
			return json.RawMessage(`null`)
		case "payload_object_key":
			return json.RawMessage(`""`)
		}
	case "change_versions":
		switch column {
		case "claim_head_sha", "gate_github_pull_request", "external_url", "activity_external_url", "created_at",
			"attempt_id", "head_sha", "repository", "external_provider", "external_id", "implementing_session_id",
			"implementing_machine_id":
			return json.RawMessage(`null`)
		case "conversion_state", "session_history_sequence", "session_history_candidate", "session_history_state":
			return json.RawMessage(`0`)
		case "payload_object_key", "session_history_machine_id":
			return json.RawMessage(`""`)
		}
	case "collaboration_events":
		switch column {
		case "scheduler_outcome", "claim_attempt_id", "run_id", "run_lease_id", "run_fencing_token", "run_sequence",
			"run_policy_id", "push_change_id", "push_version_id", "push_head_sha", "push_external_effect",
			"push_effect_state", "page_projection_ready", "actor_kind", "actor_principal_id", "event_revision", "from_state",
			"to_state", "reason", "run_present", "edited_title", "edited_body", "analytics_recorded_ns",
			"analytics_from_state", "analytics_to_state", "analytics_attempt_id", "analytics_skip_key",
			"analytics_projection_version", "run_attempt_id", "activity_spans_present", "archive_operation",
			"archive_from_state", "archive_to_state", "archive_attempt_id", "archive_outcome", "archive_landed_commit",
			"health_projected", "scheduler_present", "scheduler_at", "scheduler_revision", "transition_from",
			"transition_to", "transition_reason", "transition_detail", "transition_revision", "event_landed",
			"event_landing_at":
			return json.RawMessage(`null`)
		case "scheduler_reason", "scheduler_source", "scheduler_runner_id", "payload_attachment_id", "payload_entry",
			"payload_object_key", "payload_sha256", "scheduler_change_id", "scheduler_version_id", "scheduler_head_sha",
			"run_outcome", "run_machine_id", "run_session_id", "run_runner_id", "push_effect_id", "push_checkpoint_head_sha":
			return json.RawMessage(`""`)
		case "query_metadata_version", "conversion_state", "payload_version", "payload_bytes", "payload_content_bytes",
			"scheduler_change_present":
			return json.RawMessage(`0`)
		}
	case "conversation_events":
		switch column {
		case "query_metadata_version", "conversion_state":
			return json.RawMessage(`0`)
		case "message_id", "object_id", "message_role", "message_kind", "attempt_id", "execution_status", "thread_id",
			"turn_id", "snapshot_title", "snapshot_visibility", "snapshot_status", "snapshot_preferences_key",
			"snapshot_model", "snapshot_effort", "snapshot_access", "snapshot_subject_work_item_id", "snapshot_work_item_id",
			"snapshot_linked_at", "delta_offset", "delta_bytes", "message_revision":
			return json.RawMessage(`null`)
		case "content_attachment_id":
			return json.RawMessage(`""`)
		}
	case "conversation_messages":
		switch column {
		case "query_metadata_version", "conversion_state":
			return json.RawMessage(`0`)
		case "actor_kind", "tool_name", "tool_item_id", "tool_status", "operator_action_id", "operator_action_agent",
			"operator_action_result", "actor_principal_id", "text_bytes", "stored_text_bytes":
			return json.RawMessage(`null`)
		}
	case "native_attempts":
		switch column {
		case "health_projected", "landing_landed", "hot_projection_version", "health_instance_failure", "health_landing",
			"health_landed", "health_conflict", "conversion_state", "analytics_revision", "checkpoint_summary_ready":
			return json.RawMessage(`0`)
		case "health_failure_text", "failure_attribution", "landing_refusal_kind", "landing_change_id",
			"landing_version_id", "landing_merge_sha", "landing_observed_at", "health_failure_signature",
			"health_recovery_reason", "health_landing_at", "health_landing_key", "payload_object_key",
			"checkpoint_object_key", "checkpoint_resume", "checkpoint_availability", "checkpoint_storage",
			"checkpoint_worktree_state", "checkpoint_head_sha", "checkpoint_workspace_digest",
			"checkpoint_expected_head_sha", "checkpoint_effect_id", "checkpoint_ref", "checkpoint_commit_sha",
			"checkpoint_base_sha", "checkpoint_tree_sha", "checkpoint_change_id_summary", "checkpoint_version_id_summary",
			"checkpoint_change_head_sha", "checkpoint_publication_version_id":
			return json.RawMessage(`""`)
		case "health_updated_day", "health_landing_day", "claim_facts_known", "disposition_present",
			"disposition_blockers", "disposition_reason_code", "acceptance_recorded", "checkpoint_external_effect",
			"checkpoint_effect_state", "landing_waiting", "landing_head_sha", "usage_facts_ready",
			"process_peak_memory_bytes", "process_user_cpu_seconds", "process_system_cpu_seconds", "process_wall_seconds",
			"role", "activity_projection_version", "activity_phases_dropped", "activity_checkpoint_change_id",
			"activity_publication_url", "health_disposition_reason", "has_landing_receipt", "activity_identity_known",
			"finalization_external_url", "checkpoint_change_id", "report_projection_version", "report_requested_model",
			"report_resolved_model", "report_failed_at", "worker_cleanup_required", "cleanup_acknowledged_at",
			"implementing_session_id":
			return json.RawMessage(`null`)
		}
	}
	return nil
}

const maxArchiveSQLBytes = 16 << 20

func archiveSQLScalar(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", attachment.ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", attachment.ErrInvalid
	}
	switch value := value.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if value {
			return "1", nil
		}
		return "0", nil
	case json.Number:
		return value.String(), nil
	case string:
		if strings.ContainsRune(value, 0) {
			return "CAST(X'" + hex.EncodeToString([]byte(value)) + "' AS TEXT)", nil
		}
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	default:
		return "", attachment.ErrInvalid
	}
}
