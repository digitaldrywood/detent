package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestEventChangeMissingObjects(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, r, "org_first")
	f := seedHistory(t, r, "org_first", "first")
	for _, tc := range []struct {
		name, key string
		read      func() error
	}{
		{"receipt", f.receipt.PayloadObjectKey, func() error { _, err := f.content.Receipt(t.Context(), f.receipt); return err }},
		{"event", f.event.PayloadObjectKey, func() error { _, err := f.content.Event(t.Context(), f.event); return err }},
		{"snapshot", f.snapshot.PayloadObjectKey, func() error { _, err := f.content.CollaborationVersion(t.Context(), f.snapshot); return err }},
		{"change", f.change.PayloadObjectKey, func() error { _, err := f.content.Change(t.Context(), f.change); return err }},
		{"version", f.version.PayloadObjectKey, func() error { _, err := f.content.Version(t.Context(), f.version); return err }},
		{"evidence", f.evidence.PayloadObjectKey, func() error { _, err := f.content.Evidence(t.Context(), f.evidence); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.objects.mu.Lock()
			original := f.objects.objects[tc.key]
			delete(f.objects.objects, tc.key)
			f.objects.mu.Unlock()
			if err := tc.read(); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("missing object error=%v", err)
			}
			f.objects.mu.Lock()
			f.objects.objects[tc.key] = []byte("corrupt compressed object")
			f.objects.mu.Unlock()
			if err := tc.read(); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("corrupt object error=%v", err)
			}
			f.objects.mu.Lock()
			f.objects.objects[tc.key] = original
			f.objects.mu.Unlock()
		})
	}
}

func TestEventChangeRowSecurity(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		for _, suffix := range []string{"first", "second"} {
			seedHistory(t, r, org, suffix)
		}
	}
	runtime := runtimeFor(t, pool, pool)
	for _, table := range []string{"collaboration_events", "collaboration_versions", "change_requests", "change_versions", "change_evidence", "change_issue_links", "native_commands"} {
		t.Run(table, func(t *testing.T) {
			for _, scope := range []struct {
				org  string
				want int
			}{{"", 0}, {"org_first", 2}, {"org_second", 2}, {"org_unknown", 0}} {
				tx := tenantTx(t, runtime, pool, scope.org)
				var count int
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != scope.want {
					t.Errorf("scope=%s count=%d want=%d", scope.org, count, scope.want)
				}
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE organization_id='org_foreign'").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Error("foreign filter escaped RLS")
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			var columns string
			if err := admin.QueryRowContext(t.Context(), `SELECT string_agg(quote_ident(column_name),',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name<>'organization_id'`, table).Scan(&columns); err != nil {
				t.Fatal(err)
			}
			tx := tenantTx(t, runtime, pool, "org_first")
			selected := strings.ReplaceAll(columns, "payload_object_key", "replace(payload_object_key,'org_first','org_foreign')")
			_, err := tx.ExecContext(t.Context(), fmt.Sprintf("INSERT INTO %s(organization_id,%s) SELECT 'org_foreign',%s FROM %s LIMIT 1", table, columns, selected, table))
			requireSQLState(t, err, "42501")
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{"UPDATE " + table + " SET organization_id='org_first' WHERE organization_id='org_second'", "DELETE FROM " + table + " WHERE organization_id='org_second'"} {
				tx := tenantTx(t, runtime, pool, "org_first")
				result, err := tx.ExecContext(t.Context(), statement)
				if err != nil {
					t.Fatal(err)
				}
				count, err := result.RowsAffected()
				if err != nil || count != 0 {
					t.Fatalf("foreign mutation affected=%d error=%v", count, err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			var owner string
			var forced, enabled bool
			if err := admin.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner),relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, table).Scan(&owner, &enabled, &forced); err != nil {
				t.Fatal(err)
			}
			if owner != "detent_migration_v2" || !forced || !enabled {
				t.Fatalf("owner=%s forced=%v enabled=%v", owner, forced, enabled)
			}
		})
	}
}

func TestEventChangeRelationships(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
	}
	f := seedHistory(t, r, "org_first", "first")
	seedHistory(t, r, "org_first", "second")
	seedHistory(t, r, "org_second", "foreign")
	runtime := runtimeFor(t, pool, pool)
	for _, tc := range []struct{ name, statement, state string }{
		{"event issue", `INSERT INTO collaboration_events SELECT organization_id,project_id,'wi_missing', 'other_event',sequence,type,schema_version,actor_kind,actor_principal_id,event_revision,change_id,version_id,head_sha,run_id,attempt_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE id='event_first'`, "23503"},
		{"change project", `INSERT INTO change_requests SELECT organization_id,'prj_second',work_item_id,'bad_change',revision,title,current_version_id,evidence_sequence,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_requests WHERE id='change_first'`, "23503"},
		{"version foreign change", `INSERT INTO change_versions SELECT organization_id,'bad_version','change_foreign',number,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,attempt_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_versions LIMIT 1`, "23503"},
		{"evidence wrong change version", `INSERT INTO change_evidence SELECT organization_id,change_id,99,'version_second',kind,'new_source',actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_evidence WHERE change_id='change_first'`, "23503"},
		{"link wrong project", `INSERT INTO change_issue_links VALUES ('org_first','prj_second','wi_second_1','change_first')`, "23503"},
		{"foreign current version", `UPDATE change_requests SET current_version_id='version_foreign' WHERE id='change_first'`, "23503"},
		{"duplicate version number", `INSERT INTO change_versions SELECT organization_id,'other_version',change_id,number,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,attempt_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_versions WHERE id='version_first'`, "23505"},
		{"duplicate event sequence", `INSERT INTO collaboration_events SELECT organization_id,project_id,work_item_id,'other_event',sequence,type,schema_version,actor_kind,actor_principal_id,event_revision,change_id,version_id,head_sha,run_id,attempt_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE id='event_first'`, "23505"},
		{"cross tenant object", `UPDATE change_requests SET payload_object_key=replace(payload_object_key,'org_first','org_second') WHERE id='change_first'`, "23514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := tenantTx(t, runtime, pool, "org_first")
			_, err := tx.ExecContext(t.Context(), tc.statement)
			if err == nil && tc.name == "foreign current version" {
				_, err = tx.ExecContext(t.Context(), "SET CONSTRAINTS ALL IMMEDIATE")
			}
			requireSQLState(t, err, tc.state)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		write func(eventstore.Writer) error
	}{
		{"tenant mismatch", func(w eventstore.Writer) error {
			row := f.event
			row.OrganizationID = "org_second"
			_, err := w.AppendEvent(t.Context(), row)
			return err
		}},
		{"missing parent", func(w eventstore.Writer) error {
			row := f.event
			row.WorkItemID = "missing"
			_, err := w.AppendEvent(t.Context(), row)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, tc.write)
			want := corestore.ErrTenantMismatch
			if tc.name == "missing parent" {
				want = corestore.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
		})
	}
}

func TestEventChangeUpdateAndPages(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, r, "org_first")
	f := seedHistory(t, r, "org_first", "first")
	original := f.change
	updatedValue, err := f.content.Change(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	updatedValue.Title = "Updated title"
	updatedValue.Body = "Updated body"
	updatedValue.CurrentVersion = f.version.ID
	update, err := f.content.PrepareChange(t.Context(), "org_first", historyJSON(t, updatedValue))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		var err error
		update, err = w.UpdateChange(t.Context(), update)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if update.Revision != original.Revision+1 || update.EvidenceSequence != 1 {
		t.Fatalf("updated record=%+v", update)
	}
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error { _, err := w.UpdateChange(t.Context(), original); return err }); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale update error=%v", err)
	}
	for _, limit := range []int{0, 201} {
		err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
			_, err := reader.Events(t.Context(), "prj_first", "wi_first_1", 0, limit)
			return err
		})
		if !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("limit %d error=%v", limit, err)
		}
	}
	if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
		row, err := reader.Change(t.Context(), update.ID)
		if err != nil {
			return err
		}
		if row.Revision != 2 || row.CurrentVersionID == nil || *row.CurrentVersionID != f.version.ID {
			t.Errorf("updated row=%+v", row)
		}
		update = row
		events, err := reader.Events(t.Context(), "prj_first", "wi_first_1", f.event.Sequence, 1)
		if err != nil {
			return err
		}
		if len(events) != 0 {
			t.Error("event keyset repeated anchor")
		}
		versions, err := reader.Versions(t.Context(), f.change.ID, f.version.Number, 1)
		if err != nil {
			return err
		}
		if len(versions) != 0 {
			t.Error("version keyset repeated anchor")
		}
		evidence, err := reader.Evidence(t.Context(), f.change.ID, f.evidence.Sequence, 1)
		if err != nil {
			return err
		}
		if len(evidence) != 0 {
			t.Error("evidence keyset repeated anchor")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hydrated, err := f.content.Change(t.Context(), update)
	if err != nil || hydrated.Body != "Updated body" || hydrated.CurrentVersion != f.version.ID || hydrated.Revision != 2 {
		t.Fatalf("updated payload=%+v error=%v", hydrated, err)
	}
}

func TestEventChangeLegacyDecodeRejectsInvalidPayload(t *testing.T) {
	c := eventstore.NewContent(newHistoryObjects())
	for _, tc := range []struct {
		name    string
		prepare func([]byte) error
	}{
		{"event", func(raw []byte) error { _, err := c.PrepareEvent(t.Context(), "org_first", raw); return err }},
		{"change", func(raw []byte) error { _, err := c.PrepareChange(t.Context(), "org_first", raw); return err }},
		{"version", func(raw []byte) error { _, err := c.PrepareVersion(t.Context(), "org_first", raw); return err }},
		{"evidence", func(raw []byte) error {
			_, err := c.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: "org_first", Kind: "discussion"}, raw)
			return err
		}},
		{"snapshot", func(raw []byte) error {
			_, err := c.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_first"}, raw)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.prepare([]byte("{private-payload")); !errors.Is(err, eventstore.ErrInvalidRecord) || strings.Contains(err.Error(), "private-payload") {
				t.Fatalf("invalid payload error=%v", err)
			}
		})
	}
}

func TestEventChangeEvidenceKinds(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, r, "org_first")
	f := seedHistory(t, r, "org_first", "first")
	for _, tc := range []struct {
		kind     string
		value    any
		source   string
		decision string
	}{
		{"review", tracker.ChangeReview{ID: "review", VersionID: f.version.ID, Decision: "approved", Body: "Review body", Actor: historyActor(), CreatedAt: historyAt()}, "review-receipt", "approved"},
		{"check", tracker.ChangeCheck{ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "check-run", HeadSHA: "head", PolicyID: "policy", Conclusion: "success"}, VersionID: f.version.ID, Actor: historyActor(), ReceivedAt: historyAt()}, "check-run", ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			row, err := f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: "org_first", ChangeID: f.change.ID, Kind: tc.kind, SourceKey: new(tc.source)}, historyJSON(t, tc.value))
			if err != nil {
				t.Fatal(err)
			}
			if row.ActorKind != "human" || row.ActorPrincipalID != "example" || row.Decision != tc.decision || row.VersionID == nil || *row.VersionID != f.version.ID || row.SourceKey == nil || *row.SourceKey != tc.source {
				t.Fatalf("typed evidence=%+v", row)
			}
			if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
				var err error
				row, err = w.AppendEvidence(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var stored []eventstore.Evidence
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				var err error
				stored, err = reader.Evidence(t.Context(), f.change.ID, row.Sequence-1, 1)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 || stored[0].Kind != tc.kind {
				t.Fatalf("stored evidence=%+v", stored)
			}
			raw, err := f.content.Evidence(t.Context(), stored[0])
			if err != nil || string(raw) != string(historyJSON(t, tc.value)) {
				t.Fatalf("evidence payload=%s error=%v", raw, err)
			}
		})
	}
}
