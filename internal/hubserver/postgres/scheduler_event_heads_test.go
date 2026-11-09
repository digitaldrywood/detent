package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSchedulerEventHeadsFollowCommittedSequence(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		foreign, err := content.PrepareEvent(t.Context(), org, historyJSON(t, tracker.CollaborationEvent{ID: "foreign_item", OrganizationID: org, ProjectID: "prj_first", AggregateID: "wi_auxiliary", AggregateSequence: 10, Type: "scheduler.decision", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped"}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: "wi_auxiliary", Number: 3, Title: "Example auxiliary item"}); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), foreign)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, item := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
			t.Run(string(org)+"/"+string(item), func(t *testing.T) {
				prepare := func(id, outcome string, sequence int64) eventstore.Event {
					t.Helper()
					value := tracker.CollaborationEvent{ID: string(item) + "_" + id, OrganizationID: org, ProjectID: "prj_first", AggregateID: item, AggregateSequence: sequence, Type: "scheduler.decision", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: outcome}}}
					row, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
					if err != nil {
						t.Fatal(err)
					}
					return row
				}
				readRows := func() (eventstore.SchedulerDecisionRows, error) {
					var rows eventstore.SchedulerDecisionRows
					err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						rows, err = r.(eventstore.SchedulerDecisionReader).SchedulerDecisions(t.Context(), "prj_first", item)
						return err
					})
					return rows, err
				}
				read := func() (eventstore.SchedulerDecisionRows, error) {
					rows, err := readRows()
					if err == nil {
						for _, row := range []*eventstore.Event{rows.Latest, rows.Exclusion, rows.Unprojected} {
							if row != nil {
								if _, err := content.Event(t.Context(), *row); err != nil {
									return rows, err
								}
							}
						}
					}
					return rows, err
				}
				if rows, err := read(); err != nil || rows.Available {
					t.Fatalf("missing projection=%+v error=%v", rows, err)
				}
				latest := prepare("latest", "ready", 40)
				older := prepare("older", "skipped", 10)
				for _, event := range []eventstore.Event{latest, older} {
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
						t.Fatal(err)
					}
				}
				before, err := read()
				if err != nil || !before.Available || before.Latest == nil || before.Exclusion == nil || before.Unprojected != nil || before.Latest.ID != latest.ID || before.Exclusion.ID != older.ID {
					t.Fatalf("out-of-order heads=%+v error=%v", before, err)
				}
				for _, tc := range []struct {
					name     string
					event    eventstore.Event
					rollback bool
				}{
					{"replay", latest, false},
					{"rollback", prepare("rollback", "skipped", 50), true},
				} {
					t.Run(tc.name, func(t *testing.T) {
						failure := errors.New("example transaction rollback")
						err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							if _, err := w.AppendEvent(t.Context(), tc.event); err != nil {
								return err
							}
							if tc.rollback {
								return failure
							}
							return nil
						})
						if tc.rollback && !errors.Is(err, failure) || !tc.rollback && err != nil {
							t.Fatalf("append error=%v", err)
						}
						after, err := read()
						if err != nil || !reflect.DeepEqual(after, before) {
							t.Fatalf("%s changed committed heads: %+v error=%v", tc.name, after, err)
						}
						if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
							rows, err := r.Events(t.Context(), "prj_first", item, 0, 100)
							if err == nil && len(rows) != 2 {
								t.Fatalf("%s retained %d events, want 2", tc.name, len(rows))
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					})
				}
				for _, tc := range []struct {
					name, query string
					value       any
				}{
					{"sequence beyond latest", "UPDATE scheduler_event_heads SET sequence=$1 WHERE organization_id=$2 AND project_id='prj_first' AND work_item_id=$3 AND head_kind='excluded'", int64(41)},
					{"wrong item object", "UPDATE scheduler_event_heads SET id=$1 WHERE organization_id=$2 AND project_id='prj_first' AND work_item_id=$3 AND head_kind='excluded'", foreign.ID},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if _, err := admin.ExecContext(t.Context(), tc.query, tc.value, org, item); err != nil {
							t.Fatal(err)
						}
						defer func() {
							if _, err := admin.ExecContext(t.Context(), "UPDATE scheduler_event_heads SET id=$1,sequence=10 WHERE organization_id=$2 AND project_id='prj_first' AND work_item_id=$3 AND head_kind='excluded'", older.ID, org, item); err != nil {
								t.Error(err)
							}
						}()
						if _, err := read(); !errors.Is(err, artifact.ErrIntegrity) {
							t.Fatalf("invalid head reference error=%v", err)
						}
					})
				}
				_, err = admin.ExecContext(t.Context(), "UPDATE scheduler_event_heads SET scheduler_outcome='ready' WHERE organization_id=$1 AND project_id='prj_first' AND work_item_id=$2 AND head_kind='excluded'", org, item)
				requireSQLState(t, err, "23514")
				for _, query := range []string{
					"UPDATE scheduler_event_heads SET sequence=sequence+1 WHERE organization_id=$1",
					"DELETE FROM scheduler_event_heads WHERE organization_id=$1",
					"INSERT INTO scheduler_event_heads SELECT * FROM scheduler_event_heads WHERE organization_id=$1",
				} {
					tx, err := admin.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
						t.Fatal(err)
					}
					_, err = tx.ExecContext(t.Context(), query, org)
					requireSQLState(t, err, "42501")
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				}

				newest := prepare("newest", "skipped", 60)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), newest); return err }); err != nil {
					t.Fatal(err)
				}
				before, err = read()
				if err != nil || before.Latest == nil || before.Exclusion == nil || before.Latest.ID != newest.ID || before.Exclusion.ID != newest.ID {
					t.Fatalf("new category heads=%+v error=%v", before, err)
				}
				for _, tc := range []struct{ name, mutation string }{
					{"missing required exclusion", "DELETE FROM scheduler_event_heads WHERE organization_id=$1 AND project_id='prj_first' AND work_item_id=$2 AND head_kind='excluded'"},
					{"conflicting same event metadata", "UPDATE scheduler_event_heads SET actor_principal_id='corrupt' WHERE organization_id=$1 AND project_id='prj_first' AND work_item_id=$2 AND head_kind='excluded'"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if _, err := admin.ExecContext(t.Context(), tc.mutation, org, item); err != nil {
							t.Fatal(err)
						}
						defer func() {
							if _, err := admin.ExecContext(t.Context(), "DELETE FROM scheduler_event_heads WHERE organization_id=$1 AND project_id='prj_first' AND work_item_id=$2 AND head_kind='excluded'", org, item); err != nil {
								t.Error(err)
							}
							if _, err := admin.ExecContext(t.Context(), "INSERT INTO scheduler_event_heads(head_kind, organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome) SELECT 'excluded', organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome FROM scheduler_event_heads WHERE organization_id=$1 AND project_id='prj_first' AND work_item_id=$2 AND head_kind='latest'", org, item); err != nil {
								t.Error(err)
							}
						}()
						if _, err := readRows(); !errors.Is(err, artifact.ErrIntegrity) {
							t.Fatalf("required category error=%v", err)
						}
					})
				}

				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", item) }); err != nil {
					t.Fatal(err)
				}
				after, err := read()
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("work item deletion lost retained heads: %+v error=%v", after, err)
				}
			})
		}
	}
	for _, scope := range []struct {
		org  string
		want int
	}{{"", 0}, {"org_first", 6}, {"org_second", 6}, {"org_unknown", 0}} {
		tx, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", scope.org); err != nil {
			t.Fatal(err)
		}
		var count, largest int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM scheduler_event_heads").Scan(&count); err != nil || count != scope.want {
			t.Fatalf("scope=%s rows=%d want=%d error=%v", scope.org, count, scope.want, err)
		}
		if err := tx.QueryRowContext(t.Context(), "SELECT COALESCE(max(n),0) FROM (SELECT count(*) n FROM scheduler_event_heads GROUP BY organization_id,project_id,work_item_id) groups").Scan(&largest); err != nil || largest > 3 {
			t.Fatalf("scope=%s largest projection=%d error=%v", scope.org, largest, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}

}

func TestSchedulerEventHeadsBackfillUnknownHistory(t *testing.T) {
	const version = 20261108215450
	admin, fixture := migrationDatabaseAtVersion(t, version-1)
	seedCore(t, admin)
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, item := range []tracker.NativeWorkItemID{"wi_shared", "wi_other"} {
			project := tracker.ProjectID("prj_shared")
			if item == "wi_other" {
				project = "prj_other"
			}
			for sequence := int64(1); sequence <= 2; sequence++ {
				value := tracker.CollaborationEvent{ID: fmt.Sprintf("%s_%d", item, sequence), OrganizationID: org, ProjectID: project, AggregateID: item, AggregateSequence: sequence, Type: "scheduler.decision", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped"}}}
				event, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version) VALUES($1,$2,$3,$4,$5,'scheduler.decision',1,$6,$7,$8,$9,$10,$11,$12,2)`, org, project, item, event.ID, sequence, event.ActorKind, event.ActorPrincipalID, event.RecordedAt, event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	admin.SetMaxOpenConns(1)
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "SET detent.organization_id=''"); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"collaboration_events"} {
		var visible int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&visible); err != nil || visible != 0 {
			t.Fatalf("unscoped migration role sees %s=%d error=%v", table, visible, err)
		}
	}
	var owner string
	var definer, rls, forced, scopedRuntime, globalOwner bool
	var settings string
	if err := admin.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(p.proowner),p.prosecdef,array_to_string(p.proconfig,','),c.relrowsecurity,c.relforcerowsecurity,
        EXISTS(SELECT 1 FROM pg_policy q WHERE q.polrelid=c.oid AND q.polname='tenant_isolation' AND q.polroles=ARRAY['detent_runtime_v2'::regrole::oid] AND q.polcmd='r'),
        EXISTS(SELECT 1 FROM pg_policy q WHERE q.polrelid=c.oid AND q.polname='migration_access' AND q.polroles=ARRAY['detent_migration_v2'::regrole::oid] AND pg_get_expr(q.polqual,q.polrelid)='true')
        FROM pg_proc p CROSS JOIN pg_class c WHERE p.oid='capture_scheduler_event_head()'::regprocedure AND c.oid='scheduler_event_heads'::regclass`).Scan(&owner, &definer, &settings, &rls, &forced, &scopedRuntime, &globalOwner); err != nil || owner != "detent_migration_v2" || !definer || settings != "search_path=pg_catalog, pg_temp" || !rls || !forced || !scopedRuntime || !globalOwner {
		t.Fatalf("installed capture owner=%s definer=%v settings=%s RLS=%v/%v policies=%v/%v error=%v", owner, definer, settings, rls, forced, scopedRuntime, globalOwner, err)
	}
	var canRead, canWrite, canExecute bool
	if err := admin.QueryRowContext(t.Context(), "SELECT has_table_privilege('detent_runtime_v2','scheduler_event_heads','SELECT'),has_table_privilege('detent_runtime_v2','scheduler_event_heads','INSERT,UPDATE,DELETE'),has_function_privilege('detent_runtime_v2','capture_scheduler_event_head()','EXECUTE')").Scan(&canRead, &canWrite, &canExecute); err != nil || !canRead || canWrite || canExecute {
		t.Fatalf("projection privileges read=%v write=%v execute=%v error=%v", canRead, canWrite, canExecute, err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	pool := rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_shared", "prj_other"} {
			item := tracker.NativeWorkItemID("wi_" + string(project[4:]))
			var historical eventstore.Event
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.Events(t.Context(), project, item, 0, 10)
				if err == nil && len(rows) != 2 {
					t.Fatalf("historical rows=%d, want2", len(rows))
				}
				if err == nil {
					historical = rows[1]
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			value, err := content.Event(t.Context(), historical)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			changed := value
			decision := *value.Data.Decision
			decision.Reason = "Different source"
			changed.Data.Decision = &decision
			changedPayload, err := content.PrepareEvent(t.Context(), org, historyJSON(t, changed))
			if err != nil {
				t.Fatal(err)
			}
			missingOutcome := prepared
			missingOutcome.ID += "_new"
			missingOutcome.SchedulerOutcome = ""
			for _, test := range []struct {
				name string
				row  eventstore.Event
				want error
			}{
				{"old typed replay", historical, nil},
				{"prepared replay", prepared, nil},
				{"different immutable source", changedPayload, corestore.ErrRevisionConflict},
				{"new missing outcome", missingOutcome, eventstore.ErrInvalidRecord},
			} {
				t.Run(string(org)+"/"+string(project)+"/"+test.name, func(t *testing.T) {
					var replayed eventstore.Event
					err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						var err error
						replayed, err = w.AppendEvent(t.Context(), test.row)
						return err
					})
					if !errors.Is(err, test.want) {
						t.Fatalf("replay error=%v, want%v; requested=%+v historical=%+v", err, test.want, test.row, historical)
					}
					if err == nil && !reflect.DeepEqual(replayed, historical) {
						t.Fatalf("replay changed historical identity or projection: got=%+v want=%+v", replayed, historical)
					}
				})
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.(eventstore.SchedulerDecisionReader).SchedulerDecisions(t.Context(), project, item)
				if err != nil || !rows.Available || rows.Latest == nil || rows.Latest.Sequence != 2 || rows.Unprojected == nil || rows.Unprojected.ID != rows.Latest.ID || rows.Exclusion != nil {
					t.Fatalf("tenant=%s item=%s legacy heads=%+v error=%v", org, item, rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "DELETE FROM scheduler_event_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND head_kind='unprojected'", org, project, item); err != nil {
				t.Fatal(err)
			}
			err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(eventstore.SchedulerDecisionReader).SchedulerDecisions(t.Context(), project, item)
				return err
			})
			if !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("missing required unknown head error=%v", err)
			}

		}
	}
}
