package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkspaceClaimAssociationIsTenantScopedAndPermanent(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 20, 0, 0, 0, time.UTC)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.Write(t.Context(), organization, corestore.Serving, func(w corestore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			project, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "prj_shared", Name: "Example", Profile: "native"})
			if err != nil {
				return err
			}
			_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project.ID, NativeID: "wi_shared", Number: 1, Title: "Workspace item"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		item := claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: "prj_shared", WorkItemID: "wi_shared", WorkspaceID: "ws_" + string(organization), CreatedAt: now}
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.PutWorkspaceItem(t.Context(), item)
		}); err != nil {
			t.Fatalf("put workspace association for %s: %v", organization, err)
		}
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.PutWorkspaceItem(t.Context(), item)
		}); err != nil {
			t.Fatalf("replay workspace association for %s: %v", organization, err)
		}
		var found bool
		if err := tenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			found, err = r.IsWorkspaceWorkItem(t.Context(), item.ProjectID, item.WorkItemID)
			return err
		}); err != nil || !found {
			t.Fatalf("workspace association for %s found=%t error=%v", organization, found, err)
		}
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		found, err := r.IsWorkspaceWorkItem(t.Context(), "prj_shared", "wi_other")
		if err != nil {
			return err
		}
		if found {
			t.Fatal("unassociated work item is classified as a workspace")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closedAt := now.Add(time.Minute)
	if err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CloseWorkspaceItem(t.Context(), "prj_shared", "wi_shared", closedAt)
	}); err != nil {
		t.Fatal(err)
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		found, err := r.IsWorkspaceWorkItem(t.Context(), "prj_shared", "wi_shared")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("closed workspace association became ordinary project work")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conflict := claimstore.WorkspaceItem{OrganizationID: "org_first", ProjectID: "prj_shared", WorkItemID: "wi_shared", WorkspaceID: "ws_reassigned", CreatedAt: now}
	err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.PutWorkspaceItem(t.Context(), conflict) })
	if !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("reassigned workspace association error=%v, want unique violation", err)
	}
}

func TestClaimCandidatePageUsesTenantScopedTypedFactsAndBoundedCursor(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 21, 0, 0, 0, time.UTC)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
				Core:   corestore.Project{OrganizationID: organization, ID: "prj_claim", Name: "Claim example", Profile: "native", RequireDependencies: true},
				States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}},
			})
			return err
		}); err != nil {
			t.Fatalf("seed claim project for %s: %v", organization, err)
		}
	}
	invalidPriority := int64(9)
	invalidItem := corestore.WorkItem{OrganizationID: "org_first", ProjectID: "prj_claim", NativeID: "wi_rollback", Number: 4, Title: "Rollback example", WorkflowStateName: new("Todo"), CreatedAt: now, NativeCreatedAt: now}
	invalidFacts := claimstore.WorkItemFacts{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: "wi_rollback", Author: "author", Repository: "example/repository"}
	invalidQueue := claimstore.QueueEntry{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: "wi_rollback", Scope: "prj_claim", State: "Todo", Rank: "wi_rollback", Priority: &invalidPriority, CreatedAt: now, UpdatedAt: now}
	err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItemWithClaimFacts(t.Context(), invalidItem, nil, invalidFacts, invalidQueue)
		return err
	})
	if !errors.Is(err, claimstore.ErrInvalidCandidateFact) {
		t.Fatalf("invalid imported claim facts error=%v", err)
	}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.WorkItem(t.Context(), "prj_claim", "wi_rollback")
		if !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("failed import left a core item: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for i, id := range []tracker.NativeWorkItemID{"wi_first", "wi_second", "wi_workspace"} {
				state := "Todo"
				createdAt := now.Add(time.Duration(i) * time.Minute)
				assignees := []string{"worker_a"}
				if organization == "org_second" {
					assignees = []string{"worker_b"}
				}
				priority := int64(i)
				item := corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_claim", NativeID: id, Number: int64(i + 1), Title: string(id), WorkflowStateName: &state, CreatedAt: createdAt, NativeCreatedAt: createdAt}
				facts := claimstore.WorkItemFacts{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: id, Author: "AUTHOR", Repository: "EXAMPLE/REPOSITORY", Assignees: assignees, DispatchWaitSince: &createdAt}
				queue := claimstore.QueueEntry{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: id, Scope: "prj_claim", State: "Todo", Rank: string(id), Priority: &priority, CreatedAt: createdAt, UpdatedAt: createdAt}
				var labels []string
				if id != "wi_workspace" {
					labels = []string{" Ready "}
				}
				if _, err := w.CreateWorkItemWithClaimFacts(t.Context(), item, labels, facts, queue); err != nil {
					return err
				}
			}
			return w.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: organization, ProjectID: "prj_claim", WorkItemID: "wi_workspace", WorkspaceID: "ws_claim_" + string(organization), CreatedAt: now})
		}); err != nil {
			t.Fatalf("seed claim facts for %s: %v", organization, err)
		}
	}
	query := claimstore.CandidateQuery{ProjectID: "prj_claim", Scope: "prj_claim", Assignees: []string{"WORKER_A"}, IncludeLabels: []string{" ReAdY "}, LabelPriority: []string{"absent", " READY "}, AvailableAt: now, Limit: 1}
	var first claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		first, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].WorkItemID != "wi_first" || first.Items[0].Priority != 1 || first.Items[0].Author != "author" || first.Items[0].Repository != "example/repository" || len(first.Items[0].Assignees) != 1 || len(first.Items[0].Labels) != 1 || first.Items[0].Labels[0] != "Ready" || first.Items[0].LabelRank != 2 {
		t.Fatalf("first candidate page = %+v", first)
	}
	for _, test := range []struct {
		name               string
		included, excluded []string
		want               int
	}{
		{"normalized inclusion", []string{" READY "}, []string{"absent"}, 1},
		{"normalized exclusion", nil, []string{" ReAdY "}, 0},
		{"all included labels required", []string{"ready", "missing"}, nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			filtered := query
			filtered.IncludeLabels, filtered.ExcludeLabels = test.included, test.excluded
			if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.CandidatePage(t.Context(), filtered)
				if err == nil && (len(page.Items) != test.want || len(page.Items) > 0 && page.Items[0].WorkItemID != "wi_first") {
					t.Fatalf("normalized label candidate page=%+v want=%d", page, test.want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	requested := query
	requested.WorkItemIDs = []tracker.NativeWorkItemID{"wi_second", "wi_missing"}
	var selected claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		selected, err = r.CandidatePage(t.Context(), requested)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(selected.Items) != 1 || selected.Items[0].WorkItemID != "wi_second" {
		t.Fatalf("provider-filtered candidate page = %+v", selected)
	}
	query.After = first.Next
	var second claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		second, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].WorkItemID != "wi_second" {
		t.Fatalf("second candidate page = %+v", second)
	}
	query.After = claimstore.CandidateCursor{}
	query.Assignees = []string{"worker_b"}
	var otherTenant claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		otherTenant, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(otherTenant.Items) != 1 || otherTenant.Items[0].WorkItemID != "wi_first" {
		t.Fatalf("second tenant candidate page = %+v", otherTenant)
	}
	query.Assignees = []string{"worker_a"}
	query.IncludeLabels = nil
	query.Limit = 1
	query.WorkspaceLane = true
	var workspaceLane claimstore.CandidatePage
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		workspaceLane, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sawWorkspace bool
	for _, item := range workspaceLane.Items {
		if item.WorkItemID == "wi_workspace" {
			sawWorkspace = true
		}
	}
	if !sawWorkspace {
		t.Fatalf("workspace lane omitted associated item: %+v", workspaceLane)
	}
	if len(workspaceLane.Items) != 1 || workspaceLane.Items[0].WorkItemID != "wi_workspace" {
		t.Fatalf("ordinary work crowded the workspace page: %+v", workspaceLane)
	}
	query.Assignees = []string{"worker_b"}
	if err := tenantRepository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.CandidatePage(t.Context(), query)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].WorkItemID != "wi_workspace" {
			t.Fatalf("second tenant workspace page=%+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CloseWorkspaceItem(t.Context(), "prj_claim", "wi_workspace", now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	query.Assignees = []string{"worker_a"}
	if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.CandidatePage(t.Context(), query)
		if err != nil {
			return err
		}
		if len(page.Items) != 0 {
			t.Fatalf("closed workspace remained selectable: %+v", page)
		}
		query.WorkspaceLane, query.Limit = false, 3
		page, err = r.CandidatePage(t.Context(), query)
		if err != nil {
			return err
		}
		if len(page.Items) != 2 || page.Items[0].WorkItemID != "wi_first" || page.Items[1].WorkItemID != "wi_second" {
			t.Fatalf("ordinary lane changed after workspace closed: %+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Run("queue sort projection preserves Unicode whitespace and raw rank", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for index, test := range []struct {
				raw, normalized string
				missing         int
			}{
				{" \t\n\v\f\ra \t\n\v\f\r", "a", 0},
				{"\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000a\u3000\u205f\u202f\u2029\u2028\u200a\u2009\u2008\u2007\u2006\u2005\u2004\u2003\u2002\u2001\u2000\u1680\u00a0\u0085", "a", 0},
				{" \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000", "", 1},
				{"\u200ba\u200b", "\u200ba\u200b", 0},
				{"\ufeffa\ufeff", "\ufeffa\ufeff", 0},
				{"\u180ea\u180e", "\u180ea\u180e", 0},
				{"\u00a0a\tb\u00a0", "a\tb", 0},
			} {
				if strings.TrimSpace(test.raw) != test.normalized {
					t.Fatalf("literal rank expectation differs from Go whitespace contract at case %d", index)
				}
				scope := fmt.Sprintf("queue-key-%d", index)
				entry := claimstore.QueueEntry{OrganizationID: org, ProjectID: "prj_claim", WorkItemID: "wi_first", Scope: scope, State: "Todo", Rank: test.raw, CreatedAt: now, UpdatedAt: now}
				if err := tenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.PutQueueEntry(t.Context(), entry) }); err != nil {
					t.Fatal(err)
				}
				for pass := range 2 {
					var raw, normalized string
					var priority, missing int
					if err := admin.QueryRowContext(t.Context(), "SELECT rank,list_priority,list_rank_missing,list_rank FROM queue_entries WHERE organization_id=$1 AND project_id=$2 AND scope=$3 AND work_item_id=$4", org, "prj_claim", scope, "wi_first").Scan(&raw, &priority, &missing, &normalized); err != nil {
						t.Fatal(err)
					}
					if raw != entry.Rank || normalized != test.normalized || missing != test.missing || priority != []int{4, 0}[pass] {
						t.Fatalf("queue generated projection org=%s case=%d pass=%d raw=%q normalized=%q missing=%d priority=%d", org, index, pass, raw, normalized, missing, priority)
					}
					if pass == 0 {
						entry.Rank = "\t" + entry.Rank + "\n"
						if _, err := admin.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override=0,rank=$5 WHERE organization_id=$1 AND project_id=$2 AND scope=$3 AND work_item_id=$4", org, "prj_claim", scope, "wi_first", entry.Rank); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	})
	t.Run("queue list bounds source before private residuals", func(t *testing.T) {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		config := pool.Config()
		trace := &queueListQueryTrace{}
		config.ConnConfig.Tracer = trace
		tracedPool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tracedPool.Close)
		reader, err := postgres.NewRepository(t.Context(), tracedPool, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if _, err := admin.ExecContext(t.Context(), `WITH added AS (
INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,native_created_at,native_updated_at,actor_kind,actor_principal_id)
SELECT $1,'prj_claim','wi_seek_'||n,n+10,'Hidden queue source','Todo',$2,$2,'human','hidden' FROM generate_series(1,1000)n RETURNING organization_id,project_id,native_id,number
),facts AS (
INSERT INTO issue_claim_facts(organization_id,project_id,work_item_id,author_login,repository,assignees) SELECT organization_id,project_id,native_id,'hidden','example/source',ARRAY['hidden']::text[] FROM added RETURNING work_item_id)
INSERT INTO queue_entries(organization_id,project_id,work_item_id,scope,state,rank,priority_override,created_at,updated_at)
SELECT organization_id,project_id,native_id,'queue-list','Todo',$1||lpad(number::text,6,'0'),0,$2,$2 FROM added`, org, now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), "ANALYZE issues; ANALYZE queue_entries; ANALYZE issue_claim_facts"); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, test := range []struct {
				identifier    bool
				first, second tracker.NativeWorkItemID
			}{{false, "wi_seek_1", "wi_seek_22"}, {true, "wi_first", "wi_seek_19"}} {
				query := claimstore.QueueListQuery{ProjectID: "prj_claim", Scope: "queue-list", IdentifierOrder: test.identifier, Principal: "visible", Labels: []string{"absent"}, Assignees: []string{"absent"}, Limit: 21}
				for pageIndex := range 2 {
					var rows []claimstore.QueueListRow
					if err := reader.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						rows, err = r.QueueListPage(t.Context(), query)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					want := test.first
					if pageIndex == 1 {
						want = test.second
					}
					if len(rows) != 21 || rows[0].Entry.WorkItemID != want {
						t.Fatalf("queue source tenant=%s page=%d rows=%+v", org, pageIndex, rows)
					}
					for _, row := range rows {
						if row.Matches || row.Entry.OrganizationID != org {
							t.Fatalf("private/foreign queue row served: %+v", row)
						}
						if row.Entry.Scope != "" && !strings.HasPrefix(row.Entry.Rank, string(org)) {
							t.Fatalf("cross-tenant queue rank=%s", row.Entry.Rank)
						}
					}
					query.After = rows[len(rows)-1].Position
				}
			}
		}
		trace.mu.Lock()
		samples := append([]queueListQuerySample(nil), trace.samples...)
		trace.mu.Unlock()
		if len(samples) != 8 {
			t.Fatalf("captured actual queue requests=%d", len(samples))
		}
		for _, sample := range samples {
			if err := withPlanSeedTransaction(t.Context(), admin, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
					return err
				}
				if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", sample.args[0]); err != nil {
					return err
				}
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sample.sql, sample.args...).Scan(&raw); err != nil {
					return err
				}
				var plans []struct{ Plan queueListPlan }
				if err := json.Unmarshal(raw, &plans); err != nil {
					return err
				}
				if len(plans) != 1 {
					return fmt.Errorf("queue plan count=%d", len(plans))
				}
				visits := map[string]float64{}
				keyed := false
				identifier := strings.Contains(sample.sql, "SELECT i.* FROM issues i")
				continuation := strings.Contains(sample.sql, "AND i.number>") || strings.Contains(sample.sql, "q.created_at,q.work_item_id)>")
				var inspect func(queueListPlan, string)
				inspect = func(node queueListPlan, relation string) {
					if node.Relation != "" {
						relation = node.Relation
						visits[relation] += (node.Rows + node.Removed + node.Rechecked) * node.Loops
					}
					source := identifier && relation == "issues" || !identifier && relation == "queue_entries" && strings.Contains(node.Condition, "scope")
					if source && node.Loops == 1 && strings.Contains(node.Condition, "organization_id") && strings.Contains(node.Condition, "project_id") && (!continuation || strings.Contains(node.Condition, ">")) {
						keyed = true
					}
					for _, child := range node.Plans {
						inspect(child, relation)
					}
				}
				inspect(plans[0].Plan, "")
				if !keyed || visits["queue_entries"] > 21 || visits["issues"] > 21 || visits["issue_claim_facts"] > 21 {
					return fmt.Errorf("queue seek/residual bound visits=%v keyed=%t plan=%s", visits, keyed, raw)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})

}

func TestProviderReservationSnapshotIsTypedAndTenantScoped(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 22, 0, 0, 0, time.UTC)
	percent := 34.5
	report := providercapacity.Report{
		Provider: "example-provider", Backend: "example-backend", AccountAlias: "example-account", SharedAccountAlias: "shared-example",
		Models: []string{"example-model"}, ModelDetails: []providercapacity.ModelDetail{{ID: "example-model", Label: "Example", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low"}},
		UsageWindows:  []providercapacity.UsageWindow{{Kind: "weekly", PercentUsed: &percent, ObservedAt: now, ResetAt: now.Add(time.Hour)}},
		MaxConcurrent: 3, Availability: "available", ObservedAt: now,
	}
	reservation := providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "example-backend", Model: "example-model"}, Report: report, Reason: "Current account capacity is available"}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := tenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(t.Context(), w, organization); err != nil {
				return err
			}
			project, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "prj_provider", Name: "Example", Profile: "native"})
			if err != nil {
				return err
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project.ID, NativeID: "wi_provider", Number: 1, Title: "Example", CreatedAt: now, NativeCreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_provider", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_provider", SessionID: "session_provider", LeaseID: "lease_shared", TTL: time.Minute})
			if err != nil {
				return err
			}
			return w.CreateProviderReservation(t.Context(), claimstore.ProviderReservation{OrganizationID: organization, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: reservation})
		}); err != nil {
			t.Fatalf("seed provider reservation for %s: %v", organization, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		var got claimstore.ProviderReservation
		if err := tenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			got, err = r.ProviderReservation(t.Context(), "lease_shared")
			return err
		}); err != nil {
			t.Fatalf("read provider reservation for %s: %v", organization, err)
		}
		if got.OrganizationID != organization || got.LeaseID != "lease_shared" || got.Reservation.Requirement != reservation.Requirement || got.Reservation.Report.Provider != report.Provider || got.Reservation.Report.SharedAccountAlias != report.SharedAccountAlias || len(got.Reservation.Report.ModelDetails) != 1 || len(got.Reservation.Report.ModelDetails[0].ReasoningEfforts) != 2 || len(got.Reservation.Report.UsageWindows) != 1 || got.Reservation.Report.UsageWindows[0].PercentUsed == nil || *got.Reservation.Report.UsageWindows[0].PercentUsed != percent || got.Reservation.Reason != reservation.Reason {
			t.Fatalf("provider reservation snapshot for %s = %+v", organization, got)
		}
	}
	err := tenantRepository.ReadTenant(t.Context(), "org_missing", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ProviderReservation(t.Context(), "lease_shared")
		return err
	})
	if !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("cross-tenant provider reservation read error=%v", err)
	}
}

type queueListQuerySample struct {
	sql  string
	args []any
}
type queueListQueryTrace struct {
	mu      sync.Mutex
	samples []queueListQuerySample
}

func (trace *queueListQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "WITH raw AS MATERIALIZED (") {
		trace.mu.Lock()
		trace.samples = append(trace.samples, queueListQuerySample{sql: data.SQL, args: append([]any(nil), data.Args...)})
		trace.mu.Unlock()
	}
	return ctx
}
func (*queueListQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type queueListPlan struct {
	Relation  string  `json:"Relation Name"`
	Condition string  `json:"Index Cond"`
	Rows      float64 `json:"Actual Rows"`
	Removed   float64 `json:"Rows Removed by Filter"`
	Rechecked float64 `json:"Rows Removed by Index Recheck"`
	Loops     float64 `json:"Actual Loops"`
	Plans     []queueListPlan
}

func TestClaimCandidatePageSkipsUnclassifiedSourceCheckpoint(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	tenantRepository := tenantstore.Repository(repository)
	now := time.Date(2026, time.November, 1, 21, 0, 0, 0, time.UTC)
	items := []tracker.NativeWorkItemID{"wi_unknown", "wi_clean", "wi_dirty"}
	if err := tenantRepository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := pgstoretest.SeedCoreOrganization(t.Context(), w, "org_first"); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:   corestore.Project{OrganizationID: "org_first", ID: "prj_claim", Name: "Claim example", Profile: "native"},
			States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}},
		}); err != nil {
			return err
		}
		for i, id := range items {
			state := "Todo"
			item := corestore.WorkItem{OrganizationID: "org_first", ProjectID: "prj_claim", NativeID: id, Number: int64(i + 1), Title: string(id), WorkflowStateName: &state, CreatedAt: now, NativeCreatedAt: now}
			facts := claimstore.WorkItemFacts{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: id, Author: "author", Repository: "example/repository"}
			queue := claimstore.QueueEntry{OrganizationID: "org_first", ProjectID: "prj_claim", WorkItemID: id, Scope: "prj_claim", State: "Todo", Rank: string(id), CreatedAt: now, UpdatedAt: now}
			if _, err := w.CreateWorkItemWithClaimFacts(t.Context(), item, nil, facts, queue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('org_first','machine_claim','example',1,'example',$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		item      tracker.NativeWorkItemID
		attempts  int
		preserves sql.NullBool
	}{{"wi_unknown", 3, sql.NullBool{}}, {"wi_clean", 1, sql.NullBool{Valid: true}}, {"wi_dirty", 1, sql.NullBool{Valid: true, Bool: true}}} {
		for n := range seed.attempts {
			id := fmt.Sprintf("%s_%d", seed.item, n)
			for _, statement := range []string{
				`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT organization_id,$2,id,'machine_claim',$2,1,$3,$3,$3::timestamptz+interval '1 hour',$3 FROM issues WHERE organization_id='org_first' AND native_id=$1`,
				`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT organization_id,$2,id FROM issues WHERE organization_id='org_first' AND native_id=$1`,
				`INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,checkpoint_preserves_source,started_at,updated_at) SELECT organization_id,'prj_claim',lease_id,issue_id,lease_id,fencing_token,lease_id,1,'interrupted',1,0,'org_first/payload','org_first/checkpoint',$4::boolean,$3::timestamptz,$3::timestamptz FROM leases WHERE organization_id='org_first' AND lease_id=$2 AND issue_id=(SELECT id FROM issues WHERE organization_id='org_first' AND native_id=$1)`,
			} {
				args := []any{seed.item, id}
				if strings.Contains(statement, "$3") {
					args = append(args, now)
				}
				if strings.Contains(statement, "$4") {
					args = append(args, seed.preserves)
				}
				if _, err := admin.ExecContext(t.Context(), statement, args...); err != nil {
					t.Fatalf("seed %s: %v", id, err)
				}
			}
		}
	}
	page := func() []tracker.NativeWorkItemID {
		t.Helper()
		var ids []tracker.NativeWorkItemID
		if err := tenantRepository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: "prj_claim", Scope: "prj_claim", AvailableAt: now, Limit: 10})
			for _, item := range page.Items {
				ids = append(ids, item.WorkItemID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := page(); len(got) != 2 || slices.Contains(got, "wi_unknown") {
		t.Fatalf("unclassified source candidates=%v, want clean and dirty only", got)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_preserves_source=true WHERE organization_id='org_first' AND id='wi_unknown_2'`); err != nil {
		t.Fatal(err)
	}
	if got := page(); len(got) != 3 {
		t.Fatalf("classified source candidates=%v, want all three", got)
	}
}
