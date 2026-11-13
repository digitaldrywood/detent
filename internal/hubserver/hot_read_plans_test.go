package hubserver

import (
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
	f := newUsageHostedFixture(t)
	admin := hostedPostgresAdmin(t, f.service)
	installReportsBudgetTrace(t, f.service)
	seedProductionProject(t, f.service, nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}, productionProjectShape{issues: 594, attempts: 469, usage: 262, leases: 1682, comments: 7000})
	if _, err := admin.ExecContext(t.Context(), "ANALYZE issues,collaboration_events,native_attempts,change_requests,change_evidence,attempt_usage,report_usage_current,report_escape_facts"); err != nil {
		t.Fatal(err)
	}
	t.Run("rules reject injected unindexed reads", func(t *testing.T) {
		for _, statement := range []tracedStatement{
			{"SELECT count(*) FROM collaboration_events WHERE organization_id=$1", []any{"org_browser_preview"}},
			{"SELECT id FROM native_attempts WHERE organization_id=$1 AND length(id)>0 ORDER BY length(id),id", []any{"org_browser_preview"}},
		} {
			plan, raw := reportsExplainPostgresPlan(t, admin, "org_browser_preview", statement)
			if violations := reportsBudgetPlanViolations(statement, plan); len(violations) == 0 {
				t.Fatalf("unindexed source read accepted: %s plan=%s", statement.query, raw)
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
		{"reports all projects over a custom range", organization + "/reports?project=all&from=" + f.service.config.now().UTC().AddDate(0, 0, -89).Format(time.DateOnly) + "&to=" + f.service.config.now().UTC().Format(time.DateOnly) + "&labels=cloud"},
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
			reportsAssertPostgresPlans(t, admin, "org_browser_preview", captured)
		})
	}
}

func TestAnalyticsReportPlans(t *testing.T) {
	for _, population := range []struct {
		name        string
		shape       productionProjectShape
		transitions int
	}{
		{"source volume", productionProjectShape{issues: 11, attempts: 5000, usage: 3000, leases: 5000, days: 90, attemptDays: 90}, 20000},
		{"attempt payloads", productionProjectShape{issues: 594, attempts: 469, usage: 262, leases: 1682, comments: 7000}, 0},
	} {
		t.Run(population.name, func(t *testing.T) {
			f := newUsageHostedFixture(t)
			installReportsBudgetTrace(t, f.service)
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
			seedProductionProject(t, f.service, scope, population.shape)
			if population.transitions > 0 {
				seedReportsVolumeTransitions(t, f.service, scope, population.transitions)
			}
			admin := hostedPostgresAdmin(t, f.service)
			if _, err := admin.ExecContext(t.Context(), "ANALYZE issues,collaboration_events,native_attempts,change_requests,change_evidence,attempt_usage,report_usage_current,report_escape_facts"); err != nil {
				t.Fatal(err)
			}
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
					reportsAssertPostgresPlans(t, admin, scope.organization, captured, "issues", "native_attempts", "collaboration_events", "attempt_usage", "change_requests")
				})
			}
		})
	}
}

func seedReportsVolumeTransitions(t *testing.T, s *Service, scope nativeScope, count int) {
	t.Helper()
	content := eventstore.NewContent(s.runDetailStore())
	now := s.config.now().UTC().Truncate(time.Microsecond)
	for first := 0; first < count; first += 100 {
		records := make([]eventstore.Event, 0, 100)
		for index := first; index < min(first+100, count); index++ {
			from := "Todo"
			if index%5 == 0 {
				from = "Done"
			}
			row, err := content.PrepareEvent(t.Context(), scope.organization, historyTestJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("event_volume_%d", index), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: tracker.NativeWorkItemID(string(scope.project) + "-wi_shaped_0"), Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: now.Add(-time.Duration(index*7776000/count) * time.Second), Data: tracker.CollaborationData{FromState: from, ToState: "In Progress"}}))
			if err != nil {
				t.Fatal(err)
			}
			records = append(records, row)
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
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
	if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND id LIKE 'event_volume_%' AND type='workflow.transitioned'", scope.organization, scope.project).Scan(&got); err != nil || got != count {
		t.Fatalf("volume workflow events=%d want=%d error=%v", got, count, err)
	}
}

type productionProjectShape struct {
	issues, attempts, usage, leases, comments int
	days, attemptDays                         int
}

func seedProductionProject(t *testing.T, s *Service, scope nativeScope, shape productionProjectShape) {
	t.Helper()
	admin := hostedPostgresAdmin(t, s)
	now := s.config.now().UTC().Truncate(time.Microsecond)
	days := shape.days
	if days == 0 {
		days = 2
	}
	states := []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}, {Name: "In Progress"}, {Name: "Rework", Dispatchable: true}, {Name: "Merging"}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), scope.project)
		if err != nil {
			return err
		}
		record.States = states
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
		items[index] = seedAnalyticsTestWorkItem(t, s, scope.organization, scope.project, id, 100000+index, "Example report item", state, created)
		for _, step := range steps {
			if step.state != state {
				continue
			}
			data := tracker.CollaborationData{FromState: step.from, ToState: step.to}
			if step.kind == "scheduler.decision" {
				data.Decision = &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", At: created.Add(step.offset)}
			}
			appendAnalyticsTestEvent(t, s, scope, items[index].WorkItemID, step.kind, data, created.Add(step.offset))
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
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at)SELECT $1,$3,i.id,'analytics-fixture',$4,1,$5,$5,$6::timestamptz+interval '1 hour',$6 FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$7`, scope.organization, scope.project, fmt.Sprintf("lease_shaped_extra_%d", index), fmt.Sprintf("session_shaped_extra_%d", index), started, ended, item.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	for index := range shape.usage {
		attempt := fmt.Sprintf("attempt_shaped_%d", index*shape.attempts/shape.usage)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at)SELECT organization_id,project_id,id,date_trunc('hour',started_at),'openai','gpt-6-astra',100000,40000,8000,0.25,'USD',updated_at FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND id=$3`, scope.organization, scope.project, attempt); err != nil {
			t.Fatal(err)
		}
	}
	content := eventstore.NewContent(s.runDetailStore())
	body, err := content.PreparePayload(t.Context(), scope.organization, []byte(strings.Repeat("x", 200)))
	if err != nil {
		t.Fatal(err)
	}
	for index := range shape.comments {
		item := items[index%len(items)]
		at := item.CreatedAt.Add(time.Duration(10800+index/len(items)*60) * time.Second)
		id := fmt.Sprintf("comment_shaped_%d", index)
		if err := s.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item.WorkItemID, ID: id, Sequence: int64(index/len(items) + 1), BodyObjectKey: body.Key, ActorKind: "human", ActorPrincipalID: "example", CreatedAt: at, UpdatedAt: at})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		appendAnalyticsTestEvent(t, s, scope, item.WorkItemID, "comment.created", tracker.CollaborationData{CommentID: id}, at)
	}
	for _, test := range []struct {
		relation, predicate string
		want                int
	}{
		{"issues", "native_id LIKE $3", shape.issues}, {"native_attempts", "id LIKE 'attempt_shaped_%'", shape.attempts}, {"attempt_usage", "attempt_id LIKE 'attempt_shaped_%'", shape.usage}, {"native_comments", "id LIKE 'comment_shaped_%'", shape.comments},
	} {
		var got int
		args := []any{scope.organization, scope.project}
		if test.relation == "issues" {
			args = append(args, string(scope.project)+"-wi_shaped_%")
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
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND id LIKE 'attempt_shaped_%' AND status='running'", scope.organization, scope.project).Scan(&running); err != nil || running != wantRunning {
		t.Fatalf("report fixture running attempts=%d want=%d error=%v", running, wantRunning, err)
	}
	var leases int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*)FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id LIKE $3`, scope.organization, scope.project, string(scope.project)+"-wi_shaped_%").Scan(&leases); err != nil || leases != shape.leases {
		t.Fatalf("report fixture leases=%d want=%d error=%v", leases, shape.leases, err)
	}
}
