package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsFilterDimensions(t *testing.T) {
	f := newUsageHostedFixture(t)
	session, err := f.service.hostedSessions.Authenticate(t.Context(), f.cookies["owner"].Value)
	if err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.hostedSessionCredential(t.Context(), session, apikey.HashToken(f.cookies["owner"].Value))
	if err != nil || status != http.StatusOK {
		t.Fatalf("owner credential status=%d error=%v", status, err)
	}
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project), credential: credential}
	to := f.service.config.now().UTC().Truncate(time.Hour)
	priority := func(value int) *int { return &value }
	seeds := []struct {
		name, state, kind, filer string
		labels                   []string
		priority                 *int
	}{
		{"cloud bug", "Done", "bug", "principal_rpt_one", []string{"rpt-cloud"}, priority(1)},
		{"cloud ui change", "Done", "change_request", "principal_rpt_two", []string{"rpt-cloud", "rpt-ui"}, priority(2)},
		{"ui change", "Done", "change_request", "principal_rpt_one", []string{"rpt-ui"}, priority(1)},
		{"runner bug", "Done", "bug", "principal_rpt_two", nil, nil},
		{"cancelled cloud", "Cancelled", "change_request", "principal_rpt_one", []string{"rpt-cloud"}, priority(1)},
	}
	admin := hostedPostgresAdmin(t, f.service)
	content := eventstore.NewContent(f.service.runDetailStore())
	seedIssue := func(scope nativeScope, id tracker.NativeWorkItemID, number int64, title, state, kind, filer string, labels []string, priority *int, created, closed time.Time, existing bool) tracker.NativeIssue {
		t.Helper()
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: id, Number: int(number), Revision: 1}, Title: title, Body: nativeContractTestBody, State: state, Terminal: state == "Done" || state == "Cancelled", Type: kind, Priority: priority, Labels: labels, Actor: tracker.Actor{Kind: "human", PrincipalID: filer}, CreatedAt: created, UpdatedAt: closed, LastActivityAt: closed}
		if issue.Terminal {
			issue.ClosedAt = &closed
		}
		from := "Todo"
		var current corestore.WorkItem
		if existing {
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				current, err = r.WorkItem(t.Context(), scope.project, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			issue.Number, issue.Revision = int(current.Number), tracker.Revision(current.Revision+1)
			from = stringValue(current.WorkflowStateName)
		}
		fields, err := f.service.prepareWorkItemFields(t.Context(), scope.organization, workItemRecordPlan{issue: issue, create: !existing})
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: id, RecordID: string(id), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.organization, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: id, Type: "workflow.transitioned", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: closed, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: from, ToState: state}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if existing {
				current.Title, current.IssueType, current.Priority = title, kind, priority
				current.BodyObjectKey, current.IssueContractObjectKey = &fields.Body, fields.Contract
				current.ActorKind, current.ActorPrincipalID, current.WorkflowStateName = &issue.Actor.Kind, &issue.Actor.PrincipalID, &state
				if _, err := w.UpdateWorkItem(t.Context(), current); err != nil {
					return err
				}
			} else if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.organization, ProjectID: scope.project, NativeID: id, Number: number, Revision: 1, Title: title, IssueType: kind, Priority: priority, BodyObjectKey: &fields.Body, IssueContractObjectKey: fields.Contract, WorkflowStateName: &state, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: created, NativeCreatedAt: created, UpdatedAt: closed, NativeUpdatedAt: closed, LastActivityAt: closed}); err != nil {
				return err
			}
			if err := w.SetWorkItemLabels(t.Context(), scope.project, id, labels); err != nil {
				return err
			}
			if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: id, Author: filer}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			if err := w.BindDoctorProposalIdentities(t.Context(), corestore.DoctorProposalIdentityBinding{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: id, Revision: int64(issue.Revision), VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(issue.Body)}); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var terminal *time.Time
		if issue.Terminal {
			terminal = &closed
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE issues SET created_at=CASE WHEN $7 THEN created_at ELSE $1 END,native_created_at=$1,updated_at=$2,native_updated_at=$2,terminal_entered_at=$3 WHERE organization_id=$4 AND project_id=$5 AND native_id=$6", created, closed, terminal, scope.organization, scope.project, id, existing); err != nil {
			t.Fatal(err)
		}
		return issue
	}
	var runnerItem tracker.NativeWorkItemID
	if err := admin.QueryRowContext(t.Context(), "SELECT i.native_id FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id='attempt_browser_usage_0_0'", scope.organization, scope.project).Scan(&runnerItem); err != nil {
		t.Fatal(err)
	}
	for i, seed := range seeds {
		id, created, existing := tracker.NativeWorkItemID(newNativeID("issue")), to.Add(-30*time.Hour), false
		if seed.name == "runner bug" {
			id, created, existing = runnerItem, to.AddDate(0, 0, -20), true
		}
		seedIssue(scope, id, int64(1000+i), seed.name, seed.state, seed.kind, seed.filer, seed.labels, seed.priority, created, to.Add(-time.Duration(10+i)*time.Hour), existing)
	}
	drainTestNativeBoard(t, f.service)
	read := func(t *testing.T, query url.Values) hostedReportsReport {
		t.Helper()
		var report hostedReportsReport
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?"+query.Encode(), http.StatusOK), &report)
		return report
	}
	base := url.Values{"project": {f.project}, "range": {"24h"}}
	unfiltered := read(t, base)
	population, err := f.service.readIssues(t.Context(), scope, url.Values{"state": {"Done"}, "archived": {"all"}, "limit": {"200"}})
	if err != nil {
		t.Fatal(err)
	}
	shipped := []tracker.NativeIssue{}
	for _, issue := range population.Items {
		if issue.State == "Done" && issue.ClosedAt != nil && !issue.ClosedAt.Before(to.Add(-24*time.Hour)) && issue.ClosedAt.Before(to) {
			shipped = append(shipped, issue)
		}
	}
	if unfiltered.Completion.Done != len(shipped) || unfiltered.Completion.Done < 4 {
		t.Fatalf("unfiltered shipped=%d population=%d", unfiltered.Completion.Done, len(shipped))
	}
	for _, tt := range []struct {
		name      string
		filter    url.Values
		shipped   []string
		cancelled int
		workList  url.Values
	}{
		{"one label", url.Values{"labels": {"rpt-cloud"}}, []string{"cloud bug", "cloud ui change"}, 1, url.Values{"label": {"rpt-cloud"}}},
		{"either label", url.Values{"labels[]": {"rpt-cloud", "rpt-ui"}}, []string{"cloud bug", "cloud ui change", "ui change"}, 1, url.Values{"label": {"rpt-cloud", "rpt-ui"}}},
		{"priority", url.Values{"priorities": {"2"}}, []string{"cloud ui change"}, 0, url.Values{"priority": {"2"}}},
		{"type", url.Values{"types": {"bug"}}, []string{"cloud bug", "runner bug"}, 0, url.Values{"type": {"bug"}}},
		{"filer", url.Values{"filed_by": {"principal_rpt_two"}}, []string{"cloud ui change", "runner bug"}, 0, url.Values{"created_by": {"principal_rpt_two"}}},
		{"runner", url.Values{"runners": {"runner_browser_preview"}}, []string{"runner bug"}, 0, nil},
		{"label and type and priority", url.Values{"labels": {"rpt-cloud"}, "types": {"change_request"}, "priorities": {"2"}}, []string{"cloud ui change"}, 0, url.Values{"label": {"rpt-cloud"}, "type": {"change_request"}, "priority": {"2"}}},
		{"label and runner", url.Values{"labels": {"rpt-cloud"}, "runners": {"runner_browser_preview"}}, nil, 0, nil},
		{"filer and label", url.Values{"filed_by": {"principal_rpt_one"}, "labels": {"rpt-ui"}}, []string{"ui change"}, 0, url.Values{"created_by": {"principal_rpt_one"}, "label": {"rpt-ui"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query := url.Values{}
			for key, values := range base {
				query[key] = values
			}
			for key, values := range tt.filter {
				query[key] = values
			}
			report := read(t, query)
			if report.Completion.Done != len(tt.shipped) || report.Completion.Cancelled != tt.cancelled {
				t.Fatalf("shipped=%d cancelled=%d, want %d (%v) and %d", report.Completion.Done, report.Completion.Cancelled, len(tt.shipped), tt.shipped, tt.cancelled)
			}
			if tt.workList == nil {
				return
			}
			params := url.Values{"archived": {"all"}, "limit": {"200"}}
			for key, values := range tt.workList {
				params[key] = values
			}
			page, err := f.service.readIssues(t.Context(), scope, params)
			if err != nil {
				t.Fatal(err)
			}
			listed := 0
			for _, issue := range page.Items {
				if slices.ContainsFunc(shipped, func(s tracker.NativeIssue) bool { return s.WorkItemID == issue.WorkItemID }) {
					listed++
				}
			}
			if listed != report.Completion.Done {
				t.Fatalf("work_list lists %d shipped issues for %v, reports %d", listed, tt.workList, report.Completion.Done)
			}
		})
	}
	t.Run("attempt filters narrow cost", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			filter url.Values
			all    bool
		}{
			{"recorded stage", url.Values{"stages": {reportsUnavailableName}}, true},
			{"other stage", url.Values{"stages": {"code"}}, false},
			{"other model", url.Values{"models": {"model-not-run"}}, false},
			{"runner", url.Values{"runners": {"runner_browser_preview"}}, true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				query := url.Values{"project": {f.project}, "range": {"7d"}}
				for key, values := range tt.filter {
					query[key] = values
				}
				all, report := read(t, url.Values{"project": {f.project}, "range": {"7d"}}), read(t, query)
				if all.Cost == 0 {
					t.Fatal("the fixture recorded no spend")
				}
				if want := map[bool]float64{true: all.Cost, false: 0}[tt.all]; report.Cost != want || len(report.Spend) == 0 != !tt.all {
					t.Fatalf("cost=%v spend=%d, want %v", report.Cost, len(report.Spend), want)
				}
			})
		}
	})
	t.Run("all projects", func(t *testing.T) {
		legacy := read(t, url.Values{"project": {reportsAllProjects}, "range": {"24h"}, "labels": {"rpt-cloud"}})
		projectReport := read(t, url.Values{"project": {f.project}, "range": {"24h"}, "labels": {"rpt-cloud"}})
		if legacy.Analytics.ProjectID != reportsAllProjects || legacy.Completion.Done != projectReport.Completion.Done || legacy.Completion.Cost != projectReport.Completion.Cost || legacy.Rework == nil || legacy.Failures == nil || legacy.Stages == nil || legacy.Spend == nil {
			t.Fatalf("legacy organization report=%+v", legacy)
		}
		var organization organizationProductivity
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?scope=organization&project=all&range=24h&labels=rpt-cloud", http.StatusOK), &organization)
		project := read(t, url.Values{"project": {f.project}, "range": {"24h"}, "labels": {"rpt-cloud"}})
		if organization.Current.Completed != project.Completion.Done || organization.Current.CompletionCost != project.Completion.Cost || organization.Current.Cancelled != project.Completion.Cancelled {
			t.Fatalf("organization=%+v project=%+v", organization.Current, project.Completion)
		}
		f.usage(t, "staff", browserHostedOrganizationBase+"/reports?scope=organization&project=all&range=24h", http.StatusForbidden)
	})
	t.Run("custom range", func(t *testing.T) {
		day := to.Add(-time.Hour).Format(time.DateOnly)
		report := read(t, url.Values{"project": {f.project}, "from": {to.AddDate(0, 0, -6).Format(time.DateOnly)}, "to": {day}})
		if !report.Analytics.Window.To.Equal(to) || !report.Analytics.Window.From.Equal(to.AddDate(0, 0, -6).Truncate(24*time.Hour)) {
			t.Fatalf("window=%+v", report.Analytics.Window)
		}
		before := read(t, url.Values{"project": {f.project}, "from": {to.AddDate(0, 0, -89).Format(time.DateOnly)}, "to": {to.AddDate(0, 0, -80).Format(time.DateOnly)}})
		if before.PriorRecorded || before.PriorCost != nil || before.Completion.Done != 0 {
			t.Fatalf("a window before the project has prior data: recorded=%t cost=%v", before.PriorRecorded, before.PriorCost)
		}
		if !unfiltered.PriorRecorded {
			t.Fatal("the seeded issues predate the 24h window but no prior is recorded")
		}
	})
	t.Run("compare off drops the prior period", func(t *testing.T) {
		report := read(t, url.Values{"project": {f.project}, "range": {"7d"}, "compare": {"off"}})
		if report.PriorCost != nil || report.PriorFailed != nil || report.Previous.Done != 0 || report.OpenWork.PreviousStuck != nil {
			t.Fatalf("prior period kept: %+v", report.Previous)
		}
	})
	t.Run("options read only the readable projects", func(t *testing.T) {
		secret := nativeScope{organization: scope.organization, project: "prj_rpt_secret"}
		project, err := f.service.readTenantProjectRecord(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: secret.project, Name: "secret", Slug: "secret", Profile: "native"}, States: project.States})
			if err != nil {
				return err
			}
			cleaner, ok := w.(accessstore.ProjectAccessCleaner)
			if !ok {
				return ErrProjectParticipants
			}
			return cleaner.DeleteAccessProjectGrants(t.Context(), secret.project)
		}); err != nil {
			t.Fatal(err)
		}
		issue := seedIssue(secret, "issue_rpt_secret", 1, "secret", "Todo", "bug", "principal_rpt_secret", []string{"rpt-secret"}, nil, to.Add(-3*time.Hour), to.Add(-3*time.Hour), false)
		var issueID int64
		if err := admin.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", scope.organization, secret.project, issue.WorkItemID).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		started, finished := to.Add(-2*time.Hour), to.Add(-time.Hour)
		var fence int64
		if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at) VALUES($1,'lease_rpt_secret',$2,'machine_browser_preview_runner','session_rpt_secret',1,$3,$4,$5) RETURNING fencing_token`, scope.organization, issueID, to.Add(time.Hour), started, finished).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_rpt_secret',$2)", scope.organization, issueID); err != nil {
			t.Fatal(err)
		}
		data := tracker.NativeRunData{RunID: "run_rpt_secret", AttemptID: "attempt_rpt_secret", LeaseID: "lease_rpt_secret", FencingToken: tracker.FencingToken(fence), MachineID: "machine_browser_preview_runner", RunnerID: "runner_browser_preview_shared", Sequence: 1, Outcome: "succeeded"}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := content.PreparePayload(t.Context(), scope.organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.organization, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_rpt_secret", OrganizationID: scope.organization, ProjectID: secret.project, AggregateID: issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview_shared"}, RecordedAt: finished, Data: tracker.CollaborationData{Run: &data}}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) VALUES($1,$2,'attempt_rpt_secret',$3,'lease_rpt_secret',$4,'run_rpt_secret',1,'succeeded',1,1,$5,$6,$7)`, scope.organization, secret.project, issueID, fence, ref.Key, started, finished); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: scope.organization, LeaseID: "lease_rpt_secret", RunnerID: "runner_browser_preview_shared", AssignedAt: started}); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id='lease_rpt_secret'", finished, scope.organization); err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{f.project, reportsAllProjects} {
			var facets reportsFacets
			usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports/options?project="+project+"&range=24h", http.StatusOK), &facets)
			runner := func(id string) bool {
				return slices.ContainsFunc(facets.Runners, func(r reportsFacet) bool { return r.ID == id })
			}
			filer := func(id string) bool {
				return slices.ContainsFunc(facets.FiledBy, func(r reportsFacet) bool { return r.ID == id })
			}
			if !slices.Contains(facets.Labels, "rpt-cloud") || !filer("principal_rpt_two") || !runner("runner_browser_preview") {
				t.Fatalf("%s options miss the readable project: %+v", project, facets)
			}
			if slices.Contains(facets.Labels, "rpt-secret") || filer("principal_rpt_secret") || runner("runner_browser_preview_shared") {
				t.Fatalf("%s options leak an unreadable project or an idle runner: %+v", project, facets)
			}
		}
		f.usage(t, "owner", browserHostedOrganizationBase+"/reports/options?project="+string(secret.project)+"&range=24h", http.StatusNotFound)
		f.usage(t, "owner", browserHostedOrganizationBase+"/reports?project="+string(secret.project)+"&range=24h", http.StatusNotFound)
		var organization organizationProductivity
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?scope=organization&project=all&range=24h", http.StatusOK), &organization)
		if organization.Current.Completed != unfiltered.Completion.Done {
			t.Fatalf("all projects counted an unreadable project: %d, want %d", organization.Current.Completed, unfiltered.Completion.Done)
		}
	})
}

func TestReportsRequestValidation(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		query    string
		from, to time.Time
		invalid  bool
	}{
		{"preset", "range=7d", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC), false},
		{"custom days", "from=2026-10-01&to=2026-10-05", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), false},
		{"custom through today ends at the last hour", "from=2026-10-12&to=2026-10-13", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC), false},
		{"ninety days", "from=2026-07-15&to=2026-10-12", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), false},
		{"over ninety days", "from=2026-07-14&to=2026-10-12", time.Time{}, time.Time{}, true},
		{"reversed", "from=2026-10-05&to=2026-10-01", time.Time{}, time.Time{}, true},
		{"future", "from=2026-10-14&to=2026-10-15", time.Time{}, time.Time{}, true},
		{"one end", "from=2026-10-01", time.Time{}, time.Time{}, true},
		{"unknown range", "range=1y", time.Time{}, time.Time{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			w, err := reportsWindow(params, now)
			if (err != nil) != tt.invalid {
				t.Fatalf("err=%v invalid=%t", err, tt.invalid)
			}
			if !tt.invalid && (!w.From.Equal(tt.from) || !w.To.Equal(tt.to) || (w.To.Sub(w.From)-1)/w.Bucket+1 > 240) {
				t.Fatalf("window=%+v want %s..%s", w, tt.from, tt.to)
			}
		})
	}
	firstHour := time.Date(2026, 10, 13, 0, 30, 0, 0, time.UTC)
	if w, err := reportsWindow(url.Values{"from": {"2026-10-13"}, "to": {"2026-10-13"}}, firstHour); err != nil || !w.From.Equal(firstHour.Truncate(24*time.Hour)) || !w.To.Equal(firstHour) {
		t.Fatalf("today in its first hour: window=%+v err=%v, want midnight to now", w, err)
	}
	for _, tt := range []struct {
		name    string
		query   string
		want    reportsFilter
		invalid bool
	}{
		{"none", "", reportsFilter{Labels: []string{}, Priorities: []string{}, Types: []string{}, FiledBy: []string{}, Runners: []string{}, Models: []string{}, Stages: []string{}}, false},
		{"repeated and bracketed", "labels=b&labels[]=a&labels=b&priorities=0&types=bug&filed_by=p&runners=r&models=m&stages=code", reportsFilter{Labels: []string{"a", "b"}, Priorities: []string{"0"}, Types: []string{"bug"}, FiledBy: []string{"p"}, Runners: []string{"r"}, Models: []string{"m"}, Stages: []string{"code"}}, false},
		{"priority out of range", "priorities=4", reportsFilter{}, true},
		{"priority not a number", "priorities=high", reportsFilter{}, true},
		{"unknown type", "types=epic", reportsFilter{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseReportsFilter(params)
			if (err != nil) != tt.invalid {
				t.Fatalf("err=%v invalid=%t", err, tt.invalid)
			}
			if !tt.invalid && !slicesEqualFilter(got, tt.want) {
				t.Fatalf("filter=%+v want %+v", got, tt.want)
			}
		})
	}
}

func slicesEqualFilter(a, b reportsFilter) bool {
	return slices.Equal(a.Labels, b.Labels) && slices.Equal(a.Priorities, b.Priorities) && slices.Equal(a.Types, b.Types) && slices.Equal(a.FiledBy, b.FiledBy) && slices.Equal(a.Runners, b.Runners) && slices.Equal(a.Models, b.Models) && slices.Equal(a.Stages, b.Stages)
}

func TestMergeHostedReports(t *testing.T) {
	w := operatortool.AnalyticsWindow{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Bucket: time.Hour}
	failed := func(n int) *int { return &n }
	cost := func(v float64) *float64 { return &v }
	report := func(done int, cycle []float64, first reportsRatio, priorFailed *int, priorCost *float64, issueCosts []float64) hostedReportsReport {
		return hostedReportsReport{
			Completion:  reportsCompletion{Done: done, System: sampledDuration(slices.Clone(cycle)), perIssue: issueCosts, Cost: 1},
			Analytics:   nativeAnalyticsProject{ProjectID: "p", Unavailable: []string{"recorded_usage", "activity_complete_window"}},
			Runners:     []reportsRunner{{RunnerID: "r", Sessions: 1, Duration: sampledDuration(slices.Clone(cycle))}},
			Spend:       []reportsSpend{{WorkItemID: "wi", Cost: float64(done)}},
			firstTry:    first,
			FirstTry:    first.percent(),
			PriorFailed: priorFailed,
			PriorCost:   priorCost,
			Unavailable: []string{},
		}
	}
	for _, tt := range []struct {
		name        string
		reports     []hostedReportsReport
		done        int
		p50         float64
		firstTry    *float64
		priorFailed *int
		priorCost   *float64
		unavailable []string
	}{
		{"hourly histogram and exact samples", []hostedReportsReport{
			func() hostedReportsReport {
				r := report(2, nil, reportsRatio{1, 2}, failed(1), cost(2), nil)
				d := operatortool.AnalyticsDuration{Count: 2, Seconds: 30, Histogram: []operatortool.AnalyticsDurationBucket{{Count: 1, Minimum: 10, Maximum: 10}, {Count: 1, Minimum: 20, Maximum: 20}}}
				r.Completion.System = d
				r.Runners[0].Duration = d
				return r
			}(),
			report(2, []float64{30, 100}, reportsRatio{1, 2}, failed(1), cost(2), nil),
		}, 4, 25, new(50.0), failed(2), cost(4), []string{"activity_complete_window", "recorded_usage"}},
		{"combined samples and counts", []hostedReportsReport{
			report(2, []float64{10, 20}, reportsRatio{1, 2}, failed(1), cost(2), []float64{1, 3}),
			report(3, []float64{30, 40, 50}, reportsRatio{3, 3}, failed(2), nil, []float64{5}),
		}, 5, 30, new(80.0), failed(3), cost(2), []string{"activity_complete_window", "recorded_usage"}},
		{"a recorded prior period without cost leaves the organization's prior cost unavailable", []hostedReportsReport{
			report(1, []float64{10}, reportsRatio{1, 1}, failed(0), cost(4), nil),
			func() hostedReportsReport {
				r := report(1, []float64{20}, reportsRatio{1, 1}, failed(0), nil, nil)
				r.PriorRecorded = true
				return r
			}(),
		}, 2, 15, new(100.0), failed(0), nil, []string{"activity_complete_window", "recorded_usage"}},
		{"a first-try rate a project could not compute leaves the organization's unavailable", []hostedReportsReport{
			report(1, []float64{10}, reportsRatio{1, 1}, failed(0), nil, nil),
			func() hostedReportsReport {
				r := report(1, []float64{20}, reportsRatio{}, failed(0), nil, nil)
				r.firstTryMissing = true
				return r
			}(),
		}, 2, 15, nil, failed(0), nil, []string{"activity_complete_window", "recorded_usage"}},
		{"a project without a prior count has no prior total", []hostedReportsReport{
			report(1, []float64{10}, reportsRatio{}, failed(1), nil, nil),
			report(1, []float64{20}, reportsRatio{}, nil, nil, nil),
		}, 2, 15, nil, nil, nil, []string{"activity_complete_window", "recorded_usage"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeHostedReports(tt.reports, w)
			if got.Completion.Done != tt.done || got.Completion.System.P50Seconds != tt.p50 || got.Runners[0].Duration.P50Seconds != tt.p50 || got.Runners[0].Sessions != len(tt.reports) {
				t.Fatalf("completion=%+v runners=%+v", got.Completion, got.Runners)
			}
			if (got.FirstTry == nil) != (tt.firstTry == nil) || got.FirstTry != nil && *got.FirstTry != *tt.firstTry {
				t.Fatalf("first try=%v want %v", got.FirstTry, tt.firstTry)
			}
			if (got.PriorFailed == nil) != (tt.priorFailed == nil) || got.PriorFailed != nil && *got.PriorFailed != *tt.priorFailed {
				t.Fatalf("prior failed=%v want %v", got.PriorFailed, tt.priorFailed)
			}
			if (got.PriorCost == nil) != (tt.priorCost == nil) || got.PriorCost != nil && *got.PriorCost != *tt.priorCost {
				t.Fatalf("prior cost=%v want %v", got.PriorCost, tt.priorCost)
			}
			if !slices.Equal(got.Analytics.Unavailable, tt.unavailable) || got.Analytics.ProjectID != reportsAllProjects || len(got.Spend) != len(tt.reports) || got.Spend[0].Cost < got.Spend[1].Cost {
				t.Fatalf("analytics=%+v spend=%+v", got.Analytics.Unavailable, got.Spend)
			}
		})
	}
}

func TestMergeReportClassesAndCoverage(t *testing.T) {
	observed := func(n int) *int { return &n }
	reports := []hostedReportsReport{
		{
			Failures: []reportClassTotal{{Class: "Hub request timed out", Count: 3, itemCounts: map[string]int{"wi_a": 1, "wi_b": 2}}},
			Coverage: []diagnosticsCoverage{{Source: "Usage joins", Observed: observed(2), Total: observed(4)}, {Source: "Skip reasons", Observed: observed(1), Total: observed(1)}},
		},
		{
			Failures: []reportClassTotal{{Class: "Hub request timed out", Count: 5, itemCounts: map[string]int{"wi_c": 4, "wi_a": 1}}},
			Coverage: []diagnosticsCoverage{{Source: "Usage joins", Observed: observed(3), Total: nil}},
		},
	}
	classes := mergeReportClasses(reports, func(r hostedReportsReport) []reportClassTotal { return r.Failures })
	if len(classes) != 1 || classes[0].Count != 8 || !slices.Equal(classes[0].WorkItems, []string{"wi_c", "wi_a", "wi_b"}) {
		t.Fatalf("classes=%+v, want one class of 8 with its issues ordered by combined count", classes)
	}
	coverage := mergeReportsCoverage(reports)
	if len(coverage) != 2 || coverage[0].Observed == nil || *coverage[0].Observed != 5 || coverage[0].Total != nil || coverage[1].Observed != nil {
		t.Fatalf("coverage=%+v, want usage joins observed 5 with no total and skip reasons unobserved where a project did not report it", coverage)
	}
}
