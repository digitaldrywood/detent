package searchrecords_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthFindingsTenantVisibilityAndCursor(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	at := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		for _, id := range []string{"first", "second"} {
			_, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) VALUES($1,$2,$2,'signal','class','work_item','wi_first_1',$3,$3,'attention',$1,'inspect','','',0,0)`, org, id, at)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) VALUES($1,$2,'prj_first')`, org, id)
			if err != nil {
				t.Fatal(err)
			}
			_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_issues(organization_id,finding_id,project_id,work_item_id) VALUES($1,$2,'prj_first','wi_first_1')`, org, id)
			if err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				owner := w.(healthstore.PageMaintainer)
				candidate, err := owner.NextHealthPage(t.Context())
				if err != nil {
					return err
				}
				_, err = owner.PublishHealthPage(t.Context(), candidate, healthstore.Evidence{})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(healthstore.Reader)
			if !ok {
				t.Fatal("PostgreSQL health findings owner is missing")
			}
			page, err := owner.HealthFindings(t.Context(), "prj_first", "open", &at, 0, "example", 1)
			if err != nil {
				return err
			}
			if len(page.Findings) != 1 || page.Findings[0].Summary != string(org) || len(page.Findings[0].Issues) != 1 {
				t.Fatalf("first page = %+v", page)
			}
			next, err := owner.HealthFindings(t.Context(), "prj_first", "open", &at, page.Findings[0].Sequence, "example", 2)
			if err != nil {
				return err
			}
			if len(next.Findings) != 1 || next.Findings[0].ID == page.Findings[0].ID {
				t.Fatalf("next page = %+v", next)
			}
			for _, tc := range []struct {
				project  tracker.ProjectID
				reporter string
				since    time.Time
			}{{"prj_second", "example", at}, {"prj_first", "other", at}, {"prj_first", "example", at.Add(time.Microsecond)}} {
				hidden, err := owner.HealthFindings(t.Context(), tc.project, "open", &tc.since, 0, tc.reporter, 2)
				if err != nil {
					return err
				}
				if len(hidden.Findings) != 0 {
					t.Fatalf("hidden findings = %+v", hidden)
				}
			}
			summary, err := owner.HealthSummaryFindings(t.Context(), 100)
			if err != nil {
				return err
			}
			if len(summary) != 2 || len(summary[0].Projects) != 1 {
				t.Fatalf("summary = %+v", summary)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
			return writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "health_owner", Name: "Example owner", Hash: "health_hash_" + string(org), Fingerprint: "health_fingerprint_" + string(org), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: at, UpdatedAt: at})
		}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"conversation_first", "conversation_second"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,archived,created_at,updated_at) VALUES($1,$2,'prj_first','health_owner','user','private','active',$4,$3,$3)`, org, id, at, id == "conversation_second"); err != nil {
				t.Fatal(err)
			}
			for index, status := range []string{"pending", "sending", "sent", "unknown", "answered", "expired"} {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_questions(organization_id,conversation_id,id,status,created_at,updated_at,expires_at) VALUES($1,$2,$3,$4,$5,$5,$6)`, org, id, fmt.Sprintf("question_%d", index), status, at.Add(time.Duration(index)*time.Microsecond), at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT $1,p.id,s.name,s.terminal,NOT s.terminal,s.rank FROM projects p CROSS JOIN (VALUES('Todo',false,0),('Done',true,1)) s(name,terminal,rank) WHERE p.organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Todo' WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,workflow_state_name,archived) SELECT $1,'history_'||n,'prj_first',100+n,'Example history',CASE WHEN n%2=0 THEN 'Todo' ELSE 'Done' END,n%2=0 FROM generate_series(1,20001) n`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_questions(organization_id,conversation_id,id,status,created_at,updated_at) SELECT $1,'conversation_first','history_'||n,CASE WHEN n%2=0 THEN 'answered' ELSE 'expired' END,$2,$2 FROM generate_series(1,20001) n`, org, at.Add(-365*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(reader tenantstore.Reader) error {
			owner := reader.(healthstore.SourceReader)
			var references []corestore.WorkItemReference
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				var after int64
				for range 3 {
					page, err := owner.HealthSourceItems(t.Context(), project, "Todo", after, 1)
					if err != nil {
						return err
					}
					if len(page) == 0 {
						break
					}
					if len(page) != 1 || page[0].ID <= after {
						t.Fatalf("active item source seek=%+v after=%d", page, after)
					}
					references = append(references, page[0].WorkItemReference)
					after = page[0].ID
				}
				terminal, err := owner.HealthSourceItems(t.Context(), project, "Done", 0, 1)
				if err != nil {
					return err
				}
				if len(terminal) != 0 {
					t.Fatalf("terminal history reached health wait owner: %+v", terminal)
				}
			}
			want := []corestore.WorkItemReference{{ProjectID: "prj_first", WorkItemID: "wi_first_1"}, {ProjectID: "prj_first", WorkItemID: "wi_first_2"}, {ProjectID: "prj_second", WorkItemID: "wi_second_1"}, {ProjectID: "prj_second", WorkItemID: "wi_second_2"}}
			if !reflect.DeepEqual(references, want) {
				t.Fatalf("item source tenant/tail=%+v", references)
			}
			conversation, question := "", ""
			var seen []string
			for range 13 {
				page, err := owner.HealthSourceQuestions(t.Context(), conversation, question, 1)
				if err != nil {
					return err
				}
				if len(page) == 0 {
					break
				}
				row := page[0]
				if len(page) != 1 || row.ProjectID != "prj_first" || row.ExpiresAt == nil || !row.ExpiresAt.Equal(at.Add(time.Hour)) || row.ConversationStatus != "active" || row.ConversationDeleted || row.ConversationArchived != (row.ConversationID == "conversation_second") {
					t.Fatalf("question source scope/metadata=%+v", page)
				}
				seen = append(seen, row.Status)
				conversation, question = row.ConversationID, row.ID
			}
			wantStatus := []string{"pending", "sending", "sent", "unknown", "pending", "sending", "sent", "unknown"}
			if !reflect.DeepEqual(seen, wantStatus) {
				t.Fatalf("unresolved question tail=%v", seen)
			}
			current, err := owner.HealthSourceConversationQuestions(t.Context(), "conversation_first", "", 100)
			if err != nil {
				return err
			}
			if len(current) != 4 || current[3].Status != "unknown" || current[3].ConversationArchived {
				t.Fatalf("selected active conversation questions=%+v", current)
			}

			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, analyzed := range []bool{false, true} {
			if analyzed {
				if _, err := admin.ExecContext(t.Context(), "ANALYZE issues; ANALYZE conversation_questions; ANALYZE project_workflow_states"); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []string{"items", "items_tail", "questions", "conversation"} {
				probe := &pgstoretest.SearchPlanProbe{}
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				probe.DBTX = tx
				queries := pgquery.New(probe)
				relation, index := "conversation_questions", "health_questions_unresolved_seek"
				selected := 0
				switch kind {
				case "items", "items_tail":
					var after int64
					if kind == "items_tail" {
						if err := admin.QueryRowContext(t.Context(), `SELECT max(id) FROM issues WHERE organization_id=$1 AND project_id='prj_first' AND NOT archived AND workflow_state_name='Todo'`, org).Scan(&after); err != nil {
							tx.Rollback(t.Context())
							t.Fatal(err)
						}
					}
					rows, readErr := queries.HealthSourceItems(t.Context(), pgquery.HealthSourceItemsParams{OrganizationID: org, ProjectID: "prj_first", StateName: new("Todo"), AfterID: after, Limit: 1})
					err = readErr
					selected = len(rows)
					relation, index = "issues", "issues_billing_current_population_idx"
				case "questions":
					rows, readErr := queries.HealthSourceQuestions(t.Context(), pgquery.HealthSourceQuestionsParams{OrganizationID: org, Limit: 1})
					err = readErr
					selected = len(rows)
				case "conversation":
					rows, readErr := queries.HealthSourceConversationQuestions(t.Context(), pgquery.HealthSourceConversationQuestionsParams{OrganizationID: org, ConversationID: "conversation_first", Limit: 1})
					err = readErr
					selected = len(rows)
				}
				if err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				expected := 1
				if kind == "items_tail" {
					expected = 0
				}
				if selected != expected || probe.Statement == "" || len(probe.Args) == 0 {
					tx.Rollback(t.Context())
					t.Fatalf("health plan capture empty: kind=%s rows=%d", kind, selected)
				}
				var raw []byte
				err = tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+probe.Statement, probe.Args...).Scan(&raw)
				rollbackErr := tx.Rollback(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if rollbackErr != nil {
					t.Fatal(rollbackErr)
				}
				var plan []struct{ Plan pgstoretest.SearchPlanNode }
				if err := json.Unmarshal(raw, &plan); err != nil {
					t.Fatal(err)
				}
				var visited func(pgstoretest.SearchPlanNode) float64
				visited = func(node pgstoretest.SearchPlanNode) float64 {
					var work float64
					if node.Relation == relation {
						work = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
					}
					for _, child := range node.Plans {
						work += visited(child)
					}
					return work
				}
				if len(plan) != 1 || !plan[0].Plan.UsesIndex(index) || visited(plan[0].Plan) > 4 {
					t.Fatalf("health active seek scanned history: kind=%s analyzed=%t plan=%s", kind, analyzed, raw)
				}
			}
		}

	}
}
