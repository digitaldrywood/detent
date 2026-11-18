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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsFilterDimensions(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	session, err := f.service.hostedSessions.Authenticate(t.Context(), f.cookies["owner"].Value)
	if err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.hostedSessionCredential(t.Context(), session, apikey.HashToken(f.cookies["owner"].Value))
	if err != nil || status != http.StatusOK {
		t.Fatalf("owner credential status=%d error=%v", status, err)
	}
	scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project), Credential: credential}
	to := f.service.config.Clock().UTC().Truncate(time.Hour)
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
	seedIssue := func(scope hubscope.NativeScope, id tracker.NativeWorkItemID, number int64, title, state, kind, filer string, labels []string, priority *int, created, closed time.Time, existing bool) tracker.NativeIssue {
		t.Helper()
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, Number: int(number), Revision: 1}, Title: title, Body: nativeContractTestBody, State: state, Terminal: state == "Done" || state == "Cancelled", Type: kind, Priority: priority, Labels: labels, Actor: tracker.Actor{Kind: "human", PrincipalID: filer}, CreatedAt: created, UpdatedAt: closed, LastActivityAt: closed}
		if issue.Terminal {
			issue.ClosedAt = &closed
		}
		from := "Todo"
		var current corestore.WorkItem
		if existing {
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				current, err = r.WorkItem(t.Context(), scope.Project, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			issue.Number, issue.Revision = int(current.Number), tracker.Revision(current.Revision+1)
			from = hubapi.StringValue(current.WorkflowStateName)
		}
		fields, err := f.service.prepareWorkItemFields(t.Context(), scope.Organization, hubrecords.WorkItemRecordPlan{Issue: issue, Create: !existing})
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, RecordID: string(id), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: id, Type: "workflow.transitioned", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: closed, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: from, ToState: state}}))
		if err != nil {
			t.Fatal(err)
		}
		var filing eventstore.Event
		if !existing {
			filing, err = content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: id, Type: "issue.created", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: created, Data: tracker.CollaborationData{Revision: 1}}))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if existing {
				current.Title, current.IssueType, current.Priority = title, kind, priority
				current.BodyObjectKey, current.IssueContractObjectKey = &fields.Body, fields.Contract
				current.ActorKind, current.ActorPrincipalID, current.WorkflowStateName = &issue.Actor.Kind, &issue.Actor.PrincipalID, &state
				if _, err := w.UpdateWorkItem(t.Context(), current); err != nil {
					return err
				}
			} else if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: id, Number: number, Revision: 1, Title: title, IssueType: kind, Priority: priority, BodyObjectKey: &fields.Body, IssueContractObjectKey: fields.Contract, WorkflowStateName: &state, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: created, NativeCreatedAt: created, UpdatedAt: closed, NativeUpdatedAt: closed, LastActivityAt: closed}); err != nil {
				return err
			}
			if err := w.SetWorkItemLabels(t.Context(), scope.Project, id, labels); err != nil {
				return err
			}
			if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, Author: filer}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			if err := w.BindDoctorProposalIdentities(t.Context(), corestore.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, Revision: int64(issue.Revision), VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(issue.Body)}); err != nil {
				return err
			}
			if !existing {
				if _, err := w.AppendEvent(t.Context(), filing); err != nil {
					return err
				}
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
		if _, err := admin.ExecContext(t.Context(), "UPDATE issues SET created_at=CASE WHEN $7 THEN created_at ELSE $1 END,native_created_at=$1,updated_at=$2,native_updated_at=$2,terminal_entered_at=$3 WHERE organization_id=$4 AND project_id=$5 AND native_id=$6", created, closed, terminal, scope.Organization, scope.Project, id, existing); err != nil {
			t.Fatal(err)
		}
		return issue
	}
	var runnerItem tracker.NativeWorkItemID
	if err := admin.QueryRowContext(t.Context(), "SELECT i.native_id FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id='attempt_browser_usage_0_0'", scope.Organization, scope.Project).Scan(&runnerItem); err != nil {
		t.Fatal(err)
	}
	for i, seed := range seeds {
		id, created, existing := tracker.NativeWorkItemID(hubapi.NewID("issue")), to.Add(-30*time.Hour), false
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
		secret := hubscope.NativeScope{Organization: scope.Organization, Project: "prj_rpt_secret"}
		project, err := f.service.readTenantProjectRecord(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.Organization, ID: secret.Project, Name: "secret", Slug: "secret", Profile: "native"}, States: project.States})
			if err != nil {
				return err
			}
			cleaner, ok := w.(accessstore.ProjectAccessCleaner)
			if !ok {
				return hubapi.ErrProjectParticipants
			}
			return cleaner.DeleteAccessProjectGrants(t.Context(), secret.Project)
		}); err != nil {
			t.Fatal(err)
		}
		issue := seedIssue(secret, "issue_rpt_secret", 1, "secret", "Todo", "bug", "principal_rpt_secret", []string{"rpt-secret"}, nil, to.Add(-3*time.Hour), to.Add(-3*time.Hour), false)
		var issueID int64
		if err := admin.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", scope.Organization, secret.Project, issue.WorkItemID).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		started, finished := to.Add(-2*time.Hour), to.Add(-time.Hour)
		var fence int64
		if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at) VALUES($1,'lease_rpt_secret',$2,'machine_browser_preview_runner','session_rpt_secret',1,$3,$4,$5) RETURNING fencing_token`, scope.Organization, issueID, to.Add(time.Hour), started, finished).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_rpt_secret',$2)", scope.Organization, issueID); err != nil {
			t.Fatal(err)
		}
		data := tracker.NativeRunData{RunID: "run_rpt_secret", AttemptID: "attempt_rpt_secret", LeaseID: "lease_rpt_secret", FencingToken: tracker.FencingToken(fence), MachineID: "machine_browser_preview_runner", RunnerID: "runner_browser_preview_shared", Sequence: 1, Outcome: "succeeded"}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := content.PreparePayload(t.Context(), scope.Organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_rpt_secret", OrganizationID: scope.Organization, ProjectID: secret.Project, AggregateID: issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview_shared"}, RecordedAt: finished, Data: tracker.CollaborationData{Run: &data}}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) VALUES($1,$2,'attempt_rpt_secret',$3,'lease_rpt_secret',$4,'run_rpt_secret',1,'succeeded',1,1,$5,$6,$7)`, scope.Organization, secret.Project, issueID, fence, ref.Key, started, finished); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: scope.Organization, LeaseID: "lease_rpt_secret", RunnerID: "runner_browser_preview_shared", AssignedAt: started}); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id='lease_rpt_secret'", finished, scope.Organization); err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{f.project, reportsAllProjects} {
			var facets reportsFacets
			usageDecode(t, f.usage(t, "viewer", browserHostedOrganizationBase+"/reports/options?project="+project+"&range=24h", http.StatusOK), &facets)
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
		f.usage(t, "viewer", browserHostedOrganizationBase+"/reports/options?project="+string(secret.Project)+"&range=24h", http.StatusNotFound)
		f.usage(t, "viewer", browserHostedOrganizationBase+"/reports?project="+string(secret.Project)+"&range=24h", http.StatusNotFound)
		var organization organizationProductivity
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?scope=organization&project=all&range=24h", http.StatusOK), &organization)
		if organization.Current.Completed != unfiltered.Completion.Done {
			t.Fatalf("all projects counted an unreadable project: %d, want %d", organization.Current.Completed, unfiltered.Completion.Done)
		}
	})
}
