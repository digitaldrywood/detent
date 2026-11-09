package postgres_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	copy := newBillingImportCopy(t, catalog, tables...)
	objects := &countingHistoryObjects{base: newHistoryObjects(), storedPayloads: make(map[string]int)}
	failKeys := make(map[tracker.OrganizationID]string)
	now := "2026-11-01T18:00:00.123456Z"
	cases := []struct {
		name, delivery             string
		archived, completed, again bool
		want                       bool
	}{
		{name: "acknowledged", delivery: "interrupted", want: true},
		{name: "unacknowledged", delivery: "unknown", want: true},
		{name: "referenced_compacted", delivery: "delivered", archived: true, want: true},
		{name: "completed_then_replaced", delivery: "delivered", archived: true, completed: true},
		{name: "new_grant_after_completion", delivery: "delivered", completed: true, again: true, want: true},
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
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
			if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,origin,title,visibility,status,execution_json,created_at,updated_at,work_item_id,event_seq) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, organization, "prj_shared", "first", "worker", "Example continuation", "shared", "active", "{}", now, now, itemID, 12); err != nil {
				t.Fatal(err)
			}
			messageID := "message_" + string(organization) + "_" + tc.name
			if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_commands(conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at) VALUES(?,?,'continue','source_request','{}',?,?)`, id, "continue", now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_messages(id,conversation_id,seq,role,kind,text,data_json,delivery,actor_json,command_key,attempt_id,created_at,updated_at) VALUES(?,?,1,'user','continue','Continue','{}',?,'{}','continue','original_attempt',?,?)`, messageID, id, tc.delivery, now, now); err != nil {
				t.Fatal(err)
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
					if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version,storage_binding_id,customer_owned) VALUES(?,?,?,?,?,?,?,?,?,'archive','organization',?,?,?,?,'',0)`, contentID, organization, "prj_shared", "first", "history.json.gz", "application/gzip", ref.CompressedSize, ref.SHA256, now, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion); err != nil {
						t.Fatal(err)
					}
					metadata = 1
					raw = []byte(`{}`)
				}
				if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_events(conversation_id,seq,type,body_json,created_at,content_attachment_id,query_metadata_version,object_id,message_kind,execution_status) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, sequence, kind, string(raw), now, contentID, metadata, body["id"], body["kind"], body["status"]); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent(1, "message.accepted", map[string]string{"id": messageID, "kind": "continue"})
			status := "interrupted"
			if tc.completed {
				status = "completed"
			}
			appendEvent(4, "execution.updated", map[string]string{"status": status})
			appendEvent(7, "execution.updated", map[string]string{"status": "interrupted"})
			if tc.again {
				if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_commands(conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at) VALUES(?,'again','continue','second_source_request','{}',?,?)`, id, now, now); err != nil {
					t.Fatal(err)
				}
				if _, err := copy.db.ExecContext(t.Context(), `INSERT INTO conversation_messages(id,conversation_id,seq,role,kind,text,data_json,delivery,actor_json,command_key,created_at,updated_at) VALUES(?,?,2,'user','continue','Continue again','{}','saved','{}','again',?,?)`, messageID+"_again", id, now, now); err != nil {
					t.Fatal(err)
				}
				appendEvent(10, "message.accepted", map[string]string{"id": messageID + "_again", "kind": "continue"})
			}
		}
	}
	legacyRows := make(map[tracker.OrganizationID][]legacyContinuationRow)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		legacyRows[organization] = readLegacyContinuationRows(t, copy.db, catalog, organization)
	}
	path := copy.seal(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		source := postgres.ConversationHistoryImportSource{CopyPath: path, Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}, Objects: objects}
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
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_history_import_rows SET request_hash=$1 WHERE organization_id=$2 AND source_table='conversations' AND source_row_id=$3`, hex.EncodeToString(digest[:]), organization, row.row); err != nil {
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
}

type legacyContinuationRow struct {
	id      string
	row     int64
	columns []string
	values  []any
}

func readLegacyContinuationRows(t *testing.T, db *sql.DB, catalog storagecatalog.Catalog, organization tracker.OrganizationID) []legacyContinuationRow {
	t.Helper()
	definition := catalog.Tables["hub/conversations"]
	var sourceColumns []string
	for name := range definition.Columns {
		sourceColumns = append(sourceColumns, name)
	}
	slices.Sort(sourceColumns)
	selection := "rowid"
	for _, name := range sourceColumns {
		selection += `,CAST("` + name + `" AS TEXT)`
	}
	rows, err := db.QueryContext(t.Context(), "SELECT "+selection+" FROM conversations WHERE organization_id=? ORDER BY rowid", organization)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []legacyContinuationRow
	for rows.Next() {
		row := legacyContinuationRow{}
		cells := make([]sql.NullString, len(sourceColumns))
		args := []any{&row.row}
		for i := range cells {
			args = append(args, &cells[i])
		}
		if err := rows.Scan(args...); err != nil {
			t.Fatal(err)
		}
		values := map[string]any{"organization_id": organization}
		for i, name := range sourceColumns {
			column := definition.Columns[name]
			if column.Disposition == "rebuild" {
				if name == "conversion_state" {
					values[name] = int16(2)
				}
				continue
			}
			cell := storagecatalog.Cell{Null: !cells[i].Valid}
			if cells[i].Valid {
				cell.Encoding = "utf8"
				cell.Data = []byte(cells[i].String)
			}
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
