package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

const issueStorageArchiveAge = 72 * time.Hour
const issueStorageArchiveBatch = 500

const storageArchiveConversationSequence = `(SELECT COALESCE(sum(c.event_seq+c.revision),0) FROM conversations c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND (c.work_item_id=i.native_id OR c.subject_work_item_id=i.native_id))`

const storageArchiveSafe = `EXISTS (SELECT 1 FROM workflow_states w WHERE w.id=i.workflow_state_id AND w.terminal=1)
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND NOT EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND a.status='running')
AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND (c.work_item_id=i.native_id OR c.subject_work_item_id=i.native_id)
AND (COALESCE(json_extract(c.execution_json,'$.status'),'') NOT IN ('idle','completed','interrupted','failed')
OR EXISTS(SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','sent','responding'))
OR EXISTS(SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))))
AND NOT EXISTS (SELECT 1 FROM change_issue_links l JOIN change_requests c ON c.id=l.change_id
WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id
AND COALESCE(json_extract(c.record_json,'$.current_version_id'),'')<>''
AND COALESCE(json_extract(c.record_json,'$.landed.version_id'),'')<>json_extract(c.record_json,'$.current_version_id'))`

type storageArchiveCandidate struct {
	scope                nativeScope
	item                 string
	revision, sequence   int64
	conversationSequence int64
}

type storageArchiveSection struct {
	Kind         string                      `json:"kind"`
	Subject      string                      `json:"subject_id"`
	First        int64                       `json:"first_sequence"`
	Last         int64                       `json:"last_sequence"`
	ID           string                      `json:"section_id"`
	Reference    attachment.ArchiveReference `json:"reference"`
	localAttempt int64
}

type storageArchiveRoot struct {
	SchemaVersion        int                     `json:"schema_version"`
	Organization         string                  `json:"organization_id"`
	Project              string                  `json:"project_id"`
	Item                 string                  `json:"work_item_id"`
	Revision             int64                   `json:"revision"`
	Sequence             int64                   `json:"event_sequence"`
	Body                 string                  `json:"body"`
	ConversationSequence int64                   `json:"conversation_sequence"`
	Title                string                  `json:"title"`
	State                string                  `json:"state"`
	Timeline             []json.RawMessage       `json:"timeline"`
	Sections             []storageArchiveSection `json:"sections"`
}

func (s *Service) storageArchiveContent() entryauthority.ContentHandler {
	if s.config.content != nil {
		return s.config.content
	}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		return s.config.Hosted.SharedEntry.Content
	}
	return nil
}

func (s *Service) archiveClosedNativeIssues(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
	defer cancel()
	call := s.storageArchiveContent()
	if call == nil {
		return nil
	}
	ids, err := nativePageIDs(ctx, s.database.reader, `SELECT i.native_id FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.profile='native' AND p.deleted_at IS NULL AND i.terminal_entered_at IS NULL AND EXISTS(SELECT 1 FROM workflow_states w WHERE w.id=i.workflow_state_id AND w.terminal=1) AND `+nativeTerminalEnteredAt+` IS NOT NULL ORDER BY i.id LIMIT 4`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.database.db.ExecContext(ctx, `UPDATE issues AS i SET terminal_entered_at=`+nativeTerminalEnteredAt+` WHERE native_id=? AND terminal_entered_at IS NULL AND EXISTS(SELECT 1 FROM workflow_states w WHERE w.id=i.workflow_state_id AND w.terminal=1)`, id); err != nil {
			return err
		}
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT i.organization_id,i.project_id,i.native_id,i.revision,i.event_sequence,`+storageArchiveConversationSequence+` FROM issues i JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id LEFT JOIN issue_storage_archives a ON a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id
LEFT JOIN storage_conversion_progress progress ON progress.organization_id=i.organization_id AND progress.project_id=i.project_id AND progress.family='issue_archives' AND progress.aggregate_id=i.native_id
WHERE p.profile='native' AND p.deleted_at IS NULL AND julianday(i.terminal_entered_at)<=julianday(?) AND `+storageArchiveSafe+`
AND (a.work_item_id IS NULL OR a.issue_revision<>i.revision OR a.event_sequence<>i.event_sequence OR a.conversation_sequence<>`+storageArchiveConversationSequence+`)
ORDER BY coalesce(progress.updated_at,''),i.terminal_entered_at,i.id LIMIT 4`, formatHubTime(now.Add(-issueStorageArchiveAge)), formatHubTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	var candidates []storageArchiveCandidate
	for rows.Next() {
		var c storageArchiveCandidate
		if err := rows.Scan(&c.scope.organization, &c.scope.project, &c.item, &c.revision, &c.sequence, &c.conversationSequence); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, c := range candidates {
		if _, err := s.database.db.ExecContext(ctx, `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at) VALUES(?,?,'issue_archives',?,?,0,?)
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET complete=0,updated_at=excluded.updated_at`, c.scope.organization, c.scope.project, c.item, storageConversionVersion, formatHubTime(now)); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.publishStorageArchive(ctx, c, now, call); err != nil {
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return failures
			}
		}
	}
	return errors.Join(failures, s.retireStorageArchiveSections(ctx, now, call))
}

func archiveTable(ctx context.Context, q nativeQueryer, table, condition string, args ...any) (attachment.ArchiveTable, error) {
	order := "rowid"
	if table == "conversation_messages" {
		order = "seq"
	}
	rows, err := q.QueryContext(ctx, "SELECT rowid AS archive_rowid,* FROM "+table+" WHERE "+condition+" ORDER BY "+order, args...)
	if err != nil {
		return attachment.ArchiveTable{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return attachment.ArchiveTable{}, err
	}
	columns[0] = "rowid"
	result := attachment.ArchiveTable{Name: table, Columns: columns, Rows: [][]json.RawMessage{}}
	size := 0
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return result, err
		}
		row := make([]json.RawMessage, len(columns))
		rowSize := 0
		for i, value := range values {
			if data, ok := value.([]byte); ok {
				value = string(data)
			}
			row[i], err = json.Marshal(value)
			if err != nil {
				return result, err
			}
			rowSize += len(row[i]) + 1
		}
		if table == "conversation_messages" && size+rowSize > attachment.MaxSectionBytes/2 && len(result.Rows) > 0 {
			break
		}
		size += rowSize
		if size > attachment.MaxArchiveBytes {
			return result, attachment.ErrTooLarge
		}
		result.Rows = append(result.Rows, row)
	}
	return result, rows.Err()
}

func (s *Service) publishStorageArchive(ctx context.Context, c storageArchiveCandidate, now time.Time, call entryauthority.ContentHandler) error {
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	issue, _, err := readNativeIssue(ctx, tx, c.scope, c.item)
	if err != nil {
		return err
	}
	if int64(issue.Revision) != c.revision {
		return nil
	}
	root := storageArchiveRoot{SchemaVersion: 1, Organization: string(c.scope.organization), Project: string(c.scope.project), Item: c.item, Revision: c.revision, Sequence: c.sequence, ConversationSequence: c.conversationSequence, Body: issue.Body, Title: issue.Title, State: issue.State, Timeline: []json.RawMessage{}, Sections: []storageArchiveSection{}}
	rows, err := tx.QueryContext(ctx, `SELECT n.kind,n.subject_id,n.first_sequence,n.last_sequence,n.section_id,n.local_attempt_id,a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class FROM native_archive_sections n JOIN attachments a ON a.id=n.attachment_id WHERE n.organization_id=? AND n.project_id=? AND n.work_item_id=? AND (n.kind<>'conversation' OR EXISTS(SELECT 1 FROM conversations c WHERE c.id=n.subject_id AND c.visibility='shared')) ORDER BY n.kind,n.subject_id,n.first_sequence`, c.scope.organization, c.scope.project, c.item)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d storageArchiveSection
		if err := rows.Scan(&d.Kind, &d.Subject, &d.First, &d.Last, &d.ID, &d.localAttempt, &d.Reference.Key, &d.Reference.SHA256, &d.Reference.CompressedSize, &d.Reference.UncompressedSize, &d.Reference.Codec, &d.Reference.SchemaVersion, &d.Reference.Kind, &d.Reference.Retention); err != nil {
			return err
		}
		root.Sections = append(root.Sections, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var fresh []storageArchiveSection
	add := func(kind, subject string, first, last int64, tables ...attachment.ArchiveTable) error {
		count := 0
		for _, table := range tables {
			count += len(table.Rows)
		}
		if count == 0 {
			return nil
		}
		id := kind + ":" + subject + ":" + strconv.FormatInt(first, 10)
		section := attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: root.Organization, ProjectID: root.Project, WorkItemID: c.item, SectionID: id, Tables: tables}
		raw, err := json.Marshal(section)
		if err != nil {
			return err
		}
		if len(raw) > attachment.MaxSectionBytes {
			return attachment.ErrTooLarge
		}
		ref, data, err := attachment.PrepareArchive(root.Organization, 1, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		if _, err := call(ctx, entryauthority.ContentRequest{Organization: root.Organization, Reference: ref, Data: data, Write: true}); err != nil {
			return err
		}
		d := storageArchiveSection{Kind: kind, Subject: subject, First: first, Last: last, ID: id, Reference: ref}
		if kind == "run" {
			for _, table := range tables {
				index := slices.Index(table.Columns, "local_attempt_id")
				if table.Name == "native_attempts" && index >= 0 && len(table.Rows) > 0 {
					if err := json.Unmarshal(table.Rows[0][index], &d.localAttempt); err != nil {
						return err
					}
				}
			}
		}
		fresh = append(fresh, d)
		root.Sections = append(root.Sections, d)
		if len(fresh) > issueStorageArchiveBatch {
			return attachment.ErrTooLarge
		}
		return nil
	}
	last := func(kind, subject string) int64 {
		var value int64 = -1
		for _, d := range root.Sections {
			if d.Kind == kind && d.Subject == subject {
				value = max(value, d.Last)
			}
		}
		return value
	}
	addTables := func(kind, subject string, tables ...attachment.ArchiveTable) error {
		var first, end int64
		var part []attachment.ArchiveTable
		size := 0
		flush := func() error {
			if len(part) == 0 {
				return nil
			}
			if err := add(kind, subject, first, end, part...); err != nil {
				return err
			}
			part = nil
			size = 0
			return nil
		}
		for _, table := range tables {
			index := 0
			switch table.Name {
			case "conversation_messages", "conversation_events":
				index = slices.Index(table.Columns, "seq")
			case "collaboration_versions":
				index = slices.Index(table.Columns, "revision")
			case "collaboration_events":
				index = slices.Index(table.Columns, "sequence")
			}
			for _, row := range table.Rows {
				raw, err := json.Marshal(row)
				if err != nil {
					return err
				}
				if len(raw) > attachment.MaxSectionBytes-(64<<10) {
					return attachment.ErrTooLarge
				}
				if size+len(raw) > attachment.MaxSectionBytes/2 {
					if err := flush(); err != nil {
						return err
					}
				}
				var sequence int64
				if err := json.Unmarshal(row[index], &sequence); err != nil {
					return err
				}
				if len(part) == 0 {
					first = sequence
					if kind == "history" {
						first = max(last(kind, subject)+1, 0)
					}
				}
				end = sequence
				if len(part) == 0 || part[len(part)-1].Name != table.Name {
					part = append(part, attachment.ArchiveTable{Name: table.Name, Columns: table.Columns, Rows: [][]json.RawMessage{}})
				}
				part[len(part)-1].Rows = append(part[len(part)-1].Rows, row)
				size += len(raw)
			}
			if err := flush(); err != nil {
				return err
			}
		}
		return nil
	}
	var private bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE organization_id=? AND project_id=? AND (work_item_id=? OR subject_work_item_id=?) AND visibility<>'shared')`, c.scope.organization, c.scope.project, c.item, c.item).Scan(&private); err != nil {
		return err
	}
	if !private {
		attempts, err := nativePageIDs(ctx, tx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token`, c.scope.organization, c.scope.project, c.item)
		if err != nil {
			return err
		}
		for _, id := range attempts {
			if last("run", id) >= 0 {
				continue
			}
			table, err := archiveTable(ctx, tx, "native_attempts", "id=?", id)
			if err != nil {
				return err
			}
			if err := add("run", id, 0, 0, table); err != nil {
				return err
			}
		}
	}
	conversations, err := nativePageIDs(ctx, tx, `SELECT id FROM conversations WHERE organization_id=? AND project_id=? AND work_item_id=? AND visibility='shared' ORDER BY id`, c.scope.organization, c.scope.project, c.item)
	if err != nil {
		return err
	}
	for _, id := range conversations {
		for {
			table, err := archiveTable(ctx, tx, "conversation_messages", "conversation_id=? AND seq>?", id, last("conversation", id))
			if err != nil {
				return err
			}
			if len(table.Rows) == 0 {
				break
			}
			if err := addTables("conversation", id, table); err != nil {
				return err
			}
		}
	}

	if !private {
		history, err := archiveTable(ctx, tx, "collaboration_events", `organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND type IN ('run.started','run.checkpointed','run.observed','run.finished')`, c.scope.organization, c.scope.project, c.item, last("history", c.item))
		if err != nil {
			return err
		}
		if err := addTables("history", c.item, history); err != nil {
			return err
		}
		changes, err := nativePageIDs(ctx, tx, `SELECT c.id FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE l.organization_id=? AND l.project_id=? AND l.work_item_id=? AND NOT EXISTS(SELECT 1 FROM change_issue_links other WHERE other.change_id=c.id AND (other.organization_id<>l.organization_id OR other.project_id<>l.project_id OR other.work_item_id<>l.work_item_id)) ORDER BY c.id`, c.scope.organization, c.scope.project, c.item)
		if err != nil {
			return err
		}
		for _, id := range changes {
			if last("change", id) >= 0 {
				continue
			}
			versions, err := archiveTable(ctx, tx, "change_versions", "change_id=?", id)
			if err != nil {
				return err
			}
			evidence, err := archiveTable(ctx, tx, "change_evidence", "change_id=?", id)
			if err != nil {
				return err
			}
			if err := add("change", id, 0, 0, versions, evidence); err != nil {
				return err
			}
		}
	}
	comments, err := nativePageIDs(ctx, tx, `SELECT record_id FROM collaboration_versions WHERE organization_id=? AND project_id=? AND work_item_id=? AND record_id<>? GROUP BY record_id`, c.scope.organization, c.scope.project, c.item, c.item)
	if err != nil {
		return err
	}
	for _, id := range comments {
		var boundary int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(revision),0) FROM collaboration_versions WHERE organization_id=? AND project_id=? AND work_item_id=? AND record_id=? AND encoding='snapshot'`, c.scope.organization, c.scope.project, c.item, id).Scan(&boundary); err != nil {
			return err
		}
		table, err := archiveTable(ctx, tx, "collaboration_versions", "organization_id=? AND project_id=? AND work_item_id=? AND record_id=? AND revision>? AND revision<?", c.scope.organization, c.scope.project, c.item, id, last("revision", id), boundary)
		if err != nil {
			return err
		}
		revisionColumn := slices.Index(table.Columns, "revision")
		recordColumn := slices.Index(table.Columns, "record_json")
		encodingColumn := slices.Index(table.Columns, "encoding")
		for _, row := range table.Rows {
			var revision int64
			if err := json.Unmarshal(row[revisionColumn], &revision); err != nil {
				return err
			}
			raw, err := reconstructNativeVersion(ctx, tx, string(c.scope.organization), string(c.scope.project), c.item, id, revision)
			if err != nil {
				return err
			}
			row[recordColumn], err = json.Marshal(string(raw))
			if err != nil {
				return err
			}
			row[encodingColumn] = json.RawMessage(`"snapshot"`)
			part := attachment.ArchiveTable{Name: table.Name, Columns: table.Columns, Rows: [][]json.RawMessage{row}}
			if err := add("revision", id, revision, revision, part); err != nil {
				return err
			}
		}
	}

	rows, err = tx.QueryContext(ctx, `SELECT json_object('sequence',sequence,'type',type,'actor',json(actor_json),'recorded_at',recorded_at,'from_state',json_extract(data_json,'$.from_state'),'to_state',json_extract(data_json,'$.to_state'),'attempt_id',json_extract(data_json,'$.run.attempt_id'),'outcome',json_extract(data_json,'$.run.outcome'),'landed_commit',json_extract(data_json,'$.run.finalization.publication.external.head_sha')) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type IN ('workflow.transitioned','run.started','run.finished') ORDER BY sequence`, c.scope.organization, c.scope.project, c.item)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		root.Timeline = append(root.Timeline, json.RawMessage(raw))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT json_object('type','change.landed','recorded_at',json_extract(c.record_json,'$.landed.landed_at'),'actor',json_extract(c.record_json,'$.landed.actor'),'landed_commit',json_extract(c.record_json,'$.landed.merge_sha'),'change_id',c.id) FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE l.organization_id=? AND l.project_id=? AND l.work_item_id=? AND json_extract(c.record_json,'$.landed.merge_sha') IS NOT NULL ORDER BY c.id`, c.scope.organization, c.scope.project, c.item)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		root.Timeline = append(root.Timeline, json.RawMessage(raw))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return err
	}
	if len(raw) > attachment.MaxSectionBytes {
		return attachment.ErrTooLarge
	}
	ref, data, err := attachment.PrepareArchive(root.Organization, 1, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	ref.Key, err = attachment.IssueArchiveKey(root.Organization, root.Project, c.item, c.revision, ref.SHA256)
	if err != nil {
		return err
	}
	if _, err := call(ctx, entryauthority.ContentRequest{Organization: root.Organization, Reference: ref, Data: data, Write: true}); err != nil {
		return err
	}
	write, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer write.Rollback()
	valid, err := storageArchiveCurrent(ctx, write, c, now)
	if err != nil || !valid {
		return err
	}
	for _, d := range fresh {
		id, err := recordStorageArchiveObject(ctx, write, c, d.Reference, now)
		if err != nil {
			return err
		}
		if _, err := write.ExecContext(ctx, `INSERT INTO native_archive_sections(organization_id,project_id,work_item_id,kind,subject_id,first_sequence,last_sequence,section_id,attachment_id,local_attempt_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, c.scope.organization, c.scope.project, c.item, d.Kind, d.Subject, d.First, d.Last, d.ID, id, d.localAttempt); err != nil {
			return err
		}
	}
	id, err := recordStorageArchiveObject(ctx, write, c, ref, now)
	if err != nil {
		return err
	}
	if _, err := write.ExecContext(ctx, `INSERT INTO issue_storage_archives(organization_id,project_id,work_item_id,issue_revision,event_sequence,conversation_sequence,attachment_id,published_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET issue_revision=excluded.issue_revision,event_sequence=excluded.event_sequence,conversation_sequence=excluded.conversation_sequence,attachment_id=excluded.attachment_id,published_at=excluded.published_at`, c.scope.organization, c.scope.project, c.item, c.revision, c.sequence, c.conversationSequence, id, formatHubTime(now)); err != nil {
		return err
	}
	if _, err := write.ExecContext(ctx, `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family='issue_archives' AND aggregate_id=?`, formatHubTime(now), c.scope.organization, c.scope.project, c.item); err != nil {
		return err
	}
	return s.database.commit(ctx, write)
}

func storageArchiveCurrent(ctx context.Context, q nativeQueryer, c storageArchiveCandidate, now time.Time) (bool, error) {
	var valid bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND i.revision=? AND i.event_sequence=? AND `+storageArchiveConversationSequence+`=? AND julianday(i.terminal_entered_at)<=julianday(?) AND `+storageArchiveSafe+`)`, c.scope.organization, c.scope.project, c.item, c.revision, c.sequence, c.conversationSequence, formatHubTime(now.Add(-issueStorageArchiveAge)), formatHubTime(now)).Scan(&valid)
	return valid, err
}

func recordStorageArchiveObject(ctx context.Context, tx *sql.Tx, c storageArchiveCandidate, ref attachment.ArchiveReference, now time.Time) (string, error) {
	if ref.Validate(string(c.scope.organization)) != nil {
		return "", attachment.ErrInvalid
	}
	id := "att_" + artifact.Digest([]byte(string(c.scope.organization) + "\n" + ref.Key))[:32]
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version) VALUES(?,?,?,(SELECT token_id FROM token_grants WHERE organization_id=? AND project_id=? ORDER BY token_id LIMIT 1),'archive.gz','application/gzip',?,?,?,'archive',?,?,?,?,?) ON CONFLICT DO NOTHING`, id, c.scope.organization, c.scope.project, c.scope.organization, c.scope.project, ref.CompressedSize, ref.SHA256, formatHubTime(now), ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, c.scope.organization, c.scope.project, c.item, id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) retireStorageArchiveSections(ctx context.Context, now time.Time, call entryauthority.ContentHandler) error {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT n.organization_id,n.project_id,n.work_item_id,p.issue_revision,p.event_sequence,p.conversation_sequence,n.kind,n.first_sequence,n.last_sequence,n.section_id,n.subject_id,n.retired_rows,a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class FROM native_archive_sections n JOIN issue_storage_archives p USING(organization_id,project_id,work_item_id) JOIN attachments a ON a.id=n.attachment_id JOIN issues i ON i.organization_id=n.organization_id AND i.project_id=n.project_id AND i.native_id=n.work_item_id WHERE n.retirement_complete=0 AND (n.kind<>'conversation' OR EXISTS(SELECT 1 FROM conversations c WHERE c.id=n.subject_id AND c.organization_id=n.organization_id AND c.project_id=n.project_id AND c.work_item_id=n.work_item_id AND c.visibility='shared')) AND i.revision=p.issue_revision AND i.event_sequence=p.event_sequence AND `+storageArchiveSafe+` ORDER BY p.published_at,n.section_id LIMIT 4`, formatHubTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		c                 storageArchiveCandidate
		kind, id, subject string
		offset            int
		first, last       int64
		ref               attachment.ArchiveReference
	}
	var work []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.c.scope.organization, &p.c.scope.project, &p.c.item, &p.c.revision, &p.c.sequence, &p.c.conversationSequence, &p.kind, &p.first, &p.last, &p.id, &p.subject, &p.offset, &p.ref.Key, &p.ref.SHA256, &p.ref.CompressedSize, &p.ref.UncompressedSize, &p.ref.Codec, &p.ref.SchemaVersion, &p.ref.Kind, &p.ref.Retention); err != nil {
			return err
		}
		work = append(work, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, p := range work {
		data, err := call(ctx, entryauthority.ContentRequest{Organization: string(p.c.scope.organization), Reference: p.ref})
		if err != nil {
			return err
		}
		section, err := attachment.DecodeArchiveSection(string(p.c.scope.organization), p.ref, data)
		if err != nil {
			return err
		}
		if section.OrganizationID != string(p.c.scope.organization) || section.ProjectID != string(p.c.scope.project) || section.WorkItemID != p.c.item || section.SectionID != p.id {
			return attachment.ErrInvalid
		}
		if err := validateNativeArchiveSection(section, p.kind, p.subject, p.first, p.last); err != nil {
			return err
		}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		valid, err := storageArchiveCurrent(ctx, tx, p.c, now)
		if err != nil || !valid {
			tx.Rollback()
			if err != nil {
				return err
			}
			continue
		}
		if p.kind == "revision" {
			if _, err := tx.ExecContext(ctx, "DROP TRIGGER collaboration_versions_no_delete"); err != nil {
				tx.Rollback()
				return err
			}
		}
		offset, processed, total := 0, 0, 0
		for _, table := range section.Tables {
			total += len(table.Rows)
		}
		for _, table := range section.Tables {
			for _, row := range table.Rows {
				offset++
				if offset <= p.offset || processed >= issueStorageArchiveBatch {
					continue
				}
				var rowid int64
				if err := json.Unmarshal(row[0], &rowid); err != nil {
					tx.Rollback()
					return err
				}
				var statement string
				args := []any{rowid, p.subject, p.c.scope.organization, p.c.scope.project, p.c.item}
				switch table.Name {
				case "native_attempts":
					processed++
					continue
				case "conversation_messages":
					index := slices.Index(table.Columns, "id")
					var id string
					if index < 0 || json.Unmarshal(row[index], &id) != nil {
						tx.Rollback()
						return attachment.ErrInvalid
					}
					statement = `DELETE FROM conversation_messages WHERE rowid=? AND conversation_id=? AND id=? AND NOT EXISTS(SELECT 1 FROM message_references r WHERE r.message_id=conversation_messages.id) AND NOT EXISTS(SELECT 1 FROM conversation_attachments a WHERE a.message_id=conversation_messages.id) AND EXISTS(SELECT 1 FROM conversations c WHERE c.id=conversation_id AND c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND c.visibility='shared')`
					args = []any{rowid, p.subject, id, p.c.scope.organization, p.c.scope.project, p.c.item}
				case "collaboration_versions":
					var revision int64
					index := slices.Index(table.Columns, "revision")
					if index < 0 || json.Unmarshal(row[index], &revision) != nil {
						tx.Rollback()
						return attachment.ErrInvalid
					}
					statement = `DELETE FROM collaboration_versions WHERE rowid=? AND record_id=? AND organization_id=? AND project_id=? AND work_item_id=? AND revision=?`
					args = append(args, revision)
				case "collaboration_events", "change_versions", "change_evidence":
					processed++
					continue
				default:
					tx.Rollback()
					return fmt.Errorf("unsupported archive retirement table %q", table.Name)
				}
				if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
					tx.Rollback()
					return err
				}
				processed++
			}
		}
		if p.kind == "revision" {
			if _, err := tx.ExecContext(ctx, `CREATE TRIGGER collaboration_versions_no_delete BEFORE DELETE ON collaboration_versions BEGIN SELECT RAISE(ABORT,'collaboration history is append-only'); END`); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE native_archive_sections SET retired_rows=?,retirement_complete=? WHERE organization_id=? AND project_id=? AND work_item_id=? AND section_id=?`, p.offset+processed, p.offset+processed >= total, p.c.scope.organization, p.c.scope.project, p.c.item, p.id); err != nil {
			tx.Rollback()
			return err
		}
		if err := s.database.commit(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}
