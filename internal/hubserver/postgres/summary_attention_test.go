package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSummaryAttentionIgnoresHistoricalPopulation(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Human Review"}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues (organization_id,project_id,native_id,number,title,workflow_state_name,actor_principal_id,created_at,native_created_at,native_updated_at) SELECT $1,'project',$1||'_old_'||n,n,'Old completed issue','Done','other',$2,$2,$2 FROM generate_series(1,5001) n`, org, now.Add(-365*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			state, actor := "Human Review", "recipient"
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project", NativeID: tracker.NativeWorkItemID(string(org) + "_eligible"), Number: 5002, Title: "Needs review", WorkflowStateName: &state, ActorPrincipalID: &actor, CreatedAt: now.Add(-time.Hour), NativeCreatedAt: now.Add(-time.Hour), NativeUpdatedAt: now.Add(-time.Hour)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			query := reportstore.SummaryAttentionQuery{Project: "project", Recipient: "recipient", From: now.Add(-24 * time.Hour), To: now, Limit: 5000}
			owner, ok := r.(reportstore.SummaryAttentionReader)
			if !ok {
				_, err := r.(reportstore.HistoryReader).ReadHistory(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{"project"}, From: time.Unix(0, 0), To: now, Limit: 5000})
				return fmt.Errorf("legacy attention population: %w", err)
			}
			for _, principal := range []string{"", "recipient", "other"} {
				query.Principal = principal
				rows, err := owner.ReadSummaryAttention(t.Context(), query)
				if err != nil {
					return err
				}
				if principal == "other" {
					if len(rows) != 0 {
						t.Fatalf("foreign actor rows=%v", rows)
					}
					continue
				}
				if len(rows) != 1 || string(rows[0].WorkItemID) != string(org)+"_eligible" {
					t.Fatalf("candidates=%v", rows)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='recipient' WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_board_items
(organization_id,project_id,work_item_id,source_revision,source_event_sequence,source_project_revision,state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,identifier,execution_fence,actor_principal_id,issue_type,verification_state,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT organization_id,project_id,native_id,revision,event_sequence,1,'Done',false,true,false,false,0,0,true,native_id,0,'recipient','task','verified',created_at,updated_at,'orgs/'||organization_id||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1 AND workflow_state_name='Done'`, org); err != nil {
			t.Fatal(err)
		}
		for _, stale := range []bool{false, true} {
			if stale {
				if _, err := admin.ExecContext(t.Context(), `UPDATE native_board_items SET source_revision=0 WHERE organization_id=$1 AND work_item_id=$1||'_old_1'`, org); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(reportstore.SummaryAttentionReader)
				query := reportstore.SummaryAttentionQuery{Project: "project", Recipient: "recipient", From: now.Add(-24 * time.Hour), To: now, Limit: 5000}
				rows, err := owner.ReadSummaryAttention(t.Context(), query)
				if err != nil {
					return err
				}
				want := 1
				if stale {
					want = 2
				}
				if len(rows) != want {
					t.Fatalf("stale=%v candidates=%v", stale, rows)
				}
				if stale {
					query.Limit = 1
					if _, err := owner.ReadSummaryAttention(t.Context(), query); !errors.Is(err, reportstore.ErrPopulationExceeded) {
						t.Fatalf("bounded candidates error=%v", err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
