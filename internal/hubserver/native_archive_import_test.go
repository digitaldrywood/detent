package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeArchiveImportRecoversRetiredMessagesAndImmutableVersions(t *testing.T) {
	t.Parallel()
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprintf("newer pointer=%v", newer), func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
			if !ok {
				t.Fatal("archive import requires the PostgreSQL fixture owner")
			}
			org := f.project.OrganizationID
			objects := &memoryRunDetails{}
			names := []string{"conversations", "conversation_messages", "conversation_questions", "conversation_commands", "conversation_events", "conversation_starts", "conversation_audience_events", "conversation_turn_batches", "conversation_tool_revisions", "message_references", "conversation_attachments", "conversation_attachment_blobs", "attachments", "collaboration_versions", "native_archive_sections", "issue_archive_sections", "issue_storage_archives"}
			fixture := newCoreNeutralFixture(t, org, names...)
			type expectedArchive struct {
				item                                    tracker.NativeWorkItemID
				conversation, message, version, section string
				revision                                int64
				reference                               attachment.ArchiveReference
				raw                                     []byte
			}
			var expected []expectedArchive
			roots := map[tracker.NativeWorkItemID]attachment.ArchiveReference{}
			rootBytes := map[tracker.NativeWorkItemID][]byte{}
			for index := range 2 {
				issue := f.closed(t)
				record := f.messages(t, issue.WorkItemID, 0, conversation.VisibilityShared)
				var current corestore.WorkItem
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					current, err = r.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				at := f.now().Format(time.RFC3339Nano)
				messageID := fmt.Sprintf("historical_message_%d", index)
				versionID := fmt.Sprintf("historical_comment_%d", index)
				message := coreNeutralFixtureRow(t, fixture.tables["conversation_messages"], map[string]any{"id": messageID, "conversation_id": record.ID, "seq": 1, "role": "assistant", "kind": "text", "text": "retired message 🌍", "data_json": "{}", "delivery": "completed", "actor_json": "{}", "created_at": at, "updated_at": at, "revision": 1, "text_bytes": len("retired message 🌍"), "stored_text_bytes": len("retired message 🌍")})
				oldVersion := coreNeutralFixtureRow(t, fixture.tables["collaboration_versions"], map[string]any{"organization_id": org, "project_id": f.project.ID, "work_item_id": issue.WorkItemID, "record_id": versionID, "revision": 1, "record_json": `{"body":"retired comment","unknown":{"preserved":true}}`, "encoding": "snapshot"})
				tables := []attachment.ArchiveTable{}
				for _, source := range []struct {
					name string
					row  postgres.NeutralImportRow
				}{{"conversation_messages", message}, {"collaboration_versions", oldVersion}} {
					columns := make([]string, 0, len(source.row.Values))
					for column := range source.row.Values {
						columns = append(columns, column)
					}
					slices.Sort(columns)
					row := []json.RawMessage{json.RawMessage("1")}
					for _, column := range columns {
						cell := source.row.Values[column]
						var raw []byte
						var err error
						if cell.Null {
							raw = []byte("null")
						} else {
							var signature []json.RawMessage
							if err := json.Unmarshal([]byte(fixture.tables[source.name].Columns[column]), &signature); err != nil {
								t.Fatal(err)
							}
							var kind string
							if err := json.Unmarshal(signature[2], &kind); err != nil {
								t.Fatal(err)
							}
							if strings.Contains(strings.ToUpper(kind), "INT") || strings.Contains(strings.ToUpper(kind), "REAL") {
								raw = bytes.Clone(cell.Data)
							} else {
								raw, err = json.Marshal(string(cell.Data))
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						row = append(row, raw)
					}
					tables = append(tables, attachment.ArchiveTable{Name: source.name, Columns: append([]string{"rowid"}, columns...), Rows: [][]json.RawMessage{row}})
				}
				var sections []map[string]any
				for kindIndex, kind := range []string{"conversation", "revision"} {
					subject := record.ID
					if kind == "revision" {
						subject = versionID
					}
					sectionID := fmt.Sprintf("historical_%d_%s", index, kind)
					document := attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: string(org), ProjectID: string(f.project.ID), WorkItemID: string(issue.WorkItemID), SectionID: sectionID, Tables: []attachment.ArchiveTable{tables[kindIndex]}}
					raw, err := json.Marshal(document)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := objects.StoreDetail(t.Context(), string(org), raw)
					if err != nil {
						t.Fatal(err)
					}
					attachmentID := sectionID + "_attachment"
					fixture.add("attachments", map[string]any{"id": attachmentID, "project_id": f.project.ID, "uploader": f.scope.Credential.ID, "name": "archive.gz", "content_type": "application/gzip", "size": ref.CompressedSize, "sha256": ref.SHA256, "created_at": at, "kind": ref.Kind, "retention_class": ref.Retention, "object_key": ref.Key, "uncompressed_size": ref.UncompressedSize, "codec": ref.Codec, "schema_version": ref.SchemaVersion, "storage_binding_id": "", "customer_owned": 0})
					fixture.add("native_archive_sections", map[string]any{"project_id": f.project.ID, "work_item_id": issue.WorkItemID, "kind": kind, "subject_id": subject, "first_sequence": 1, "last_sequence": 1, "section_id": sectionID, "attachment_id": attachmentID, "retired_rows": 1, "retirement_complete": 1})
					fixture.add("issue_archive_sections", map[string]any{"project_id": f.project.ID, "work_item_id": issue.WorkItemID, "archive_revision": issue.Revision, "section_id": sectionID, "attachment_id": attachmentID})
					expected = append(expected, expectedArchive{item: issue.WorkItemID, conversation: record.ID, message: messageID, version: versionID, section: sectionID, revision: int64(issue.Revision), reference: ref, raw: raw})
					sections = append(sections, map[string]any{"kind": kind, "subject_id": subject, "first_sequence": 1, "last_sequence": 1, "section_id": sectionID, "reference": ref})
				}
				raw, err := json.Marshal(map[string]any{"schema_version": 1, "organization_id": org, "project_id": f.project.ID, "work_item_id": issue.WorkItemID, "revision": int64(issue.Revision), "event_sequence": current.EventSequence, "conversation_sequence": 0, "body": "historical body", "sections": sections})
				if err != nil {
					t.Fatal(err)
				}
				ref, err := objects.StoreDetail(t.Context(), string(org), raw)
				if err != nil {
					t.Fatal(err)
				}
				id := fmt.Sprintf("historical_root_%d", index)
				fixture.add("attachments", map[string]any{"id": id, "project_id": f.project.ID, "uploader": f.scope.Credential.ID, "name": "archive.gz", "content_type": "application/gzip", "size": ref.CompressedSize, "sha256": ref.SHA256, "created_at": at, "kind": ref.Kind, "retention_class": ref.Retention, "object_key": ref.Key, "uncompressed_size": ref.UncompressedSize, "codec": ref.Codec, "schema_version": ref.SchemaVersion, "storage_binding_id": "", "customer_owned": 0})
				fixture.add("issue_storage_archives", map[string]any{"project_id": f.project.ID, "work_item_id": issue.WorkItemID, "issue_revision": issue.Revision, "event_sequence": current.EventSequence, "conversation_sequence": 0, "attachment_id": id, "published_at": at})
				roots[issue.WorkItemID], rootBytes[issue.WorkItemID] = ref, raw
			}
			catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
			if err != nil {
				t.Fatal(err)
			}
			source := postgres.ConversationHistoryImportSource{BundlePath: fixture.write(), Catalog: catalog, Source: storagecatalog.Source{Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(org)}, Objects: objects}
			if _, err := repository.ImportConversationHistory(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			pointers := map[tracker.NativeWorkItemID]attachmentstore.StorageArchivePointer{}
			if newer {
				for item := range roots {
					current := readWorkItem(t, f.nativeFixture, item, "")
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "newer-" + string(item)}, ExpectedRevision: current.Revision, Title: new("Newer current title")})
					requireNativeStatus(t, response, http.StatusOK)
				}
				f.archive(t)
				for item := range roots {
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						pointers[item], err = r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, item)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			recovery := postgres.NativeArchiveImportSource{ConversationHistoryImportSource: source, ValidateSection: validateNativeArchiveSection}
			missing := expected[0]
			objects.mu.Lock()
			delete(objects.data, missing.reference.Key)
			objects.mu.Unlock()
			if _, err := repository.ImportNativeArchiveHistory(t.Context(), recovery); !errors.Is(err, artifact.ErrMissing) {
				t.Fatalf("missing archived bytes error=%v", err)
			}
			objects.mu.Lock()
			objects.data[missing.reference.Key] = bytes.Clone(missing.raw)
			objects.mu.Unlock()
			root := roots[missing.item]
			originalRoot := rootBytes[missing.item]
			tamperedRoot := bytes.Replace(originalRoot, []byte(missing.reference.SHA256), []byte(strings.Repeat("b", 64)), 1)
			if bytes.Equal(tamperedRoot, originalRoot) || len(tamperedRoot) != len(originalRoot) {
				t.Fatal("root reference mismatch fixture changed length")
			}
			objects.mu.Lock()
			objects.data[root.Key] = tamperedRoot
			objects.mu.Unlock()
			if _, err := repository.ImportNativeArchiveHistory(t.Context(), recovery); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("mismatched root section error=%v", err)
			}
			objects.mu.Lock()
			objects.data[root.Key] = bytes.Clone(originalRoot)
			objects.mu.Unlock()
			tamperedSection := bytes.Replace(missing.raw, []byte(missing.section), []byte(strings.Repeat("x", len(missing.section))), 1)
			objects.mu.Lock()
			objects.data[missing.reference.Key] = tamperedSection
			objects.mu.Unlock()
			if _, err := repository.ImportNativeArchiveHistory(t.Context(), recovery); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("mismatched section header error=%v", err)
			}
			objects.mu.Lock()
			objects.data[missing.reference.Key] = bytes.Clone(missing.raw)
			objects.mu.Unlock()
			for range 2 {
				result, err := repository.ImportNativeArchiveHistory(t.Context(), recovery)
				if err != nil || result.Sections != 4 || result.Messages != 2 || result.Versions != 2 {
					t.Fatalf("archive recovery=%+v error=%v", result, err)
				}
			}
			for item, reference := range roots {
				raw, err := objects.ReadDetail(t.Context(), string(org), reference)
				if err != nil || !bytes.Equal(raw, rootBytes[item]) {
					t.Fatalf("historical root bytes changed: %v", err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					pointer, err := r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, item)
					if err != nil {
						return err
					}
					if newer {
						if pointer.Root.Reference != pointers[item].Root.Reference || pointer.Stamp.Revision != pointers[item].Stamp.Revision {
							t.Fatal("recovery overwrote a newer canonical pointer")
						}
					} else {
						if pointer.Root.Reference != reference {
							t.Fatal("legacy root lost canonical custody")
						}
						if pointer.ThroughHistory != 0 || pointer.LastMessageScanned != 0 {
							t.Fatal("historical recovery claimed fresh schema2 scan progress")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, ref := range roots {
				var owners int
				if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT count(*) FROM archive_objects a JOIN archive_object_owners o ON o.organization_id=a.organization_id AND o.archive_id=a.id WHERE a.organization_id=$1 AND a.sha256=$2 AND o.owner_kind='issue-storage-archive'`, org, ref.SHA256).Scan(&owners); err != nil || owners != 1 {
					t.Fatalf("unpointed historical root custody=%d error=%v", owners, err)
				}
			}
			f.archive(t)
			objectsAfter := len(f.archiveObjects(t))
			f.archive(t)
			if len(f.archiveObjects(t)) != objectsAfter {
				t.Fatal("completed schema2 continuation republished unchanged payloads")
			}
			for item := range roots {
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					pointer, err := r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, item)
					if err != nil {
						return err
					}
					if pointer.Root.Reference.SchemaVersion != 2 || pointer.ThroughHistory < pointer.Stamp.EventSequence {
						t.Fatal("zero progress did not resume fresh schema2 publication")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, want := range expected {
				raw, err := objects.ReadDetail(t.Context(), string(org), want.reference)
				if err != nil || !bytes.Equal(raw, want.raw) {
					t.Fatalf("historical source bytes changed: %v", err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					section, err := readIssueArchiveSection(t.Context(), r.(attachmentstore.StorageArchiveReader), f.scope, string(want.item), want.section)
					if err != nil {
						return err
					}
					if section.SchemaVersion != 1 || section.SHA256 != want.reference.SHA256 {
						t.Fatal("verified legacy section lost provenance")
					}
					if strings.HasSuffix(want.section, "revision") {
						versions, err := r.(eventstore.CollaborationVersionsReader).CollaborationVersions(t.Context(), []eventstore.CollaborationVersionKey{{RecordID: want.version, Revision: 1}})
						if err != nil {
							return err
						}
						if len(versions) != 1 {
							t.Fatal("retired immutable version was not restored")
						}
						raw, err := eventstore.NewContent(objects).CollaborationVersion(t.Context(), versions[0])
						if err != nil {
							return err
						}
						if !bytes.Contains(raw, []byte(`"unknown":{"preserved":true}`)) {
							t.Fatal("recovery changed immutable version fields")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				messages := f.readMessages(t, want.conversation)
				if len(messages) != 1 || messages[0].ID != want.message || messages[0].Text != "retired message 🌍" {
					t.Fatal("retired canonical message read parity failed")
				}
			}
			for _, want := range expected {
				if !strings.HasSuffix(want.section, "conversation") {
					continue
				}
				f.write(t, func(w tenantstore.Writer) error {
					record, err := w.Conversation(t.Context(), f.project.ID, want.conversation)
					if err != nil {
						return err
					}
					revision := record.Revision
					record.Visibility = conversation.VisibilityPrivate
					_, err = w.UpdateConversation(t.Context(), record, revision)
					return err
				})
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.(attachmentstore.StorageArchiveReader).StorageArchiveSection(t.Context(), attachmentstore.StorageArchiveSectionQuery{ProjectID: f.project.ID, WorkItemID: want.item, SectionID: want.section})
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("private archive metadata error=%v", err)
					}
					_, err = r.(attachmentstore.ArchiveReader).AttachmentArchive(t.Context(), attachmentstore.ArchiveQuery{ProjectID: f.project.ID, WorkItemID: want.item, Section: want.section, Revision: want.revision, Digest: want.reference.SHA256})
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("private archive access error=%v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), "org_first_second", corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(attachmentstore.StorageArchiveReader).StorageArchiveSection(t.Context(), attachmentstore.StorageArchiveSectionQuery{ProjectID: f.project.ID, WorkItemID: expected[0].item, SectionID: expected[0].section})
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("cross tenant archive error=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_history_import_rows WHERE organization_id=$1 AND source_table LIKE 'archive/%/conversation_messages'`, org).Scan(&count); err != nil || count != 2 {
				t.Fatalf("archive provenance receipts=%d error=%v", count, err)
			}
		})
	}

}
