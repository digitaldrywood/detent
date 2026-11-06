package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
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
	copy := newBillingImportCopy(t, catalog, tables...)
	now := "2026-11-01T18:00:00.123456Z"
	objects := &countingHistoryObjects{base: newHistoryObjects(), storedPayloads: make(map[string]int)}
	archiveReferences := make(map[tracker.OrganizationID]attachment.ArchiveReference)
	archivePayloads := make(map[tracker.OrganizationID]string)
	for _, organization := range []string{"org_first", "org_second"} {
		conversationID := "conversation_" + organization
		messageID := "message_" + organization
		artifactRef := "artifact_" + organization
		blob := []byte("conversation attachment " + organization)
		if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,origin,title,visibility,status,execution_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, conversationID, organization, "prj_shared", "first", "user", "Example conversation", "private", "active", "{}", now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_messages(id,conversation_id,seq,role,kind,text,data_json,delivery,actor_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, messageID, conversationID, 1, "user", "text", "Example message", "{}", "saved", "{}", now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_attachments(id,conversation_id,principal_id,name,mime,size,artifact_ref,created_at) VALUES(?,?,?,?,?,?,?,?)`, "attachment_"+organization, conversationID, "first", "example.txt", "text/plain", len(blob), artifactRef, now); err != nil {
			t.Fatal(err)
		}
		if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_attachment_blobs(artifact_ref,content) VALUES(?,?)`, artifactRef, blob); err != nil {
			t.Fatal(err)
		}
		archiveID := "archive_" + organization
		archiveBytes := []byte(`{"body":"preserved existing archive"}`)
		archivePayloads[tracker.OrganizationID(organization)] = organization + "\x00" + string(archiveBytes)
		archiveRef, err := objects.StoreDetail(t.Context(), organization, archiveBytes)
		if err != nil {
			t.Fatal(err)
		}
		archiveReferences[tracker.OrganizationID(organization)] = archiveRef
		if _, err := copy.db.ExecContext(t.Context(), `UPDATE conversation_messages SET content_attachment_id=? WHERE id=?`, archiveID, messageID); err != nil {
			t.Fatal(err)
		}
		if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version,storage_binding_id,customer_owned) VALUES(?,?,?,?,?,?,?,?,?,'archive','organization',?,?,?,?,'',0)`, archiveID, organization, "prj_shared", "first", "history.json.gz", "application/gzip", archiveRef.CompressedSize, archiveRef.SHA256, now, archiveRef.Key, archiveRef.UncompressedSize, archiveRef.Codec, archiveRef.SchemaVersion); err != nil {
			t.Fatal(err)
		}
	}
	copyPath := copy.seal(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		source := postgres.ConversationHistoryImportSource{CopyPath: copyPath, Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: objects, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}}
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
			if result.Rows != 3 {
				t.Fatalf("imported rows=%d want 3", result.Rows)
			}
		}
		if objects.stores-storesBefore != 4 || objects.reads-readsBefore != 6 || objects.storedPayloads[archivePayloads[organization]] != 1 {
			t.Fatalf("archive import store/read calls = %d/%d, original payload stores=%d; want 4/6 including source-pin verification and the existing archive stored once", objects.stores-storesBefore, objects.reads-readsBefore, objects.storedPayloads[archivePayloads[organization]])
		}
		conversationID := "conversation_" + string(organization)
		messageID := "message_" + string(organization)
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
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
		var contentID string
		if err := admin.QueryRowContext(t.Context(), `SELECT content_attachment_id FROM conversation_attachments WHERE organization_id=$1 AND id=$2`, organization, "attachment_"+string(organization)).Scan(&contentID); err != nil {
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
			if !bytes.Equal(actual, []byte("conversation attachment "+string(organization))) {
				t.Fatal("imported attachment bytes changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	changedBytes, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	changedPath := filepath.Join(t.TempDir(), "changed.sqlite")
	if err := os.WriteFile(changedPath, changedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := sql.Open("sqlite", changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.ExecContext(t.Context(), `DELETE FROM conversations WHERE organization_id='org_first'`); err != nil {
		t.Fatal(err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(changedPath, 0400); err != nil {
		t.Fatal(err)
	}
	changedSource := postgres.ConversationHistoryImportSource{CopyPath: changedPath, Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: objects, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_first"}}
	if _, err := repository.ImportConversationHistory(t.Context(), changedSource); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed source copy error=%v", err)
	}
	var count int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_attachments`).Scan(&count); err != nil || count != 2 {
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
