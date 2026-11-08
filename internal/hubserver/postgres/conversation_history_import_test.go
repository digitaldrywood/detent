package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationHistoryImportPreservesTenantHistoryAndAttachmentBytes(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateSchema("hub", schemaFromCatalogForTest(catalog)); err != nil {
		t.Fatalf("catalog source schema: %v", err)
	}
	tables := []string{"conversations", "conversation_messages", "conversation_questions", "conversation_commands", "conversation_events", "conversation_starts", "conversation_audience_events", "conversation_turn_batches", "conversation_tool_revisions", "message_references", "conversation_attachments", "conversation_attachment_blobs", "attachments"}
	copies := map[tracker.OrganizationID]string{}
	now := "2026-11-01T18:00:00.123456Z"
	objects := &countingHistoryObjects{base: newHistoryObjects(), storedPayloads: make(map[string]int)}
	archiveReferences := make(map[tracker.OrganizationID]attachment.ArchiveReference)
	archivePayloads := make(map[tracker.OrganizationID]string)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		copy := newNeutralBillingFixture(t, organization, tables...)
		archiveID := "archive_" + string(organization)
		archiveBytes := []byte(`{"body":"preserved existing archive"}`)
		archivePayloads[organization] = string(organization) + "\x00" + string(archiveBytes)
		archiveRef, err := objects.StoreDetail(t.Context(), string(organization), archiveBytes)
		if err != nil {
			t.Fatal(err)
		}
		archiveReferences[organization] = archiveRef
		copy.add(t, "attachments", map[string]any{"id": archiveID, "organization_id": organization, "project_id": "prj_shared", "uploader": "first", "name": "history.json.gz", "content_type": "application/gzip", "size": archiveRef.CompressedSize, "sha256": archiveRef.SHA256, "created_at": now, "kind": "archive", "retention_class": "organization", "object_key": archiveRef.Key, "uncompressed_size": archiveRef.UncompressedSize, "codec": archiveRef.Codec, "schema_version": archiveRef.SchemaVersion, "storage_binding_id": "", "customer_owned": 0})
		for _, suffix := range []string{"", "_second"} {
			conversationID := "conversation_" + string(organization) + suffix
			messageID := "message_" + string(organization) + suffix
			artifactRef := "artifact_" + string(organization) + suffix
			blob := append([]byte("conversation attachment "+string(organization)+suffix), 0, 255, 128)
			copy.add(t, "conversations", map[string]any{"id": conversationID, "organization_id": organization, "project_id": "prj_shared", "owner_principal_id": "first", "origin": "user", "title": "Example conversation", "visibility": "private", "status": "active", "execution_json": "{}", "created_at": now, "updated_at": now})
			copy.add(t, "conversation_messages", map[string]any{"id": messageID, "conversation_id": conversationID, "seq": 1, "role": "user", "kind": "text", "text": "Example message", "data_json": "{}", "delivery": "saved", "actor_json": "{}", "created_at": now, "updated_at": now, "content_attachment_id": archiveID})
			copy.add(t, "conversation_attachments", map[string]any{"id": "attachment_" + string(organization) + suffix, "conversation_id": conversationID, "principal_id": "first", "name": "example.txt", "mime": "text/plain", "size": len(blob), "artifact_ref": artifactRef, "created_at": now})
			copy.add(t, "conversation_attachment_blobs", map[string]any{"artifact_ref": artifactRef, "content": blob})
		}
		for position, target := range []string{"z-work", "a-work"} {
			copy.add(t, "message_references", map[string]any{"message_id": "message_" + string(organization), "conversation_id": "conversation_" + string(organization), "target_kind": "issue", "target_id": target, "label": target, "created_at": now})
			copy.records["message_references"][position].SourcePosition = new(int64(position))
		}
		copies[organization] = copy.seal(t)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		source := postgres.ConversationHistoryImportSource{BundlePath: copies[organization], Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: objects, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}}
		storesBefore, readsBefore := objects.stores, objects.reads
		objects.failKey = archiveReferences[organization].Key
		if _, err := repository.ImportConversationHistory(t.Context(), source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing archive import error=%v", err)
		}
		objects.failKey = ""
		for range 2 {
			result, err := repository.ImportConversationHistory(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if result.Rows != 8 {
				t.Fatalf("imported rows=%d want 8", result.Rows)
			}
		}
		if objects.stores-storesBefore != 8 || objects.reads-readsBefore != 10 || objects.storedPayloads[archivePayloads[organization]] != 1 {
			t.Fatalf("archive import store/read calls = %d/%d, original payload stores=%d; want 8/10 including source-manifest verification and two attachment objects and the existing archive stored once", objects.stores-storesBefore, objects.reads-readsBefore, objects.storedPayloads[archivePayloads[organization]])
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		repository, _ = repositoryFor(t, admin, smallPool())
		conversationID := "conversation_" + string(organization)
		messageID := "message_" + string(organization)
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			references, err := reader.MessageReferences(t.Context(), conversationID, messageID)
			if err != nil {
				return err
			}
			if len(references) != 2 || references[0].TargetID != "z-work" || references[1].TargetID != "a-work" {
				t.Fatalf("reference order changed: %+v", references)
			}
			second, err := reader.ConversationByID(t.Context(), conversationID+"_second")
			if err != nil {
				return err
			}
			if second.MessageCount != 1 || second.OrganizationID != organization {
				t.Fatalf("second conversation=%+v", second)
			}
			secondMessage, err := reader.Message(t.Context(), conversationID+"_second", messageID+"_second")
			if err != nil {
				return err
			}
			if secondMessage.Sequence != 1 || secondMessage.Text != "Example message" {
				t.Fatalf("second message=%+v", secondMessage)
			}
			conversation, err := reader.ConversationByID(t.Context(), conversationID)
			if err != nil {
				return err
			}
			if conversation.OrganizationID != organization || conversation.ProjectID != "prj_shared" || conversation.OwnerPrincipalID != "first" || conversation.MessageCount != 1 {
				t.Fatalf("imported conversation=%+v", conversation)
			}
			message, err := reader.Message(t.Context(), conversationID, messageID)
			if err != nil {
				return err
			}
			if message.Text != "Example message" || message.Sequence != 1 {
				t.Fatalf("imported message=%+v", message)
			}
			content, err := reader.ConversationContent(t.Context(), message.ContentReference)
			if err != nil {
				return err
			}
			if content.Reference != archiveReferences[organization] {
				t.Fatalf("import rewrote existing archive reference: got %+v want %+v", content.Reference, archiveReferences[organization])
			}
			attachments, err := reader.ArchiveBytes(t.Context())
			if err != nil {
				return err
			}
			if attachments <= 0 {
				t.Fatal("verified attachment bytes were not accounted")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", "_second"} {
			var contentID string
			if err := admin.QueryRowContext(t.Context(), `SELECT content_attachment_id FROM conversation_attachments WHERE organization_id=$1 AND id=$2`, organization, "attachment_"+string(organization)+suffix).Scan(&contentID); err != nil {
				t.Fatal(err)
			}
			if contentID == "" {
				t.Fatal("attachment has no archived content reference")
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
				object, err := reader.ConversationContent(t.Context(), contentID)
				if err != nil {
					return err
				}
				actual, err := objects.ReadDetail(t.Context(), string(organization), object.Reference)
				if err != nil {
					return err
				}
				if !bytes.Equal(actual, append([]byte("conversation attachment "+string(organization)+suffix), 0, 255, 128)) {
					t.Fatal("imported attachment bytes changed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	manifestPath := filepath.Join(copies["org_first"], "manifest.json")
	changedBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest postgres.NeutralImportManifest
	if err := json.Unmarshal(changedBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Identity.SourceID = "changed-source"
	changedBytes, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, changedBytes, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0400); err != nil {
		t.Fatal(err)
	}
	changedSource := postgres.ConversationHistoryImportSource{BundlePath: copies["org_first"], Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: objects, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_first"}}
	if _, err := repository.ImportConversationHistory(t.Context(), changedSource); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed source copy error=%v", err)
	}
	var count int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_attachments`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("conversation attachment count=%d error=%v", count, err)
	}
}

type countingHistoryObjects struct {
	base           *historyObjects
	stores         int
	reads          int
	failKey        string
	storedPayloads map[string]int
}

func (s *countingHistoryObjects) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.stores++
	s.storedPayloads[organization+"\x00"+string(raw)]++
	return s.base.StoreDetail(ctx, organization, raw)
}

func (s *countingHistoryObjects) ReadDetail(ctx context.Context, organization string, reference attachment.ArchiveReference) ([]byte, error) {
	s.reads++
	if reference.Key == s.failKey {
		return nil, os.ErrNotExist
	}
	return s.base.ReadDetail(ctx, organization, reference)
}
