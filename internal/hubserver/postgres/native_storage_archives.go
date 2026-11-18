package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const storageArchiveHistoryBatch = 32

const storageArchiveConversationSequence = `COALESCE((SELECT c.event_seq+c.revision FROM conversations c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.visibility='shared' AND c.deleted_at IS NULL),0)`
const storageArchiveSafe = `i.terminal_entered_at IS NOT NULL AND i.terminal_entered_at<=$2
AND EXISTS (SELECT 1 FROM projects p WHERE p.organization_id=i.organization_id AND p.id=i.project_id AND p.profile='native' AND p.deleted_at IS NULL)
AND EXISTS (SELECT 1 FROM project_workflow_states w WHERE w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name AND w.terminal)
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>$3)
AND NOT EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.status='running' AND (a.checkpoint_object_key IS NOT NULL AND a.cleanup_acknowledged_at IS NULL
OR NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND (l.released_at IS NOT NULL OR l.expires_at<=$3))))
AND NOT EXISTS (SELECT 1 FROM change_issue_links x JOIN change_requests r ON r.organization_id=x.organization_id AND r.project_id=x.project_id AND r.id=x.change_id WHERE x.organization_id=i.organization_id AND x.project_id=i.project_id AND x.work_item_id=i.native_id AND (r.current_version_id IS NULL OR r.landed_version_id IS DISTINCT FROM r.current_version_id))
AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND (c.work_item_id=i.native_id OR c.subject_work_item_id=i.native_id) AND c.deleted_at IS NULL AND
(c.execution_status IS NULL OR c.execution_lease_id IS NULL OR c.execution_status NOT IN ('idle','completed','interrupted','failed')
OR EXISTS (SELECT 1 FROM conversation_messages m WHERE m.organization_id=c.organization_id AND m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','responding'))
OR EXISTS (SELECT 1 FROM conversation_questions q WHERE q.organization_id=c.organization_id AND q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))))`

func (s *session) StorageArchiveCandidates(ctx context.Context, before time.Time, after attachmentstore.StorageArchiveCursor, limit int) (attachmentstore.StorageArchivePage, error) {
	var page attachmentstore.StorageArchivePage
	if before.IsZero() || limit < 1 || limit > 4 {
		return page, corestore.ErrInvalidPage
	}
	var afterTime any
	if after.ID != 0 {
		if after.TerminalEnteredAt.IsZero() {
			return page, corestore.ErrInvalidPage
		}
		afterTime = after.TerminalEnteredAt
	}
	rows, err := s.db.Query(ctx, `WITH scanned AS MATERIALIZED (
SELECT organization_id,project_id,native_id,revision,event_sequence,terminal_entered_at,id FROM issues WHERE organization_id=$1 AND terminal_entered_at<=$2 AND ($3::timestamptz IS NULL OR (terminal_entered_at,id)>($3,$4)) ORDER BY terminal_entered_at,id LIMIT $5)
SELECT i.project_id,i.native_id,i.revision,i.event_sequence,`+storageArchiveConversationSequence+`,i.terminal_entered_at,i.id FROM scanned i ORDER BY i.terminal_entered_at,i.id`, s.organization, before, afterTime, after.ID, limit)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var stamp attachmentstore.StorageArchiveStamp
		if err := rows.Scan(&stamp.ProjectID, &stamp.WorkItemID, &stamp.Revision, &stamp.EventSequence, &stamp.ConversationSequence, &page.Next.TerminalEnteredAt, &page.Next.ID); err != nil {
			return page, mapError(ctx, err)
		}
		page.Items = append(page.Items, attachmentstore.StorageArchiveCandidate{Stamp: stamp, Cursor: page.Next})
	}
	if len(page.Items) < limit {
		page.Next = attachmentstore.StorageArchiveCursor{}
	}
	return page, mapError(ctx, rows.Err())
}

func (s *session) storageArchiveRecord(ctx context.Context, project tracker.ProjectID, id string) (attachmentstore.ArchiveRecord, error) {
	row, err := scanAttachmentRecord(s.db.QueryRow(ctx, `SELECT `+attachmentRecordColumns+` FROM attachments WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND kind='archive' AND deleted_at IS NULL AND object_deleted_at IS NULL`, s.organization, project, id))
	if err != nil {
		return attachmentstore.ArchiveRecord{}, mapError(ctx, err)
	}
	return attachmentstore.ArchiveRecord{OrganizationID: s.organization, ProjectID: project, ID: id, Uploader: row.Metadata.Uploader, CreatedAt: row.Metadata.CreatedAt, Reference: archiveReference(row)}, nil
}

func (s *session) StorageArchivePointer(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (attachmentstore.StorageArchivePointer, error) {
	result := attachmentstore.StorageArchivePointer{OrganizationID: s.organization, Stamp: attachmentstore.StorageArchiveStamp{ProjectID: project, WorkItemID: item}}
	var id string
	err := s.db.QueryRow(ctx, `SELECT issue_revision,event_sequence,conversation_sequence,root_attachment_id,through_history,last_message_scanned,published_at FROM issue_storage_archives WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, project, item).Scan(&result.Stamp.Revision, &result.Stamp.EventSequence, &result.Stamp.ConversationSequence, &id, &result.ThroughHistory, &result.LastMessageScanned, &result.PublishedAt)
	if err != nil {
		return result, mapError(ctx, err)
	}
	result.Root, err = s.storageArchiveRecord(ctx, project, id)
	return result, err
}

func (s *session) StorageArchiveSection(ctx context.Context, q attachmentstore.StorageArchiveSectionQuery) (attachmentstore.StorageArchiveSection, error) {
	result := attachmentstore.StorageArchiveSection{OrganizationID: s.organization, Stamp: attachmentstore.StorageArchiveStamp{ProjectID: q.ProjectID, WorkItemID: q.WorkItemID}, Kind: q.Kind, Subject: q.Subject}
	var id string
	err := s.db.QueryRow(ctx, `SELECT a.archive_revision,a.source_event_sequence,a.source_conversation_sequence,a.section_id,a.kind,a.subject_id,a.first_sequence,a.last_sequence,a.attachment_id,a.retired_through,a.retirement_complete FROM issue_archive_sections a
WHERE a.organization_id=$1 AND a.project_id=$2 AND a.work_item_id=$3 AND a.kind<>'' AND ($4::text='' OR a.kind=$4) AND ($5::text='' OR a.subject_id=$5) AND ($8::text='' OR a.section_id=$8)
AND ($6::bigint=0 OR a.first_sequence<$6) AND ($7::bigint=0 OR (a.first_sequence<=$7 AND a.last_sequence>=$7))
AND (a.kind<>'conversation_messages' OR EXISTS (SELECT 1 FROM conversations c WHERE c.organization_id=a.organization_id AND c.project_id=a.project_id AND c.id=a.subject_id AND c.work_item_id=a.work_item_id AND c.visibility='shared' AND c.deleted_at IS NULL))
ORDER BY a.last_sequence DESC,a.archive_revision DESC,a.section_id LIMIT 1`, s.organization, q.ProjectID, q.WorkItemID, q.Kind, q.Subject, q.Before, q.At, q.SectionID).Scan(&result.Stamp.Revision, &result.Stamp.EventSequence, &result.Stamp.ConversationSequence, &result.ID, &result.Kind, &result.Subject, &result.First, &result.Last, &id, &result.RetiredThrough, &result.RetirementComplete)
	if err != nil {
		return result, mapError(ctx, err)
	}
	result.Archive, err = s.storageArchiveRecord(ctx, q.ProjectID, id)
	return result, err
}

func (s *session) StorageArchiveRetirements(ctx context.Context, after attachmentstore.StorageArchiveRetirementCursor, limit int) (attachmentstore.StorageArchiveRetirementPage, error) {
	var page attachmentstore.StorageArchiveRetirementPage
	if limit < 1 || limit > 4 {
		return page, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT project_id,work_item_id,archive_revision,source_event_sequence,source_conversation_sequence,section_id,kind,subject_id,first_sequence,last_sequence,attachment_id,retired_through FROM issue_archive_sections
WHERE organization_id=$1 AND kind='conversation_messages' AND NOT retirement_complete AND ($3::text='' OR (project_id,work_item_id,archive_revision,section_id)>($3,$4,$5,$6))
ORDER BY project_id,work_item_id,archive_revision,section_id LIMIT $2`, s.organization, limit, after.ProjectID, after.WorkItemID, after.Revision, after.SectionID)
	if err != nil {
		return page, mapError(ctx, err)
	}
	type pending struct {
		section    attachmentstore.StorageArchiveSection
		attachment string
	}
	var found []pending
	for rows.Next() {
		entry := pending{section: attachmentstore.StorageArchiveSection{OrganizationID: s.organization}}
		p := &entry.section
		if err := rows.Scan(&p.Stamp.ProjectID, &p.Stamp.WorkItemID, &p.Stamp.Revision, &p.Stamp.EventSequence, &p.Stamp.ConversationSequence, &p.ID, &p.Kind, &p.Subject, &p.First, &p.Last, &entry.attachment, &p.RetiredThrough); err != nil {
			rows.Close()
			return page, mapError(ctx, err)
		}
		found = append(found, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, mapError(ctx, err)
	}
	for _, entry := range found {
		entry.section.Archive, err = s.storageArchiveRecord(ctx, entry.section.Stamp.ProjectID, entry.attachment)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, entry.section)
	}
	if len(page.Items) == limit {
		last := page.Items[len(page.Items)-1]
		page.Next = attachmentstore.StorageArchiveRetirementCursor{ProjectID: last.Stamp.ProjectID, WorkItemID: last.Stamp.WorkItemID, Revision: last.Stamp.Revision, SectionID: last.ID}
	}
	return page, nil
}

const (
	storageArchiveRead      = ""
	storageArchiveWriteLock = " FOR NO KEY UPDATE OF i"
	storageArchiveKeyLock   = " FOR UPDATE OF i"
)

func (s *session) storageArchiveCurrentSource(ctx context.Context, stamp attachmentstore.StorageArchiveStamp, before time.Time, lock string) (attachmentstore.StorageArchiveStamp, error) {
	now, err := s.leaseTime(ctx)
	if err != nil {
		return attachmentstore.StorageArchiveStamp{}, err
	}
	current := attachmentstore.StorageArchiveStamp{ProjectID: stamp.ProjectID, WorkItemID: stamp.WorkItemID}
	err = s.db.QueryRow(ctx, `SELECT i.revision,i.event_sequence,`+storageArchiveConversationSequence+` FROM issues i WHERE i.organization_id=$1 AND `+storageArchiveSafe+` AND i.project_id=$4 AND i.native_id=$5`+lock, s.organization, before, now, stamp.ProjectID, stamp.WorkItemID).Scan(&current.Revision, &current.EventSequence, &current.ConversationSequence)
	if err != nil {
		return current, mapError(ctx, err)
	}
	return current, nil
}

func (s *session) storageArchiveSource(ctx context.Context, stamp attachmentstore.StorageArchiveStamp, before time.Time, lock string) (attachmentstore.StorageArchiveStamp, error) {
	current, err := s.storageArchiveCurrentSource(ctx, stamp, before, lock)
	if err != nil {
		return current, err
	}
	if current != stamp {
		return current, corestore.ErrRevisionConflict
	}
	return current, nil
}

func (s *session) StorageArchiveSnapshot(ctx context.Context, stamp attachmentstore.StorageArchiveStamp, before time.Time, limit int) (attachmentstore.StorageArchiveSnapshot, error) {
	result := attachmentstore.StorageArchiveSnapshot{Stamp: stamp}
	if before.IsZero() || limit < 1 || limit > 500 {
		return result, corestore.ErrInvalidPage
	}
	if _, err := s.storageArchiveSource(ctx, stamp, before, storageArchiveRead); err != nil {
		return result, err
	}
	var err error
	result.Item, err = s.WorkItem(ctx, stamp.ProjectID, stamp.WorkItemID)
	if err != nil {
		return result, err
	}
	pointer, err := s.StorageArchivePointer(ctx, stamp.ProjectID, stamp.WorkItemID)
	if err == nil {
		result.Previous = &pointer
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return result, err
	}
	var through int64
	if result.Previous != nil {
		through = result.Previous.ThroughHistory
	}
	historyLimit := min(storageArchiveHistoryBatch, limit)
	result.History, err = s.HistoryEvents(ctx, stamp.ProjectID, stamp.WorkItemID, through, historyLimit)
	if err != nil {
		return result, err
	}
	result.ThroughHistory = stamp.EventSequence
	if len(result.History) == historyLimit {
		result.ThroughHistory = result.History[len(result.History)-1].Sequence
	}

	attempt, err := s.LatestAttempt(ctx, result.Item.ID)
	if err == nil {
		result.Attempt = &attempt
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return result, err
	}
	var id string
	err = s.db.QueryRow(ctx, `SELECT id FROM conversations WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND visibility='shared' AND deleted_at IS NULL`, s.organization, stamp.ProjectID, stamp.WorkItemID).Scan(&id)
	if err != nil {
		err = mapError(ctx, err)
		if errors.Is(err, corestore.ErrNotFound) {
			return result, nil
		}
		return result, err
	}
	conversation, err := s.Conversation(ctx, stamp.ProjectID, id)
	if err != nil {
		return result, err
	}
	result.Conversation = &conversation
	afterMessage := int64(0)
	if result.Previous != nil && result.Previous.Stamp == stamp {
		afterMessage = result.Previous.LastMessageScanned
	}
	rows, err := s.db.Query(ctx, `WITH scanned AS MATERIALIZED (
SELECT * FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND seq>$5 ORDER BY seq LIMIT $6)
SELECT m.organization_id,m.conversation_id,m.id,m.seq,m.role,m.kind,m.text,m.data_json,m.delivery,m.attempt_id,m.thread_id,m.turn_id,m.provider_item_id,m.actor_json,m.command_key,m.created_at,m.updated_at,m.content_attachment_id,m.revision,COALESCE(m.text_bytes,0),m.stored_text_bytes,m.tool_name,m.tool_item_id,m.tool_status,m.operator_action_id,m.operator_action_agent,m.operator_action_result,m.actor_kind,m.actor_principal_id,EXISTS (SELECT 1 FROM issue_archive_message_payloads r WHERE r.organization_id=$1 AND r.project_id=$3 AND r.work_item_id=$4 AND r.conversation_id=$2 AND r.message_id=m.id AND r.message_revision=m.revision AND (r.expected_reference=m.content_attachment_id OR r.content_id=m.content_attachment_id))
FROM scanned m ORDER BY m.seq`, s.organization, id, stamp.ProjectID, stamp.WorkItemID, afterMessage, limit)
	if err != nil {
		return result, mapError(ctx, err)
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		row := &storageArchiveMessageRow{row: rows}
		message, err := scanPGMessage(row)
		if err != nil {
			return result, err
		}
		scanned++
		result.LastMessageScanned = message.Sequence
		if !row.published || result.Previous == nil {
			result.Messages = append(result.Messages, message)
		}
	}
	if scanned < limit {
		result.LastMessageScanned = 0
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, mapError(ctx, err)
	}
	ids := make([]string, 0, len(result.Messages))
	seen := make(map[string]bool, len(result.Messages))
	for _, message := range result.Messages {
		if message.ContentReference != "" && !seen[message.ContentReference] {
			ids = append(ids, message.ContentReference)
			seen[message.ContentReference] = true
		}
	}
	if len(ids) > 0 {
		objects, err := s.db.Query(ctx, `SELECT organization_id,id,archive_id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM conversation_content_objects WHERE organization_id=$1 AND id=ANY($2::text[]) LIMIT $3`, s.organization, ids, len(ids))
		if err != nil {
			return result, mapError(ctx, err)
		}
		for objects.Next() {
			var object conversationstore.ContentObject
			ref := &object.Reference
			if err := objects.Scan(&object.OrganizationID, &object.ID, &object.ArchiveID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt); err != nil {
				objects.Close()
				return result, mapError(ctx, err)
			}
			if err := ref.Validate(string(s.organization)); err != nil {
				objects.Close()
				return result, err
			}
			result.Contents = append(result.Contents, object)
		}
		err = objects.Err()
		objects.Close()
		if err != nil {
			return result, mapError(ctx, err)
		}
		if len(result.Contents) != len(ids) {
			return result, corestore.ErrInvalidPage
		}
	}
	return result, nil
}

type storageArchiveMessageRow struct {
	row       interface{ Scan(...any) error }
	published bool
}

func (r *storageArchiveMessageRow) Scan(destinations ...any) error {
	return r.row.Scan(append(destinations, &r.published)...)
}

func (s *session) publishStorageArchiveObject(ctx context.Context, record attachmentstore.ArchiveRecord, item tracker.NativeWorkItemID) error {
	if record.OrganizationID != s.organization {
		return corestore.ErrInvalidPage
	}
	if _, err := s.PrepareAttachmentArchive(ctx, record); err != nil {
		return err
	}
	if err := s.PublishAttachmentArchive(ctx, record, item); err != nil {
		return err
	}
	object, err := s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: record.ID, Reference: record.Reference, CreatedAt: record.CreatedAt})
	if err != nil {
		return err
	}
	return s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: object.ID, Kind: "issue-storage-archive", ID: record.ID, CreatedAt: record.CreatedAt})
}

func (s *session) PublishStorageArchive(ctx context.Context, publication attachmentstore.StorageArchivePublication, before time.Time) (bool, error) {
	stamp := publication.Stamp
	if len(publication.MessageSources) > 500 || len(publication.MessagePayloads) > 500 || len(publication.Sections) > 4 || publication.PublishedAt.IsZero() || publication.Root.ProjectID != stamp.ProjectID || publication.LastMessageScanned < 0 || publication.ExpectedMessageCursor < 0 || publication.ThroughHistory < 0 || publication.ThroughHistory > stamp.EventSequence {
		return false, corestore.ErrInvalidPage
	}
	if _, err := s.storageArchiveSource(ctx, stamp, before, storageArchiveWriteLock); err != nil {
		return false, err
	}
	var conversationID string
	err := s.db.QueryRow(ctx, `SELECT id FROM conversations WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND visibility='shared' AND deleted_at IS NULL FOR NO KEY UPDATE`, s.organization, stamp.ProjectID, stamp.WorkItemID).Scan(&conversationID)
	if err != nil && !errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return false, mapError(ctx, err)
	}
	if err := s.LockArchiveAccounting(ctx); err != nil {
		return false, err
	}
	if _, err := s.storageArchiveSource(ctx, stamp, before, storageArchiveKeyLock); err != nil {
		return false, err
	}
	if conversationID != "" {
		var locked string
		if err := s.db.QueryRow(ctx, `SELECT id FROM conversations WHERE organization_id=$1 AND id=$2 AND work_item_id=$3 AND visibility='shared' AND deleted_at IS NULL FOR UPDATE`, s.organization, conversationID, stamp.WorkItemID).Scan(&locked); err != nil {
			if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
				return false, corestore.ErrRevisionConflict
			}
			return false, mapError(ctx, err)
		}
	}
	sources := make(map[string]attachmentstore.StorageArchiveMessageSource, len(publication.MessageSources))
	ids := make([]string, 0, len(publication.MessageSources))
	sequences := make([]int64, 0, len(publication.MessageSources))
	revisions := make([]int64, 0, len(publication.MessageSources))
	references := make([]string, 0, len(publication.MessageSources))
	for _, source := range publication.MessageSources {
		if source.ConversationID != conversationID || source.MessageID == "" || source.Sequence < 1 || source.Revision < 1 {
			return false, corestore.ErrInvalidPage
		}
		if _, exists := sources[source.MessageID]; exists {
			return false, corestore.ErrInvalidPage
		}
		sources[source.MessageID] = source
		ids = append(ids, source.MessageID)
		sequences = append(sequences, source.Sequence)
		revisions = append(revisions, source.Revision)
		references = append(references, source.ContentReference)
	}
	if len(ids) > 0 {
		var matched int
		err = s.db.QueryRow(ctx, `SELECT count(*) FROM unnest($3::text[],$4::bigint[],$5::bigint[],$6::text[]) AS source(id,sequence,revision,reference)
JOIN conversation_messages m ON m.organization_id=$1 AND m.conversation_id=$2 AND m.id=source.id AND m.seq=source.sequence AND m.revision=source.revision AND m.content_attachment_id=source.reference`, s.organization, conversationID, ids, sequences, revisions, references).Scan(&matched)
		if err != nil {
			return false, mapError(ctx, err)
		}
		if matched != len(ids) {
			return false, corestore.ErrRevisionConflict
		}
	}
	var priorRoot string
	var priorCursor int64
	err = s.db.QueryRow(ctx, `SELECT root_attachment_id,last_message_scanned FROM issue_storage_archives WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 FOR UPDATE`, s.organization, stamp.ProjectID, stamp.WorkItemID).Scan(&priorRoot, &priorCursor)
	if err != nil && !errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return false, mapError(ctx, err)
	}
	if priorRoot != publication.ExpectedRootID || priorCursor != publication.ExpectedMessageCursor {
		return false, corestore.ErrRevisionConflict
	}
	if err := s.publishStorageArchiveObject(ctx, publication.Root, stamp.WorkItemID); err != nil {
		return false, err
	}
	for _, section := range publication.Sections {
		if section.OrganizationID != s.organization || section.Stamp != stamp || section.Kind != "conversation_messages" || section.Subject != conversationID || section.First < 1 || section.Last < section.First || section.Archive.ProjectID != stamp.ProjectID || section.ID == "" {
			return false, corestore.ErrInvalidPage
		}
		if err := s.publishStorageArchiveObject(ctx, section.Archive, stamp.WorkItemID); err != nil {
			return false, err
		}
		payloadCount := 0
		for _, payload := range publication.MessagePayloads {
			if payload.ConversationID == section.Subject && payload.Sequence >= section.First && payload.Sequence <= section.Last {
				payloadCount++
			}
		}
		_, err := s.db.Exec(ctx, `INSERT INTO issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,source_event_sequence,source_conversation_sequence,section_id,attachment_id,kind,subject_id,first_sequence,last_sequence,retired_through,retirement_complete)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (organization_id,project_id,work_item_id,archive_revision,section_id) DO NOTHING`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, stamp.EventSequence, stamp.ConversationSequence, section.ID, section.Archive.ID, section.Kind, section.Subject, section.First, section.Last, func() int64 {
			if payloadCount == 0 {
				return section.Last
			}
			return 0
		}(), payloadCount == 0)
		if err != nil {
			return false, mapError(ctx, err)
		}
		_, err = s.db.Exec(ctx, `INSERT INTO archive_object_owners (organization_id,archive_id,owner_kind,owner_id,created_at)
SELECT DISTINCT c.organization_id,c.archive_id,'issue-storage-section', $4::text, $7::timestamptz FROM conversation_messages m JOIN conversation_content_objects c ON c.organization_id=m.organization_id AND c.id=m.content_attachment_id
WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.seq BETWEEN $5 AND $6 AND m.id=ANY($8::text[]) AND EXISTS (SELECT 1 FROM conversations c WHERE c.organization_id=m.organization_id AND c.id=m.conversation_id AND c.work_item_id=$3 AND c.visibility='shared' AND c.deleted_at IS NULL) ON CONFLICT DO NOTHING`, s.organization, section.Subject, stamp.WorkItemID, section.ID, section.First, section.Last, publication.PublishedAt, ids)
		if err != nil {
			return false, mapError(ctx, err)
		}
	}
	for _, payload := range publication.MessagePayloads {
		if payload.Content.OrganizationID != s.organization || payload.ConversationID != conversationID || payload.MessageID == "" || payload.Revision < 1 || payload.Sequence < 1 || payload.ExpectedReference != "" {
			return false, corestore.ErrInvalidPage
		}
		source, exists := sources[payload.MessageID]
		if !exists || source.ConversationID != payload.ConversationID || source.Sequence != payload.Sequence || source.Revision != payload.Revision || source.ContentReference != payload.ExpectedReference {
			return false, corestore.ErrInvalidPage
		}
		var section *attachmentstore.StorageArchiveSection
		for i := range publication.Sections {
			candidate := &publication.Sections[i]
			if candidate.Subject == payload.ConversationID && payload.Sequence >= candidate.First && payload.Sequence <= candidate.Last {
				if section != nil {
					return false, corestore.ErrInvalidPage
				}
				section = candidate
			}
		}
		if section == nil {
			return false, corestore.ErrInvalidPage
		}
		if err := s.PutConversationContent(ctx, payload.Content); err != nil {
			return false, err
		}
		storedContent, err := s.ConversationContent(ctx, payload.Content.ID)
		if err != nil {
			return false, err
		}
		if err := s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: storedContent.ArchiveID, Kind: "issue-storage-section", ID: section.ID, CreatedAt: publication.PublishedAt}); err != nil {
			return false, err
		}
	}
	for _, source := range publication.MessageSources {
		var section *attachmentstore.StorageArchiveSection
		for i := range publication.Sections {
			candidate := &publication.Sections[i]
			if candidate.Subject == source.ConversationID && source.Sequence >= candidate.First && source.Sequence <= candidate.Last {
				if section != nil {
					return false, corestore.ErrInvalidPage
				}
				section = candidate
			}
		}
		if section == nil {
			return false, corestore.ErrInvalidPage
		}
		contentID := source.ContentReference
		if contentID == "" {
			for _, payload := range publication.MessagePayloads {
				if payload.MessageID == source.MessageID {
					contentID = payload.Content.ID
					break
				}
			}
		}
		if contentID == "" {
			return false, corestore.ErrInvalidPage
		}
		result, err := s.db.Exec(ctx, `INSERT INTO issue_archive_message_payloads (organization_id,project_id,work_item_id,archive_revision,section_id,conversation_id,message_id,sequence,message_revision,expected_reference,content_id)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11 WHERE EXISTS (SELECT 1 FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$6 AND id=$7 AND seq=$8 AND revision=$9 AND content_attachment_id=$10)
ON CONFLICT (organization_id,project_id,work_item_id,archive_revision,section_id,message_id) DO NOTHING`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, section.ID, source.ConversationID, source.MessageID, source.Sequence, source.Revision, source.ContentReference, contentID)
		if err != nil {
			return false, mapError(ctx, err)
		}
		if result.RowsAffected() == 0 {
			var matches bool
			err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM issue_archive_message_payloads r JOIN conversation_messages m ON m.organization_id=r.organization_id AND m.conversation_id=r.conversation_id AND m.id=r.message_id AND m.seq=r.sequence AND m.revision=r.message_revision AND (m.content_attachment_id=r.expected_reference OR m.content_attachment_id=r.content_id) WHERE r.organization_id=$1 AND r.project_id=$2 AND r.work_item_id=$3 AND r.archive_revision=$4 AND r.section_id=$5 AND r.message_id=$6 AND r.message_revision=$7 AND r.content_id=$8)`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, section.ID, source.MessageID, source.Revision, contentID).Scan(&matches)
			if err != nil {
				return false, mapError(ctx, err)
			}
			if !matches {
				return false, corestore.ErrRevisionConflict
			}
		}
	}

	_, err = s.db.Exec(ctx, `INSERT INTO issue_storage_archives (organization_id,project_id,work_item_id,issue_revision,event_sequence,conversation_sequence,root_attachment_id,through_history,last_message_scanned,published_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (organization_id,project_id,work_item_id) DO UPDATE SET issue_revision=excluded.issue_revision,event_sequence=excluded.event_sequence,conversation_sequence=excluded.conversation_sequence,root_attachment_id=excluded.root_attachment_id,through_history=excluded.through_history,last_message_scanned=excluded.last_message_scanned,published_at=CASE WHEN issue_storage_archives.root_attachment_id=excluded.root_attachment_id THEN issue_storage_archives.published_at ELSE excluded.published_at END`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, stamp.EventSequence, stamp.ConversationSequence, publication.Root.ID, publication.ThroughHistory, publication.LastMessageScanned, publication.PublishedAt)
	return err == nil, mapError(ctx, err)
}

func (s *session) RetireStorageArchiveSection(ctx context.Context, section attachmentstore.StorageArchiveSection, before time.Time, limit int) (attachmentstore.StorageArchiveRetirementResult, error) {
	if section.OrganizationID != s.organization || limit < 1 || limit > 500 {
		return attachmentstore.StorageArchiveRetirementResult{}, corestore.ErrInvalidPage
	}
	stamp := section.Stamp
	if _, err := s.storageArchiveCurrentSource(ctx, stamp, before, storageArchiveKeyLock); err != nil {
		return attachmentstore.StorageArchiveRetirementResult{}, err
	}
	conversation, err := s.LockConversation(ctx, stamp.ProjectID, section.Subject)
	if err != nil {
		return attachmentstore.StorageArchiveRetirementResult{}, err
	}
	if string(conversation.Visibility) != "shared" || conversation.WorkItemID != string(stamp.WorkItemID) {
		return attachmentstore.StorageArchiveRetirementResult{}, corestore.ErrRevisionConflict
	}
	if _, err := s.storageArchiveCurrentSource(ctx, stamp, before, storageArchiveRead); err != nil {
		return attachmentstore.StorageArchiveRetirementResult{}, err
	}
	var last, through int64
	var complete bool
	err = s.db.QueryRow(ctx, `SELECT last_sequence,retired_through,retirement_complete FROM issue_archive_sections WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND archive_revision=$4 AND section_id=$5 AND kind='conversation_messages' AND subject_id=$6 AND attachment_id=$7 FOR UPDATE`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, section.ID, section.Subject, section.Archive.ID).Scan(&last, &through, &complete)
	if err != nil {
		return attachmentstore.StorageArchiveRetirementResult{}, mapError(ctx, err)
	}
	if complete {
		return attachmentstore.StorageArchiveRetirementResult{Complete: true}, nil
	}
	var processed, updated int
	var retiredThrough *int64
	err = s.db.QueryRow(ctx, `WITH batch AS MATERIALIZED (
SELECT message_id,sequence,message_revision,expected_reference,content_id FROM issue_archive_message_payloads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND archive_revision=$4 AND section_id=$5 AND sequence>$6 ORDER BY sequence,message_id LIMIT $7
), changed AS (
UPDATE conversation_messages m SET text='',data_json='{}',content_attachment_id=b.content_id FROM batch b
WHERE m.organization_id=$1 AND m.conversation_id=$8 AND m.id=b.message_id AND m.seq=b.sequence AND m.revision=b.message_revision AND m.content_attachment_id=b.expected_reference AND b.expected_reference='' RETURNING m.id)
SELECT (SELECT COUNT(*) FROM batch),(SELECT COUNT(*) FROM changed),(SELECT MAX(sequence) FROM batch)`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, section.ID, through, limit, section.Subject).Scan(&processed, &updated, &retiredThrough)
	if err != nil {
		return attachmentstore.StorageArchiveRetirementResult{}, mapError(ctx, err)
	}
	if retiredThrough != nil {
		through = *retiredThrough
	} else {
		through = last
	}
	_, err = s.db.Exec(ctx, `UPDATE issue_archive_sections SET retired_through=$6,retirement_complete=$7 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND archive_revision=$4 AND section_id=$5`, s.organization, stamp.ProjectID, stamp.WorkItemID, stamp.Revision, section.ID, through, through >= last || processed < limit)
	return attachmentstore.StorageArchiveRetirementResult{Processed: processed, Complete: through >= last || processed < limit}, mapError(ctx, err)
}

var _ attachmentstore.StorageArchiveWriter = (*session)(nil)

func (s *session) StorageArchiveScanCursor(ctx context.Context) (attachmentstore.StorageArchiveCursor, error) {
	var result attachmentstore.StorageArchiveCursor
	var positioned bool
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT positioned,archive_terminal_at,sequence FROM storage_conversion_cursors WHERE organization_id=$1 AND family='issue-archives'`, s.organization).Scan(&positioned, &at, &result.ID)
	if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, mapError(ctx, err)
	}
	if !positioned {
		if at != nil || result.ID != 0 {
			return result, corestore.ErrInvalidPage
		}
		return result, nil
	}
	if at == nil || result.ID < 1 {
		return result, corestore.ErrInvalidPage
	}
	result.TerminalEnteredAt = *at
	return result, nil
}

func (s *session) AdvanceStorageArchiveScan(ctx context.Context, from, to attachmentstore.StorageArchiveCursor) (bool, error) {
	if from.ID < 0 || to.ID < 0 || from.ID > 0 && from.TerminalEnteredAt.IsZero() || to.ID > 0 && to.TerminalEnteredAt.IsZero() {
		return false, corestore.ErrInvalidPage
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO storage_conversion_cursors (organization_id,family) VALUES ($1,'issue-archives') ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return false, mapError(ctx, err)
	}
	var before, after any
	if from.ID > 0 {
		before = from.TerminalEnteredAt
	}
	if to.ID > 0 {
		after = to.TerminalEnteredAt
	}
	result, err := s.db.Exec(ctx, `UPDATE storage_conversion_cursors SET positioned=$2,archive_terminal_at=$3,sequence=$4,turn=turn+1 WHERE organization_id=$1 AND family='issue-archives' AND positioned=$5 AND archive_terminal_at IS NOT DISTINCT FROM $6::timestamptz AND sequence=$7`, s.organization, to.ID > 0, after, to.ID, from.ID > 0, before, from.ID)
	if err != nil {
		return false, mapError(ctx, err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *session) StorageArchiveRetirementCursor(ctx context.Context) (attachmentstore.StorageArchiveRetirementCursor, error) {
	var result attachmentstore.StorageArchiveRetirementCursor
	var positioned bool
	err := s.db.QueryRow(ctx, `SELECT positioned,key_1,key_2,key_3,sequence FROM storage_conversion_cursors WHERE organization_id=$1 AND family='issue-archive-retirement'`, s.organization).Scan(&positioned, &result.ProjectID, &result.WorkItemID, &result.SectionID, &result.Revision)
	if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, mapError(ctx, err)
	}
	if positioned != (result.ProjectID != "" && result.WorkItemID != "" && result.SectionID != "" && result.Revision > 0) {
		return result, corestore.ErrInvalidPage
	}
	return result, nil
}

func (s *session) AdvanceStorageArchiveRetirement(ctx context.Context, from, to attachmentstore.StorageArchiveRetirementCursor) (bool, error) {
	positioned := to.SectionID != ""
	if positioned && (to.ProjectID == "" || to.WorkItemID == "" || to.Revision < 1) {
		return false, corestore.ErrInvalidPage
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO storage_conversion_cursors (organization_id,family) VALUES ($1,'issue-archive-retirement') ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return false, mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `UPDATE storage_conversion_cursors SET positioned=$2,key_1=$3,key_2=$4,key_3=$5,sequence=$6,turn=turn+1 WHERE organization_id=$1 AND family='issue-archive-retirement' AND positioned=$7 AND key_1=$8 AND key_2=$9 AND key_3=$10 AND sequence=$11`, s.organization, positioned, to.ProjectID, to.WorkItemID, to.SectionID, to.Revision, from.SectionID != "", from.ProjectID, from.WorkItemID, from.SectionID, from.Revision)
	if err != nil {
		return false, mapError(ctx, err)
	}
	return result.RowsAffected() == 1, nil
}
