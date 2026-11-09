package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestConversationHistoryImportPreservesContinuationOwnership(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{"conversations", "conversation_messages", "conversation_questions", "conversation_commands", "conversation_events", "conversation_starts", "conversation_audience_events", "conversation_turn_batches", "conversation_tool_revisions", "message_references", "conversation_attachments", "conversation_attachment_blobs", "attachments"}
	copies := make(map[tracker.OrganizationID]*neutralBillingFixture)
	paths := make(map[tracker.OrganizationID]string)
	objects := &countingHistoryObjects{base: newHistoryObjects(), storedPayloads: make(map[string]int)}
	failKeys := make(map[tracker.OrganizationID]string)
	now := "2026-11-01T18:00:00.123456Z"
	cases := []struct {
		name, delivery             string
		archived, completed, again bool
		want                       bool
		padding                    int
	}{
		{name: "acknowledged", delivery: "interrupted", want: true},
		{name: "unacknowledged", delivery: "unknown", want: true},
		{name: "referenced_compacted", delivery: "delivered", archived: true, want: true, padding: 130},
		{name: "completed_then_replaced", delivery: "delivered", archived: true, completed: true},
		{name: "new_grant_after_completion", delivery: "delivered", completed: true, again: true, want: true},
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		copy := newNeutralBillingFixture(t, organization, tables...)
		copies[organization] = copy
		seedTenantAccess(t, repository, organization)
		for index, tc := range cases {
			id := "continuation_" + string(organization) + "_" + tc.name
			itemID := tracker.NativeWorkItemID(fmt.Sprintf("wi_continue_%s_%d", organization, index))
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_shared", NativeID: itemID, Number: int64(index + 10), Title: "Example continuation"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			copy.add(t, "conversations", map[string]any{"id": id, "organization_id": organization, "project_id": "prj_shared", "owner_principal_id": "first", "origin": "worker", "title": "Example continuation", "visibility": "shared", "status": "active", "execution_json": "{}", "created_at": now, "updated_at": now, "work_item_id": itemID, "event_seq": 12 + tc.padding})
			messageID := "message_" + string(organization) + "_" + tc.name
			copy.add(t, "conversation_commands", map[string]any{"conversation_id": id, "key": "continue", "kind": "continue", "request_hash": "source_request", "receipt_json": "{}", "created_at": now, "updated_at": now})
			copy.add(t, "conversation_messages", map[string]any{"id": messageID, "conversation_id": id, "seq": 1, "role": "user", "kind": "continue", "text": "Continue", "data_json": "{}", "delivery": tc.delivery, "actor_json": "{}", "command_key": "continue", "attempt_id": "original_attempt", "created_at": now, "updated_at": now})
			copy.add(t, "conversation_commands", map[string]any{"conversation_id": id, "key": "reservation_only", "kind": "continue", "request_hash": "reservation_request", "receipt_json": "{}", "created_at": now, "updated_at": now})
			for position := range tc.padding {
				copy.add(t, "conversation_messages", map[string]any{"id": fmt.Sprintf("padding_%s_%03d", organization, position), "conversation_id": id, "seq": position + 2, "role": "assistant", "kind": "text", "text": "Example response", "data_json": "{}", "delivery": "completed", "actor_json": "{}", "created_at": now, "updated_at": now})
			}
			appendEvent := func(sequence int, kind string, body map[string]string) {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				contentID := ""
				metadata := 0
				if tc.archived {
					contentID = fmt.Sprintf("archive_%s_%s_%d", organization, tc.name, sequence)
					ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					failKeys[organization] = ref.Key
					copy.add(t, "attachments", map[string]any{"id": contentID, "organization_id": organization, "project_id": "prj_shared", "uploader": "first", "name": "history.json.gz", "content_type": "application/gzip", "size": ref.CompressedSize, "sha256": ref.SHA256, "created_at": now, "kind": "archive", "retention_class": "organization", "object_key": ref.Key, "uncompressed_size": ref.UncompressedSize, "codec": ref.Codec, "schema_version": ref.SchemaVersion, "storage_binding_id": "", "customer_owned": 0})
					metadata = 1
					raw = []byte(`{}`)
				}
				copy.add(t, "conversation_events", map[string]any{"conversation_id": id, "seq": sequence, "type": kind, "body_json": string(raw), "created_at": now, "content_attachment_id": contentID, "query_metadata_version": metadata, "object_id": body["id"], "message_kind": body["kind"], "execution_status": body["status"]})
			}
			appendEvent(1, "message.accepted", map[string]string{"id": messageID, "kind": "continue"})
			status := "interrupted"
			if tc.completed {
				status = "completed"
			}
			appendEvent(4, "execution.updated", map[string]string{"status": status})
			appendEvent(7, "execution.updated", map[string]string{"status": "interrupted"})
			if tc.again {
				copy.add(t, "conversation_commands", map[string]any{"conversation_id": id, "key": "again", "kind": "continue", "request_hash": "second_source_request", "receipt_json": "{}", "created_at": now, "updated_at": now})
				copy.add(t, "conversation_messages", map[string]any{"id": messageID + "_again", "conversation_id": id, "seq": 2, "role": "user", "kind": "continue", "text": "Continue again", "data_json": "{}", "delivery": "saved", "actor_json": "{}", "command_key": "again", "created_at": now, "updated_at": now})
				appendEvent(10, "message.accepted", map[string]string{"id": messageID + "_again", "kind": "continue"})
			}
		}
	}
	var sources []postgres.ConversationHistoryImportSource
	legacyRows := make(map[tracker.OrganizationID][]legacyContinuationRow)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		legacyRows[organization] = readLegacyContinuationRows(t, copies[organization], catalog, organization)
		paths[organization] = copies[organization].seal(t, 2)
	}
	invalid := newNeutralBillingFixture(t, "org_first", tables...)
	for name, rows := range copies["org_first"].records {
		for _, row := range rows {
			if name == "conversation_events" && string(row.Values["conversation_id"].Data) == "continuation_org_first_acknowledged" && string(row.Values["type"].Data) == "message.accepted" {
				continue
			}
			invalid.records[name] = append(invalid.records[name], row)
		}
	}
	invalidSource := postgres.ConversationHistoryImportSource{BundlePath: invalid.seal(t, 2), Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_first"}, Objects: objects}
	if _, err := repository.ImportConversationHistory(t.Context(), invalidSource); err == nil || !strings.Contains(err.Error(), "accepted history is incomplete") {
		t.Fatalf("missing accepted event error=%v", err)
	}
	var pinned int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM neutral_import_sources WHERE organization_id='org_first'`).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != 0 {
		t.Fatalf("invalid continuation source pinned %d receipts", pinned)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		source := postgres.ConversationHistoryImportSource{BundlePath: paths[organization], Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}, Objects: objects}
		sources = append(sources, source)
		objects.failKey = failKeys[organization]
		if _, err := repository.ImportConversationHistory(t.Context(), source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial import error=%v", err)
		}
		objects.failKey = ""
		var snapshot string
		if err := admin.QueryRowContext(t.Context(), `SELECT snapshot_id FROM conversation_history_imports WHERE organization_id=$1 AND NOT completed`, organization).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		for _, row := range legacyRows[organization] {
			digest := legacyContinuationReceiptHash(t, snapshot, row)
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id=$2 AND source_table='conversations' AND source_row_id=$3`, digest, organization, row.row); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET execution_continuation=false WHERE organization_id=$1 AND id=$2`, organization, row.id); err != nil {
				t.Fatal(err)
			}
		}
		first := legacyRows[organization][0]
		var verifiedHash string
		if err := admin.QueryRowContext(t.Context(), `SELECT request_hash FROM conversation_history_import_rows WHERE organization_id=$1 AND source_table='conversations' AND source_row_id=$2`, organization, first.row).Scan(&verifiedHash); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id=$2 AND source_table='conversations' AND source_row_id=$3`, strings.Repeat("f", 64), organization, first.row); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ImportConversationHistory(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed legacy receipt accepted: %v", err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id=$2 AND source_table='conversations' AND source_row_id=$3`, verifiedHash, organization, first.row); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET revision=revision+1 WHERE organization_id=$1 AND id=$2`, organization, first.id); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ImportConversationHistory(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("advanced imported conversation overwritten: %v", err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET revision=revision-1 WHERE organization_id=$1 AND id=$2`, organization, first.id); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := repository.ImportConversationHistory(t.Context(), source); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, tc := range cases {
				stored, err := r.ConversationByID(t.Context(), "continuation_"+string(organization)+"_"+tc.name)
				if err != nil {
					return err
				}
				if stored.ExecutionContinuation != tc.want {
					t.Errorf("%s/%s continuation=%t want=%t", organization, tc.name, stored.ExecutionContinuation, tc.want)
				}
				message, err := r.Message(t.Context(), stored.ID, "message_"+string(organization)+"_"+tc.name)
				if err != nil {
					return err
				}
				if message.AttemptID != "original_attempt" || string(message.Delivery) != tc.delivery {
					t.Errorf("imported Continue source changed: %+v", message)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("upgrade imported compacted history and later PostgreSQL events", func(t *testing.T) {
		before := make(map[string][32]byte)
		for _, source := range sources {
			before[source.BundlePath] = continuationBundleDigest(t, source.BundlePath)
		}
		for _, source := range sources {
			organization := source.Source.OrganizationID
			for _, tc := range cases {
				id := "continuation_" + organization + "_" + tc.name
				status := "interrupted"
				if tc.name == "acknowledged" && organization == "org_second" {
					status = "completed"
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json,execution_status,query_metadata_version) VALUES($1,$2,$4,'execution.updated','{}',$3,1)`, organization, id, status, 13+tc.padding); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET event_seq=$3 WHERE organization_id=$1 AND id=$2`, organization, id, 13+tc.padding); err != nil {
					t.Fatal(err)
				}
				if organization == "org_first" && tc.name == "completed_then_replaced" {
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) VALUES($1,$2,'postgres_continue','continue','postgres_request','{}')`, organization, id); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,command_key) VALUES($1,'postgres_continue',$2,2,'user','continue','sent','postgres_continue')`, organization, id); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json,object_id,message_kind,query_metadata_version) VALUES($1,$2,14,'message.accepted','{}','postgres_continue','continue',1)`, organization, id); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json,execution_status,query_metadata_version) VALUES($1,$2,15,'execution.updated','{}','interrupted',1)`, organization, id); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET event_seq=15 WHERE organization_id=$1 AND id=$2`, organization, id); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		admin.SetMaxOpenConns(1)
		admin.SetMaxIdleConns(1)
		if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
			t.Fatal(err)
		}
		provider, err := migrationProvider(admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.DownTo(t.Context(), 20261108224926); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET execution_continuation=NULL`); err != nil {
			t.Fatal(err)
		}
		var legacySnapshot string
		if err := admin.QueryRowContext(t.Context(), `SELECT snapshot_id FROM conversation_history_imports WHERE organization_id='org_second' AND completed`).Scan(&legacySnapshot); err != nil {
			t.Fatal(err)
		}
		for _, row := range legacyRows["org_second"] {
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id='org_second' AND source_table='conversations' AND source_row_id=$2`, legacyContinuationReceiptHash(t, legacySnapshot, row), row.row); err != nil {
				t.Fatal(err)
			}
		}
		login, password := "continuation_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
		quoted := pgx.Identifier{login}.Sanitize()
		if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE detent_migration_v2"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			deadline, _ := t.Deadline()
			ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
			defer cancel()
			if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
				t.Error(err)
			}
		})
		connection := template.Connection(t, admin, login, password)
		refuse := func(name string, input []postgres.ConversationHistoryImportSource) error {
			t.Helper()
			err := postgres.Migrate(t.Context(), connection, os.DirFS("migrations"), input...)
			if err == nil {
				t.Fatalf("%s accepted incomplete recovery evidence", name)
			}
			var recovered int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversations WHERE execution_continuation IS NOT NULL`).Scan(&recovered); err != nil {
				t.Fatal(err)
			}
			if recovered != 0 {
				t.Fatalf("%s partially recovered %d conversations", name, recovered)
			}
			return err
		}
		refuse("missing declared source", nil)
		var receipt string
		if err := admin.QueryRowContext(t.Context(), `SELECT request_hash FROM conversation_history_import_rows WHERE organization_id='org_second' AND source_table='conversations' ORDER BY source_row_id LIMIT 1`).Scan(&receipt); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id='org_second' AND source_table='conversations' AND source_row_id=(SELECT min(source_row_id) FROM conversation_history_import_rows WHERE organization_id='org_second' AND source_table='conversations')`, strings.Repeat("f", 64)); err != nil {
			t.Fatal(err)
		}
		refuse("mismatched source row receipt", sources)
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id='org_second' AND source_table='conversations' AND source_row_id=(SELECT min(source_row_id) FROM conversation_history_import_rows WHERE organization_id='org_second' AND source_table='conversations')`, receipt); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM conversation_events WHERE organization_id='org_second' AND conversation_id='continuation_org_second_completed_then_replaced' AND seq=4`); err != nil {
			t.Fatal(err)
		}
		refuse("missing imported completion boundary", sources)
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM neutral_import_sources WHERE organization_id='org_second'`); err != nil {
			t.Fatal(err)
		}
		forged := newNeutralBillingFixture(t, "org_second", tables...)
		for name, rows := range copies["org_second"].records {
			for _, row := range rows {
				if name == "conversation_events" && string(row.Values["conversation_id"].Data) == "continuation_org_second_completed_then_replaced" && string(row.Values["seq"].Data) == "4" {
					continue
				}
				forged.records[name] = append(forged.records[name], row)
			}
		}
		forgedSources := slices.Clone(sources)
		forgedSources[1].BundlePath = forged.seal(t, 2)
		if err := refuse("completion absent from supplied bundle and PostgreSQL", forgedSources); !strings.Contains(err.Error(), "receipt history is incomplete") {
			t.Fatalf("historical row receipt proof error=%v", err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json,execution_status,query_metadata_version) VALUES('org_second','continuation_org_second_completed_then_replaced',4,'execution.updated','{}','completed',1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET event_seq=14 WHERE organization_id='org_second' AND id='continuation_org_second_unacknowledged'`); err != nil {
			t.Fatal(err)
		}
		refuse("missing PostgreSQL suffix", sources)
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET event_seq=13 WHERE organization_id='org_second' AND id='continuation_org_second_unacknowledged'`); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := postgres.Migrate(t.Context(), connection, os.DirFS("migrations"), sources...); err != nil {
				t.Fatal(err)
			}
		}
		for _, source := range sources {
			for _, tc := range cases {
				var actual bool
				if err := admin.QueryRowContext(t.Context(), `SELECT execution_continuation FROM conversations WHERE organization_id=$1 AND id=$2`, source.Source.OrganizationID, "continuation_"+source.Source.OrganizationID+"_"+tc.name).Scan(&actual); err != nil {
					t.Fatal(err)
				}
				want := tc.want && !(source.Source.OrganizationID == "org_second" && tc.name == "acknowledged")
				if source.Source.OrganizationID == "org_first" && tc.name == "completed_then_replaced" {
					want = true
				}
				if actual != want {
					t.Errorf("upgraded %s/%s continuation=%t want=%t", source.Source.OrganizationID, tc.name, actual, want)
				}
			}
		}
		var staging bool
		if err := admin.QueryRowContext(t.Context(), `SELECT to_regclass('conversation_continuation_recovery') IS NOT NULL`).Scan(&staging); err != nil {
			t.Fatal(err)
		}
		if staging {
			t.Error("migration recovery staging remains after upgrade")
		}
		var rebound int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM neutral_import_sources WHERE organization_id='org_second'`).Scan(&rebound); err != nil {
			t.Fatal(err)
		}
		if rebound != 0 {
			t.Error("legacy recovery created a replacement neutral source pin")
		}
		for _, source := range sources {
			if continuationBundleDigest(t, source.BundlePath) != before[source.BundlePath] {
				t.Error("verified recovery source changed")
			}
		}
	})
}

type legacyContinuationRow struct {
	id      string
	row     int64
	columns []string
	values  []any
}

func readLegacyContinuationRows(t *testing.T, copy *neutralBillingFixture, catalog storagecatalog.Catalog, organization tracker.OrganizationID) []legacyContinuationRow {
	t.Helper()
	definition := catalog.Tables["hub/conversations"]
	var sourceColumns []string
	for name := range definition.Columns {
		sourceColumns = append(sourceColumns, name)
	}
	slices.Sort(sourceColumns)
	var result []legacyContinuationRow
	for _, sourceRow := range copy.records["conversations"] {
		row := legacyContinuationRow{row: sourceRow.SourceRowID}
		values := map[string]any{"organization_id": organization}
		for _, name := range sourceColumns {
			column := definition.Columns[name]
			if column.Disposition == "rebuild" {
				if name == "conversion_state" {
					values[name] = int16(2)
				}
				continue
			}
			cell := sourceRow.Values[name]
			decoded, err := catalog.Decode("hub/conversations", name, cell)
			if err != nil {
				t.Fatalf("legacy source column %s: %v", name, err)
			}
			var value any
			if !decoded.Null {
				switch column.Codec {
				case "text":
					value = decoded.Text
				case "json":
					value = string(decoded.Bytes)
				case "int32":
					value = int32(decoded.Integer)
				case "int64":
					value = decoded.Integer
				case "bool01":
					value = decoded.Boolean
				case "float64":
					value = decoded.Real
				case "timestamp", "unix-seconds":
					value = time.Time(decoded.Time)
				default:
					t.Fatalf("unsupported legacy fixture codec %s", column.Codec)
				}
			}
			values[column.Targets[0].Column] = value
		}
		row.id = values["id"].(string)
		for name := range values {
			row.columns = append(row.columns, name)
		}
		slices.Sort(row.columns)
		for _, name := range row.columns {
			row.values = append(row.values, values[name])
		}
		result = append(result, row)
	}
	return result
}

func continuationBundleDigest(t *testing.T, directory string) [32]byte {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		digest.Write([]byte(entry.Name()))
		digest.Write(raw)
	}
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func legacyContinuationReceiptHash(t *testing.T, snapshot string, row legacyContinuationRow) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Copy    string
		Table   string
		Row     int64
		Columns []string
		Values  []any
	}{snapshot, "conversations", row.row, row.columns, row.values})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
