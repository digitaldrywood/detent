package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeArchiveImportSource struct {
	ConversationHistoryImportSource
	ValidateSection func(attachment.ArchiveSection, string, string, int64, int64) error
}

type NativeArchiveImportResult struct{ Sections, Messages, Versions int64 }

type nativeArchiveImportSection struct {
	organization                                               tracker.OrganizationID
	project                                                    tracker.ProjectID
	item                                                       tracker.NativeWorkItemID
	kind, subject, id, attachment                              string
	first, last, revision, eventSequence, conversationSequence int64
	published                                                  time.Time
	record                                                     attachmentstore.ArchiveRecord
	root                                                       attachmentstore.ArchiveRecord
}

func (r *Repository) ImportNativeArchiveHistory(ctx context.Context, source NativeArchiveImportSource) (NativeArchiveImportResult, error) {
	var result NativeArchiveImportResult
	if source.CopyPath != "" || source.BundlePath == "" || source.Objects == nil || source.Source.Store != "hub" || source.Source.OrganizationID == "" || source.ValidateSection == nil {
		return result, neutralimport.ErrInvalid
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	source.Source.Schema = "hub_copy"
	if source.Schema == nil {
		source.Schema = schemaFromCatalog(source.Catalog)
	}
	err := neutralimport.Read(ctx, source.BundlePath, organization, func(bundle *neutralimport.Bundle) error {
		source.neutral = bundle
		if _, exists := bundle.Schema()["native_archive_sections"]; !exists {
			return nil
		}
		for _, table := range []struct {
			name string
			keys []string
		}{
			{"native_archive_sections", []string{"organization_id", "project_id", "work_item_id", "kind", "subject_id", "first_sequence"}},
			{"issue_archive_sections", []string{"organization_id", "project_id", "work_item_id", "archive_revision", "section_id"}},
			{"issue_storage_archives", []string{"organization_id", "project_id", "work_item_id"}},
			{"attachments", []string{"id"}},
			{"conversation_messages", []string{"id"}},
			{"collaboration_versions", []string{"organization_id", "record_id", "revision"}},
		} {
			if err := bundle.ValidateCatalogTable(source.Catalog, source.Schema, table.name, table.keys); err != nil {
				return err
			}
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var after []string
		for {
			rows, err := bundle.Page(ctx, "native_archive_sections", after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				section, err := readNativeArchiveImportSection(ctx, source, row)
				if err != nil {
					return fmt.Errorf("verify historical archive metadata: %w", err)
				}
				raw, err := source.Objects.ReadDetail(ctx, string(organization), section.record.Reference)
				if err != nil {
					return fmt.Errorf("read historical archive: %w", err)
				}
				if int64(len(raw)) != section.record.Reference.UncompressedSize || len(raw) > attachment.MaxSectionBytes {
					return artifact.ErrIntegrity
				}
				var document attachment.ArchiveSection
				if json.Unmarshal(raw, &document) != nil || document.SchemaVersion != 1 || document.OrganizationID != string(organization) || document.ProjectID != string(section.project) || document.WorkItemID != string(section.item) || document.SectionID != section.id {
					return artifact.ErrIntegrity
				}
				if err := source.ValidateSection(document, section.kind, section.subject, section.first, section.last); err != nil {
					return err
				}
				if err := r.importNativeArchiveRows(ctx, source, section, document, &result); err != nil {
					return err
				}
				if err := r.publishImportedNativeArchiveSection(ctx, source, section); err != nil {
					return err
				}
				result.Sections++
			}
			after = rows[len(rows)-1].Key
		}
	})
	return result, err
}

func readNativeArchiveImportSection(ctx context.Context, source NativeArchiveImportSource, row neutralimport.Row) (nativeArchiveImportSection, error) {
	var section nativeArchiveImportSection
	if err := row.Decode(map[string]any{"organization_id": &section.organization, "project_id": &section.project, "work_item_id": &section.item, "kind": &section.kind, "subject_id": &section.subject, "first_sequence": &section.first, "last_sequence": &section.last, "section_id": &section.id, "attachment_id": &section.attachment}); err != nil {
		return section, err
	}
	if string(section.organization) != source.Source.OrganizationID || section.project == "" || section.item == "" || section.id == "" || section.attachment == "" || section.first < 0 || section.last < section.first {
		return section, artifact.ErrIntegrity
	}
	root, err := source.neutral.Lookup(ctx, "issue_storage_archives", []string{string(section.organization), string(section.project), string(section.item)})
	if err != nil {
		return section, err
	}
	var published, rootAttachment string
	if err := root.Decode(map[string]any{"issue_revision": &section.revision, "event_sequence": &section.eventSequence, "conversation_sequence": &section.conversationSequence, "published_at": &published, "attachment_id": &rootAttachment}); err != nil {
		return section, err
	}
	section.published, err = time.Parse(time.RFC3339Nano, published)
	if err != nil || section.revision < 1 || section.eventSequence < 0 || section.conversationSequence < 0 {
		return section, artifact.ErrIntegrity
	}
	link, err := source.neutral.Lookup(ctx, "issue_archive_sections", []string{string(section.organization), string(section.project), string(section.item), strconv.FormatInt(section.revision, 10), section.id})
	if err != nil {
		return section, err
	}
	var attachmentID string
	if err := link.Decode(map[string]any{"attachment_id": &attachmentID}); err != nil {
		return section, err
	}
	if attachmentID != section.attachment {
		return section, artifact.ErrIntegrity
	}
	section.record, err = readNativeArchiveImportAttachment(ctx, source, section.organization, section.project, section.attachment)
	if err != nil {
		return section, err
	}
	section.root, err = readNativeArchiveImportAttachment(ctx, source, section.organization, section.project, rootAttachment)
	if err != nil {
		return section, err
	}
	raw, err := source.Objects.ReadDetail(ctx, string(section.organization), section.root.Reference)
	if err != nil {
		return section, err
	}
	if int64(len(raw)) != section.root.Reference.UncompressedSize {
		return section, artifact.ErrIntegrity
	}
	var document struct {
		SchemaVersion        int    `json:"schema_version"`
		Organization         string `json:"organization_id"`
		Project              string `json:"project_id"`
		Item                 string `json:"work_item_id"`
		Revision             int64  `json:"revision"`
		Sequence             int64  `json:"event_sequence"`
		ConversationSequence int64  `json:"conversation_sequence"`
		Sections             []struct {
			Kind      string                      `json:"kind"`
			Subject   string                      `json:"subject_id"`
			First     int64                       `json:"first_sequence"`
			Last      int64                       `json:"last_sequence"`
			ID        string                      `json:"section_id"`
			Reference attachment.ArchiveReference `json:"reference"`
		} `json:"sections"`
	}
	if json.Unmarshal(raw, &document) != nil || document.SchemaVersion != 1 || document.Organization != string(section.organization) || document.Project != string(section.project) || document.Item != string(section.item) || document.Revision != section.revision || document.Sequence != section.eventSequence || document.ConversationSequence != section.conversationSequence {
		return section, artifact.ErrIntegrity
	}
	matches := 0
	for _, entry := range document.Sections {
		if entry.ID == section.id {
			if entry.Kind != section.kind || entry.Subject != section.subject || entry.First != section.first || entry.Last != section.last || entry.Reference != section.record.Reference {
				return section, artifact.ErrIntegrity
			}
			matches++
		}
	}
	if matches != 1 {
		return section, artifact.ErrIntegrity
	}
	return section, nil
}

func readNativeArchiveImportAttachment(ctx context.Context, source NativeArchiveImportSource, organization tracker.OrganizationID, project tracker.ProjectID, id string) (attachmentstore.ArchiveRecord, error) {
	var record attachmentstore.ArchiveRecord
	record.OrganizationID = organization
	row, err := source.neutral.Lookup(ctx, "attachments", []string{id})
	if err != nil {
		return record, err
	}
	var created string
	ref := &record.Reference
	if err := row.Decode(map[string]any{"id": &record.ID, "project_id": &record.ProjectID, "uploader": &record.Uploader, "object_key": &ref.Key, "sha256": &ref.SHA256, "size": &ref.CompressedSize, "uncompressed_size": &ref.UncompressedSize, "codec": &ref.Codec, "schema_version": &ref.SchemaVersion, "kind": &ref.Kind, "retention_class": &ref.Retention, "storage_binding_id": &ref.BindingID, "customer_owned": &ref.CustomerOwned, "created_at": &created}); err != nil {
		return record, err
	}
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil || record.ID != id || record.ProjectID != project || record.Uploader == "" || ref.Validate(string(organization)) != nil || ref.SchemaVersion != 1 || ref.UncompressedSize > attachment.MaxSectionBytes {
		return record, artifact.ErrIntegrity
	}
	return record, nil
}

func archivedImportRow(table attachment.ArchiveTable, values []json.RawMessage) (neutralimport.Row, error) {
	row := neutralimport.Row{Values: map[string]storagecatalog.Cell{}}
	if len(table.Columns) != len(values) || len(values) == 0 || table.Columns[0] != "rowid" || json.Unmarshal(values[0], &row.SourceRowID) != nil || row.SourceRowID < 1 {
		return row, artifact.ErrIntegrity
	}
	for i, name := range table.Columns[1:] {
		if _, exists := row.Values[name]; exists {
			return row, artifact.ErrIntegrity
		}
		raw := values[i+1]
		cell := storagecatalog.Cell{Encoding: "utf8"}
		if bytes.Equal(raw, []byte("null")) {
			cell = storagecatalog.Cell{Null: true}
		} else {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				cell.Data = []byte(text)
			} else {
				var number json.Number
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				var scalar any
				if decoder.Decode(&scalar) != nil {
					return row, artifact.ErrIntegrity
				}
				var ok bool
				number, ok = scalar.(json.Number)
				if !ok {
					return row, artifact.ErrIntegrity
				}
				cell.Data = []byte(number.String())
			}
		}
		row.Values[name] = cell
	}
	return row, nil
}

func (r *Repository) importNativeArchiveRows(ctx context.Context, source NativeArchiveImportSource, section nativeArchiveImportSection, document attachment.ArchiveSection, result *NativeArchiveImportResult) error {
	for _, table := range document.Tables {
		if table.Name != "conversation_messages" && table.Name != "collaboration_versions" {
			continue
		}
		definition, ok := source.Catalog.Tables["hub/"+table.Name]
		if !ok || len(table.Columns) != len(definition.Columns)+1 {
			return storagecatalog.ErrUnmapped
		}
		seen := map[string]bool{}
		for _, name := range table.Columns[1:] {
			if _, ok := definition.Columns[name]; !ok || seen[name] {
				return storagecatalog.ErrUnmapped
			}
			seen[name] = true
		}
		for _, values := range table.Rows {
			row, err := archivedImportRow(table, values)
			if err != nil {
				return err
			}
			if table.Name == "conversation_messages" {
				var id, conversationID string
				if err := row.Decode(map[string]any{"id": &id, "conversation_id": &conversationID}); err != nil {
					return err
				}
				if id == "" || conversationID != section.subject {
					return artifact.ErrIntegrity
				}
				_, err := source.neutral.Lookup(ctx, "conversation_messages", []string{id})
				if err == nil {
					continue
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				objects := map[string]conversationstore.ContentObject{}
				prepared, content, err := prepareConversationImportRow(ctx, source.ConversationHistoryImportSource, source.neutral.Identity().SourceSHA256, section.organization, conversationImportTable{source: "conversation_messages", target: "conversation_messages", keys: []string{"id"}}, conversationSourceRow{rowID: row.SourceRowID, cells: row.Values}, objects)
				if err != nil {
					return err
				}
				for i := range prepared {
					prepared[i].Receipt.Table = "archive/" + section.record.Reference.SHA256 + "/conversation_messages"
				}
				if err := r.transaction(ctx, section.organization, corestore.Background, false, func(s *session) error {
					current, err := s.LockConversation(ctx, section.project, conversationID)
					if err != nil {
						return err
					}
					if current.WorkItemID != string(section.item) {
						return artifact.ErrIntegrity
					}
					for _, object := range content {
						if err := s.PutConversationContent(ctx, object); err != nil {
							return err
						}
					}
					for _, record := range prepared {
						if err := s.importConversationRow(ctx, record); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					return err
				}
				result.Messages++
			} else {
				var id, encoding, raw string
				var revision int64
				if err := row.Decode(map[string]any{"record_id": &id, "revision": &revision, "record_json": &raw, "encoding": &encoding}); err != nil {
					return err
				}
				if id != section.subject || revision < section.first || revision > section.last || encoding != "snapshot" || !json.Valid([]byte(raw)) {
					return artifact.ErrIntegrity
				}
				_, err := source.neutral.Lookup(ctx, "collaboration_versions", []string{string(section.organization), id, strconv.FormatInt(revision, 10)})
				if err == nil {
					continue
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				normalized, err := eventstore.NewContent(source.Objects).PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: section.organization, ProjectID: section.project, WorkItemID: section.item, RecordID: id, Revision: revision}, []byte(raw))
				if err != nil {
					return err
				}
				if err := r.transaction(ctx, section.organization, corestore.Background, false, func(s *session) error { _, err := s.CreateCollaborationVersion(ctx, normalized); return err }); err != nil {
					return err
				}
				result.Versions++
			}
		}
	}
	return nil
}

func (r *Repository) publishImportedNativeArchiveSection(ctx context.Context, source NativeArchiveImportSource, section nativeArchiveImportSection) error {
	kind := section.kind
	if kind == "conversation" {
		kind = "conversation_messages"
	}
	if !slices.Contains([]string{"run", "change", "revision", "conversation_messages", "history"}, kind) {
		return artifact.ErrIntegrity
	}
	return r.transaction(ctx, section.organization, corestore.Background, false, func(s *session) error {
		if err := s.publishStorageArchiveObject(ctx, section.root, section.item); err != nil {
			return err
		}
		_, err := s.db.Exec(ctx, `INSERT INTO issue_storage_archives (organization_id,project_id,work_item_id,issue_revision,event_sequence,conversation_sequence,root_attachment_id,through_history,last_message_scanned,published_at) VALUES ($1,$2,$3,$4,$5,$6,$7,0,0,$8) ON CONFLICT DO NOTHING`, section.organization, section.project, section.item, section.revision, section.eventSequence, section.conversationSequence, section.root.ID, section.published)
		if err != nil {
			return mapError(ctx, err)
		}
		if err := s.publishStorageArchiveObject(ctx, section.record, section.item); err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,section_id,attachment_id,kind,subject_id,first_sequence,last_sequence,source_event_sequence,source_conversation_sequence,retirement_complete) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,true) ON CONFLICT DO NOTHING`, section.organization, section.project, section.item, section.revision, section.id, section.record.ID, kind, section.subject, section.first, section.last, section.eventSequence, section.conversationSequence)
		if err != nil {
			return mapError(ctx, err)
		}
		_, err = s.db.Exec(ctx, `UPDATE issue_archive_sections SET kind=$7,subject_id=$8,first_sequence=$9,last_sequence=$10,source_event_sequence=$11,source_conversation_sequence=$12,retirement_complete=true WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND archive_revision=$4 AND section_id=$5 AND attachment_id=$6 AND kind=''`, section.organization, section.project, section.item, section.revision, section.id, section.record.ID, kind, section.subject, section.first, section.last, section.eventSequence, section.conversationSequence)
		if err != nil {
			return mapError(ctx, err)
		}
		var attachmentID, storedKind, subject string
		var first, last int64
		err = s.db.QueryRow(ctx, `SELECT attachment_id,kind,subject_id,first_sequence,last_sequence FROM issue_archive_sections WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND archive_revision=$4 AND section_id=$5`, section.organization, section.project, section.item, section.revision, section.id).Scan(&attachmentID, &storedKind, &subject, &first, &last)
		if err != nil {
			return mapError(ctx, err)
		}
		if attachmentID != section.record.ID || storedKind != kind || subject != section.subject || first != section.first || last != section.last {
			return mutation.ErrConflict
		}
		ref := section.record.Reference
		receipt := eventstore.Receipt{OrganizationID: section.organization, ActorID: "historical-archive-import", Operation: "archive-section", CommandKey: source.neutral.Identity().SourceSHA256 + ":" + section.id, RequestHash: artifact.Digest([]byte(source.neutral.Digest() + ":" + ref.SHA256)), CreatedAt: section.published, PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize}
		previous, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err == nil {
			if previous.RequestHash != receipt.RequestHash || previous.PayloadSHA256 != receipt.PayloadSHA256 {
				return mutation.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		_, err = s.PutReceipt(ctx, receipt)
		return err
	})
}
