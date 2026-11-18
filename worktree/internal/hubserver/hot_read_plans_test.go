package hubserver

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type tracedStatement struct {
	query string
	args  []any
}

func TestHotReadQueryPlans(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	admin := hostedPostgresAdmin(t, f.service)
	installReportsBudgetTrace(t, f.service)
	seedProductionProject(t, f.service, hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project)}, productionProjectShape{issues: 594, attempts: 469, usage: 262, current: 131, leases: 1682, comments: 7000})
	seedRetainedReportHistory(t, admin, "org_browser_preview", 10)
	freezeReportPlannerInputs(t, admin)
	t.Run("rules reject injected unindexed reads", func(t *testing.T) {
		for _, statement := range []pgstoretest.ReportStatement{
			{Query: "SELECT count(*) FROM collaboration_events WHERE organization_id=$1", Args: []any{"org_browser_preview"}},
			{Query: "SELECT id FROM native_attempts WHERE organization_id=$1 AND length(id)>0 ORDER BY length(id),id", Args: []any{"org_browser_preview"}},
		} {
			if report, err := pgstoretest.CheckReportRequest(t.Context(), admin, "org_browser_preview", []pgstoretest.ReportStatement{statement}, reportsBudgetHotRelations, pgstoretest.ReportRequestBudget{Overhead: 1 << 30}); err != nil || len(report.Problems) == 0 {
				t.Fatalf("unindexed source read accepted: %s error=%v", statement.Query, err)
			}
		}
	})
	organization := browserHostedOrganizationBase
	project := organization + "/projects/" + f.project
	var item string
	if err := admin.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE organization_id=$1 AND project_id=$2 ORDER BY id LIMIT 1", "org_browser_preview", f.project).Scan(&item); err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		name string
		path string
	}{
		{"activity", organization + "/activity"},
		{"newest issue history", project + "/work-items/" + item + "/history?direction=newest&view=timeline&limit=100"},
		{"newest issue attempts", project + "/work-items/" + item + "/attempts?direction=newest&limit=100"},
		{"newest issue comments", project + "/work-items/" + item + "/comments?direction=newest&limit=100"},
		{"work items list", project + "/work-items?limit=50"},
		{"board lanes", project + "/work-items?include=board&state=Todo&open=true&limit=50"},
		{"projects", organization + "/projects"},
		{"reports", organization + "/reports?project=" + f.project + "&range=30d"},
		{"reports filtered by issue", organization + "/reports?project=" + f.project + "&range=30d&labels=cloud&priorities=1&types=bug&filed_by=principal"},
		{"reports filtered by attempt", organization + "/reports?project=" + f.project + "&range=30d&runners=runner_browser_preview&models=gpt-6-astra&stages=code"},
		{"report filter options", organization + "/reports/options?project=all&range=30d"},
		{"reports all projects over a custom range", organization + "/reports?project=all&from=" + f.service.config.Clock().UTC().AddDate(0, 0, -89).Format(time.DateOnly) + "&to=" + f.service.config.Clock().UTC().Format(time.DateOnly) + "&labels=cloud"},
		{"diagnostics", organization + "/diagnostics?range=30d"},
	} {
		t.Run(read.name, func(t *testing.T) {
			var mu sync.Mutex
			statements := []tracedStatement{}
			timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
				if args == nil {
					return
				}
				values := make([]any, len(args))
				for i, arg := range args {
					values[i] = arg.Value
				}
				mu.Lock()
				statements = append(statements, tracedStatement{query, values})
				mu.Unlock()
			}}
			ctx := testenv.WaitContext(t, withRequestTiming(t.Context(), timing))
			request := httptest.NewRequest(http.MethodGet, f.server.URL+read.path, nil).WithContext(ctx)
			request.AddCookie(f.cookies["owner"])
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			mu.Lock()
			captured := append([]tracedStatement(nil), statements...)
			mu.Unlock()
			reads := 0
			for _, statement := range captured {
				head := strings.ToUpper(strings.TrimSpace(statement.query))
				if strings.HasPrefix(head, "SELECT") || strings.HasPrefix(head, "WITH") {
					reads++
				}
				if read.name == "reports" && strings.HasPrefix(statement.query, "SELECT i.id,coalesce(i.actor_principal_id,'')") {
					t.Errorf("an unfiltered report ran a filter or option statement: %s", statement.query)
				}
			}
			if reads == 0 {
				t.Fatal("no read statements were traced")
			}
			reportsAssertPostgresPlans(t, admin, "org_browser_preview", captured, read.name)
		})
	}
}

func TestAnalyticsReportPlans(t *testing.T) {
	t.Parallel()
	for _, population := range []struct {
		name        string
		shape       productionProjectShape
		transitions int
	}{
		{"source volume", productionProjectShape{issues: 11, attempts: 5000, usage: 3000, leases: 5000, days: 90, attemptDays: 90}, 20000},
		{"attempt payloads", productionProjectShape{issues: 594, attempts: 469, usage: 262, leases: 1682, comments: 7000}, 0},
	} {
		t.Run(population.name, func(t *testing.T) {
			t.Parallel()
			f := newUsageHostedFixture(t)
			installReportsBudgetTrace(t, f.service)
			scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project)}
			seedProductionProject(t, f.service, scope, population.shape)
			if population.transitions > 0 {
				seedReportsVolumeTransitions(t, f.service, scope, population.transitions)
			}
			admin := hostedPostgresAdmin(t, f.service)
			seedRetainedReportHistory(t, admin, scope.Organization, 10)
			freezeReportPlannerInputs(t, admin)
			for _, window := range []string{"24h", "7d", "30d"} {
				t.Run(window, func(t *testing.T) {
					var mu sync.Mutex
					var statements []tracedStatement
					timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
						if args == nil {
							return
						}
						values := make([]any, len(args))
						for i, arg := range args {
							values[i] = arg.Value
						}
						mu.Lock()
						statements = append(statements, tracedStatement{query, values})
						mu.Unlock()
					}}
					ctx := testenv.WaitContext(t, withRequestTiming(t.Context(), timing))
					request := httptest.NewRequest(http.MethodGet, browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+window, nil).WithContext(ctx)
					request.AddCookie(f.cookies["owner"])
					response := httptest.NewRecorder()
					started := time.Now()
					f.service.Handler().ServeHTTP(response, request)
					elapsed := time.Since(started)
					if response.Code != http.StatusOK {
						t.Fatalf("HTTP %d after %s: %s", response.Code, elapsed, response.Body.String())
					}
					var report hostedReportsReport
					if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
						t.Fatal(err)
					}
					if report.Analytics.Window.From.IsZero() || !report.Analytics.Window.From.Before(report.Analytics.Window.To) {
						t.Fatalf("empty report window: %+v", report.Analytics.Window)
					}
					timing.Start = started
					t.Logf("reports %s on %s took %s: %s", window, population.name, elapsed, timing.Header(started.Add(elapsed)))
					mu.Lock()
					captured := append([]tracedStatement(nil), statements...)
					mu.Unlock()
					if len(captured) == 0 {
						t.Fatal("no report statements traced")
					}
					reportsAssertPostgresPlans(t, admin, scope.Organization, captured, "reports", "issues", "native_attempts", "collaboration_events", "attempt_usage", "change_requests")
				})
			}
		})
	}
}

func seedReportsVolumeTransitions(t *testing.T, s *Service, scope hubscope.NativeScope, count int) {
	t.Helper()
	content := eventstore.NewContent(s.runDetailStore())
	now := s.config.Clock().UTC().Truncate(time.Microsecond)
	for first := 0; first < count; first += 100 {
		records := make([]eventstore.Event, 0, 100)
		for index := first; index < min(first+100, count); index++ {
			from := "Todo"
			if index%5 == 0 {
				from = "Done"
			}
			row, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("event_volume_%d", index), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: tracker.NativeWorkItemID(string(scope.Project) + "-wi_shaped_0"), Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: now.Add(-time.Duration(index*7776000/count) * time.Second), Data: tracker.CollaborationData{FromState: from, ToState: "In Progress"}}))
			if err != nil {
				t.Fatal(err)
			}
			records = append(records, row)
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, row := range records {
				if _, err := w.AppendEvent(t.Context(), row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var got int
	if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND id LIKE 'event_volume_%' AND type='workflow.transitioned'", scope.Organization, scope.Project).Scan(&got); err != nil || got != count {
		t.Fatalf("volume workflow events=%d want=%d error=%v", got, count, err)
	}
}

var retainedReportHistoryKeys = []struct {
	relation string
	keys     map[string]string
}{
	{"issues", map[string]string{"id": "offset", "native_id": "suffix", "number": "offset", "native_source_key": "suffix", "archived": "=true"}},
	{"leases", map[string]string{"lease_id": "suffix", "issue_id": "offset", "session_id": "suffix", "fencing_token": "offset", "released_at": "=coalesce(released_at,expires_at)-history_copy*interval '2880 hours'"}},
	{"native_attempt_runs", map[string]string{"run_id": "suffix", "issue_id": "offset"}},
	{"native_attempts", map[string]string{"id": "suffix", "issue_id": "offset", "lease_id": "suffix", "run_id": "suffix", "fencing_token": "offset", "status": "=CASE WHEN status='running' THEN 'cancelled' ELSE status END"}},
	{"collaboration_events", map[string]string{"id": "suffix", "work_item_id": "suffix", "aggregate_id": "suffix", "attempt_id": "suffix", "change_id": "suffix", "version_id": "suffix", "run_id": "suffix", "lease_id": "suffix"}},
	{"change_requests", map[string]string{"id": "suffix", "work_item_id": "suffix", "current_version_id": "suffix", "landed_version_id": "suffix"}},
	{"change_versions", map[string]string{"id": "suffix", "change_id": "suffix", "run_id": "suffix", "attempt_id": "suffix", "work_item_id": "suffix"}},
	{"attempt_usage", map[string]string{"attempt_id": "suffix"}}, {"cost_sources", map[string]string{"source_key": "suffix", "resource_id": "suffix"}}, {"report_usage_current", map[string]string{"source_key": "suffix", "attempt_id": "suffix"}},
}

func seedRetainedReportHistory(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, copies int) {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, table := range retainedReportHistoryKeys {
		rows, err := tx.QueryContext(t.Context(), `SELECT column_name,data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND is_generated='NEVER' ORDER BY ordinal_position`, table.relation)
		if err != nil {
			t.Fatal(err)
		}
		var columns, values []string
		for rows.Next() {
			var name, kind string
			if err := rows.Scan(&name, &kind); err != nil {
				t.Fatal(err)
			}
			value := name
			switch {
			case strings.HasPrefix(table.keys[name], "="):
				value = strings.TrimPrefix(table.keys[name], "=")
			case table.keys[name] == "offset":
				value = fmt.Sprintf("%s+history_copy*10000000", name)
			case table.keys[name] == "suffix":
				value = fmt.Sprintf("CASE WHEN %s IS NULL OR %s='' THEN %s ELSE %s||'_h'||history_copy END", name, name, name, name)
			case strings.HasPrefix(kind, "timestamp"):
				value = fmt.Sprintf("%s-history_copy*interval '2880 hours'", name)
			}
			columns, values = append(columns, name), append(values, value)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		statement := fmt.Sprintf(`INSERT INTO %s(%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s CROSS JOIN generate_series(1,$2) history_copy WHERE organization_id=$1`, table.relation, strings.Join(columns, ","), strings.Join(values, ","), table.relation)
		if _, err := tx.ExecContext(t.Context(), statement, organization, copies); err != nil {
			t.Fatalf("retained %s history: %v", table.relation, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type productionProjectShape struct {
	issues, attempts, usage, current, leases, comments int
	days, attemptDays                                  int
}

func seedProductionProject(t *testing.T, s *Service, scope hubscope.NativeScope, shape productionProjectShape) {
	t.Helper()
	admin := hostedPostgresAdmin(t, s)
	now := s.config.Clock().UTC().Truncate(time.Microsecond)
	days := shape.days
	if days == 0 {
		days = 2
	}
	states := []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}, {Name: "In Progress"}, {Name: "Rework", Dispatchable: true}, {Name: "Merging"}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		record.States = states
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	items := make([]tracker.NativeIssue, shape.issues)
	worked := []int{}
	steps := []struct {
		state          string
		sequence       int
		kind, from, to string
		offset         time.Duration
	}{
		{"Done", 2, "workflow.transitioned", "Backlog", "Todo", 600 * time.Second}, {"Done", 3, "scheduler.decision", "", "", 900 * time.Second}, {"Done", 4, "workflow.transitioned", "Todo", "In Progress", 1200 * time.Second},
		{"Done", 5, "workflow.transitioned", "In Progress", "Merging", 7200 * time.Second}, {"Done", 6, "workflow.transitioned", "Merging", "Rework", 7800 * time.Second}, {"Done", 7, "scheduler.decision", "", "", 8000 * time.Second},
		{"Done", 8, "workflow.transitioned", "Rework", "In Progress", 8100 * time.Second}, {"Done", 9, "workflow.transitioned", "In Progress", "Merging", 8400 * time.Second}, {"Done", 10, "workflow.transitioned", "Merging", "Done", 9000 * time.Second},
		{"Cancelled", 2, "workflow.transitioned", "Backlog", "Cancelled", 1800 * time.Second}, {"Todo", 2, "workflow.transitioned", "Backlog", "Todo", 600 * time.Second},
		{"In Progress", 2, "workflow.transitioned", "Backlog", "Todo", 600 * time.Second}, {"In Progress", 3, "scheduler.decision", "", "", 900 * time.Second}, {"In Progress", 4, "workflow.transitioned", "Todo", "In Progress", 1200 * time.Second},
	}
	for index := range shape.issues {
		created := now.Add(-time.Duration(days*86400-index*(days*86400-28800)/shape.issues) * time.Second)
		state := "Backlog"
		switch n := index % 11; {
		case n < 4:
			state = "Done"
		case n == 4:
			state = "Cancelled"
		case n == 9:
			state = "Todo"
		case n == 10:
			state = "In Progress"
		}
		id := fmt.Sprintf("wi_shaped_%d", index)
		items[index] = seedAnalyticsTestWorkItem(t, s, scope.Organization, scope.Project, id, 100000+index, "Example report item", state, created)
		var events []eventstore.Event
		for _, step := range steps {
			if step.state != state {
				continue
			}
			data := tracker.CollaborationData{FromState: step.from, ToState: step.to}
			if step.kind == "scheduler.decision" {
				data.Decision = &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", At: created.Add(step.offset)}
			}
			event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: items[index].WorkItemID, Type: step.kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: created.Add(step.offset), Data: data}))
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		if len(events) > 0 {
			if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, event := range events {
					if _, err := w.AppendEvent(t.Context(), event); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if state == "Done" || state == "In Progress" {
			worked = append(worked, index)
		}
		if state == "Done" && (100000+index)%11 < 3 {
			seedAnalyticsTestChange(t, s, scope, items[index].WorkItemID, "change_"+id, created, &tracker.ChangeLanding{MergeSHA: fmt.Sprintf("%040x", index+1), BaseRef: "develop", LandedAt: created.Add(9000 * time.Second)})
		}
	}
	if len(worked) == 0 && (shape.attempts > 0 || shape.leases > 0) {
		t.Fatal("report fixture has no worked item")
	}
	for index := range shape.attempts {
		item := items[worked[index%len(worked)]]
		started := item.CreatedAt.Add(time.Duration(1200+index/len(worked)*1800) * time.Second)
		updated := started.Add(1500 * time.Second)
		role := []string{"code", "validator", "merge"}[index%3]
		if shape.attemptDays > 0 {
			started = now.Add(-time.Duration(index*shape.attemptDays*86400/shape.attempts) * time.Second)
			updated = started.Add(30 * time.Minute)
			role = "code"
		}
		status := "succeeded"
		if index%14 == 0 || shape.attemptDays > 0 && index%7 == 0 {
			status = "failed"
		}
		if shape.attemptDays == 0 && index < len(worked) && index%15 == 1 {
			status = "running"
		}
		runtime := &tracker.NativeRuntimeObservation{Phase: role, Identity: agentidentity.Identity{Role: role}, Phases: []tracker.NativePhase{{Name: role, StartedAt: started, FinishedAt: updated}}, Activity: &workflowmetrics.ActivityProfile{Schema: 1, Stage: role, AttemptID: int64(index + 1), Generation: 1, StartedAt: started, AsOf: updated, FinishedAt: updated, Status: "completed", Coverage: "complete"}}
		spanCount, evidenceBytes := 6, 2800
		if shape.attemptDays > 0 {
			spanCount, evidenceBytes = 1, 3000
		}
		for span := range spanCount {
			runtime.Activity.Spans = append(runtime.Activity.Spans, workflowmetrics.ActivitySpan{ID: fmt.Sprintf("span_%d", span), Kind: "command", Evidence: strings.Repeat("x", evidenceBytes), Attribution: "agent", StartedAt: started, FinishedAt: updated, Outcome: "succeeded", Repeat: 1})
		}
		if role == "validator" {
			runtime.Validation = &gate.CommandResult{Command: "make check", HeadSHA: strings.Repeat("a", 40), StartedAt: started, FinishedAt: updated, Output: strings.Repeat("x", 65536)}
		}
		seedAnalyticsTestAttempt(t, admin, s, scope, item.WorkItemID, fmt.Sprintf("attempt_shaped_%d", index), status, started, updated, tracker.NativeRunData{Runtime: runtime})
	}
	for index := shape.attempts; index < shape.leases; index++ {
		item := items[worked[index%len(worked)]]
		started := item.CreatedAt.Add(time.Duration(1200+index/len(worked)*1800) * time.Second)
		ended := started.Add(1500 * time.Second)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at)SELECT $1,$3,i.id,'analytics-fixture',$4,1,$5,$5,$6::timestamptz+interval '1 hour',$6 FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$7`, scope.Organization, scope.Project, fmt.Sprintf("lease_shaped_extra_%d", index), fmt.Sprintf("session_shaped_extra_%d", index), started, ended, item.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	for index := range shape.usage {
		attempt := fmt.Sprintf("attempt_shaped_%d", index*shape.attempts/shape.usage)
		if _, err := admin.ExecContext(t.Context(), `WITH usage AS (INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at)SELECT organization_id,project_id,id,date_trunc('hour',started_at),'openai','gpt-6-astra',100000,40000,8000,0.25,'USD',updated_at FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND id=$3 RETURNING *), source AS (INSERT INTO cost_sources(organization_id,source_key,project_id,provider,provider_account,resource_id,bucket,metric,period_start,period_end,revision,attribution_hash,payload_hash,amount_micros,currency,voided)SELECT organization_id,'source_'||attempt_id,project_id,provider,'example',attempt_id,'runner_api','tokens',to_char(period,'YYYY-MM-DD"T"HH24'),to_char(period+interval '1 hour','YYYY-MM-DD"T"HH24'),1,'example','example',250000,currency,false FROM usage WHERE $4 RETURNING source_key,resource_id) INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete,currency)SELECT u.organization_id,s.source_key,1,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,true,u.currency FROM usage u JOIN source s ON s.resource_id=u.attempt_id`, scope.Organization, scope.Project, attempt, index < shape.current); err != nil {
			t.Fatal(err)
		}
	}
	body, err := content.PreparePayload(t.Context(), scope.Organization, []byte(strings.Repeat("x", 200)))
	if err != nil {
		t.Fatal(err)
	}
	for first := 0; first < shape.comments; first += 100 {
		comments := make([]corestore.Comment, 0, 100)
		events := make([]eventstore.Event, 0, 100)
		for index := first; index < min(first+100, shape.comments); index++ {
			item := items[index%len(items)]
			at := item.CreatedAt.Add(time.Duration(10800+index/len(items)*60) * time.Second)
			id := fmt.Sprintf("comment_shaped_%d", index)
			comments = append(comments, corestore.Comment{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.WorkItemID, ID: id, Sequence: int64(index/len(items) + 1), BodyObjectKey: body.Key, ActorKind: "human", ActorPrincipalID: "example", CreatedAt: at, UpdatedAt: at})
			event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item.WorkItemID, Type: "comment.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: at, Data: tracker.CollaborationData{CommentID: id}}))
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			for index, comment := range comments {
				if _, err := w.CreateComment(t.Context(), comment); err != nil {
					return err
				}
				if _, err := w.AppendEvent(t.Context(), events[index]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		relation, predicate string
		want                int
	}{
		{"issues", "native_id LIKE $3", shape.issues}, {"native_attempts", "id LIKE 'attempt_shaped_%'", shape.attempts}, {"attempt_usage", "attempt_id LIKE 'attempt_shaped_%'", shape.usage}, {"native_comments", "id LIKE 'comment_shaped_%'", shape.comments},
	} {
		var got int
		args := []any{scope.Organization, scope.Project}
		if test.relation == "issues" {
			args = append(args, string(scope.Project)+"-wi_shaped_%")
		}
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.relation+" WHERE organization_id=$1 AND project_id=$2 AND "+test.predicate, args...).Scan(&got); err != nil || got != test.want {
			t.Fatalf("report fixture %s count=%d want=%d error=%v", test.relation, got, test.want, err)
		}
	}
	wantRunning := 0
	if shape.attemptDays == 0 {
		wantRunning = (min(shape.attempts, len(worked)) + 13) / 15
	}
	var running int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND id LIKE 'attempt_shaped_%' AND status='running'", scope.Organization, scope.Project).Scan(&running); err != nil || running != wantRunning {
		t.Fatalf("report fixture running attempts=%d want=%d error=%v", running, wantRunning, err)
	}
	var leases int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*)FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id LIKE $3`, scope.Organization, scope.Project, string(scope.Project)+"-wi_shaped_%").Scan(&leases); err != nil || leases != shape.leases {
		t.Fatalf("report fixture leases=%d want=%d error=%v", leases, shape.leases, err)
	}
}
