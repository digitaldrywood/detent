package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBillingEventMetadataRetainsReplayAndSnapshotSemantics(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 2, BackgroundConnections: 1})
	content := eventstore.NewContent(newHistoryObjects())
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		read := func(t *testing.T, count int64, known bool, eventAt, sessionAt *time.Time) {
			t.Helper()
			if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				got, ready, err := r.(billingstore.PopulationReader).BillingPopulation(t.Context(), "retained_collaboration_events", historyAt(), 0)
				if err != nil {
					return err
				}
				if got != count || ready != known {
					return fmt.Errorf("retained events %s=%d/%t, want %d/%t", org, got, ready, count, known)
				}
				for _, field := range []struct {
					name string
					get  func(context.Context) (*time.Time, error)
					want *time.Time
				}{
					{"event", r.LatestEventRecordedAt, eventAt}, {"session", r.LatestAccessSessionCreatedAt, sessionAt},
				} {
					got, err := field.get(t.Context())
					if field.want == nil {
						if !errors.Is(err, corestore.ErrNotFound) {
							return fmt.Errorf("empty %s did not report not-found: %v", field.name, err)
						}
					} else if err != nil {
						return err
					}
					if (got == nil) != (field.want == nil) || got != nil && !got.Equal(*field.want) {
						return fmt.Errorf("%s %s=%v, want %v", org, field.name, got, field.want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		read(t, 0, true, nil, nil)
		at := historyAt().Add(time.Duration(index) * time.Hour)
		prepare := func(id string, sequence int64, recorded time.Time) eventstore.Event {
			value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", AggregateSequence: sequence, Type: "issue.edited", SchemaVersion: 1, Actor: historyActor(), RecordedAt: recorded, Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}}}
			row, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			return row
		}
		first := prepare("retained_first", 1, at)
		second := prepare("retained_hidden", 100, at.Add(-time.Hour))
		second.HistoryHidden = true
		appendRow := func(row eventstore.Event) error {
			return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), row); return err })
		}
		for _, row := range []eventstore.Event{first, second, first, second} {
			if err := appendRow(row); err != nil {
				t.Fatal(err)
			}
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", RecordID: "metadata_version", Revision: 1}, historyJSON(t, tracker.NativeIssue{Title: "Example"}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		read(t, 2, true, &at, nil)
		changed := first
		changed.Type = "issue.created"
		if err := appendRow(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("conflicting replay=%v", err)
		}
		rollback := errors.New("rollback event fixture")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.AppendEvent(t.Context(), prepare("rollback", 101, at.Add(time.Hour))); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		for _, statement := range []string{`DELETE FROM collaboration_events WHERE organization_id=$1`, `UPDATE collaboration_events SET history_hidden=false WHERE organization_id=$1 AND history_hidden`} {
			if _, err := admin.ExecContext(t.Context(), statement, org); err == nil {
				t.Fatalf("immutable event mutation allowed: %s", statement)
			}
		}
		read(t, 2, true, &at, nil)
		created := at.Add(2 * time.Hour)
		session := accessstore.Session{OrganizationID: org, Hash: "metadata_" + string(org), Subject: "example", CreatedAt: created, ExpiresAt: created.Add(time.Hour), IdentityCreatedAt: at, IdentityExpiresAt: created}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessSession(t.Context(), session); err != nil {
				return err
			}
			session.CreatedAt = created.Add(time.Hour)
			if err := w.EnsureAccessSession(t.Context(), session); err != nil {
				return err
			}
			return w.RevokeAccessSession(t.Context(), session.Hash, created.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		read(t, 2, true, &at, &created)
		if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.LatestEventRecordedAt(t.Context()); err != nil {
				return err
			}
			if err := appendRow(prepare("after_snapshot", 102, at.Add(3*time.Hour))); err != nil {
				return err
			}
			count, known, err := r.(billingstore.PopulationReader).BillingPopulation(t.Context(), "retained_collaboration_events", at, 0)
			if err == nil && (count != 2 || !known) {
				return fmt.Errorf("snapshot mixed counter with newer source: %d/%t", count, known)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		latest := at.Add(3 * time.Hour)
		read(t, 3, true, &latest, &created)
		if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_usage_counters SET initialized=false WHERE organization_id=$1 AND name='retained_collaboration_events'`, org); err != nil {
			t.Fatal(err)
		}
		read(t, 0, false, &latest, &created)
		if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_usage_counters SET initialized=true WHERE organization_id=$1 AND name='retained_collaboration_events'`, org); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'retained_collaboration_events',0)`,
			`UPDATE hosted_usage_counters SET value=0 WHERE organization_id=$1 AND name='retained_collaboration_events'`,
			`DELETE FROM hosted_usage_counters WHERE organization_id=$1 AND name='retained_collaboration_events'`,
		} {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
				t.Fatal(err)
			}
			result, denied := tx.Exec(t.Context(), statement, string(org))
			if denied == nil && result.RowsAffected() != 0 {
				t.Errorf("runtime mutated protected count: %s", statement)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		read(t, 3, true, &latest, &created)
		var history int64
		if err := admin.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='history_records'`, org).Scan(&history); err != nil || history != 4 {
			t.Fatalf("aggregate history=%d, error=%v", history, err)
		}
	}
}

func TestBillingEventMetadataUsesTenantTimestampIndexes(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	for _, statement := range []string{
		`INSERT INTO organizations(organization_id,name,local,created_at) SELECT 'org_noise_'||n,'Example',false,'2026-10-01'::timestamptz FROM generate_series(1,9998) n`,
		`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,schema_version,recorded_at,history_hidden,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT organization_id,'prj_shared','wi_shared','plan_'||n,n,1,'2026-10-01'::timestamptz+n*interval '1 second',n%2=0,'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM organizations CROSS JOIN generate_series(1,20002) n WHERE organization_id IN ('org_first','org_second')`,
		`INSERT INTO hosted_sessions(organization_id,token_hash,email,identity_subject,provider_organization_id,session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at)
SELECT organization_id,'plan_'||n,'example@example.test','example','','',now(),now(),'','','2026-10-01'::timestamptz+n*interval '1 second',now(),now() FROM organizations CROSS JOIN generate_series(1,20002) n WHERE organization_id IN ('org_first','org_second')`,
		`ANALYZE collaboration_events,hosted_sessions,hosted_usage_counters`,
	} {
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []string{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
			count, known, err := r.(billingstore.PopulationReader).BillingPopulation(t.Context(), "retained_collaboration_events", historyAt(), 0)
			if err == nil && (count != 20002 || !known) {
				return fmt.Errorf("exact retained population=%d/%t, want20002/true", count, known)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			relation, index, query string
			rows                   int
		}{
			{"collaboration_events", "collaboration_events_tenant_recorded_idx", `SELECT recorded_at FROM collaboration_events WHERE organization_id=$1 ORDER BY recorded_at DESC,id DESC LIMIT 1`, 1},
			{"hosted_sessions", "hosted_sessions_tenant_created_idx", `SELECT created_at FROM hosted_sessions WHERE organization_id=$1 ORDER BY created_at DESC,token_hash DESC LIMIT 1`, 1},
			{"hosted_usage_counters", "hosted_usage_counters_pkey", `SELECT value,initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='retained_collaboration_events'`, 1},
		} {
			t.Run(org+"/"+test.relation, func(t *testing.T) {
				tx, err := admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
						t.Error(err)
					}
				}()
				if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_runtime_v2`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, org); err != nil {
					t.Fatal(err)
				}
				var raw json.RawMessage
				if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+test.query, org).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				t.Logf("billing_event_activity_default_plan tenant=%s relation=%s plan=%s", org, test.relation, raw)
				var plans []struct{ Plan map[string]any }
				if err := json.Unmarshal(raw, &plans); err != nil {
					t.Fatal(err)
				}
				scans := 0
				var visit func(map[string]any)
				visit = func(node map[string]any) {
					if node["Relation Name"] == test.relation {
						scans++
						if !strings.HasPrefix(fmt.Sprint(node["Node Type"]), "Index") || node["Index Name"] != test.index || node["Actual Rows"] != float64(test.rows) || node["Actual Loops"] != float64(1) || !strings.Contains(fmt.Sprint(node["Index Cond"]), org) || node["Rows Removed by Filter"] != nil && node["Rows Removed by Filter"] != float64(0) {
							t.Errorf("unbounded metadata scan: %v", node)
						}
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, child := range children {
							visit(child.(map[string]any))
						}
					}
				}
				visit(plans[0].Plan)
				if scans != 1 {
					t.Fatalf("source scans=%d", scans)
				}
			})
		}
	}
}
