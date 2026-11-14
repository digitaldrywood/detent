package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationProductivity(t *testing.T) {
	t.Parallel()
	f := diagnosticsPostgresFixtures(t)[0]
	service := f.hosted.service
	to := service.config.Clock().UTC().Truncate(time.Hour).Add(time.Hour)
	service.config.Clock = func() time.Time { return to }
	org := f.organization
	projects := []tracker.ProjectID{f.hosted.project, "prj_productivity_second", "prj_productivity_secret"}
	states := append(nativeFixtureStates(), tracker.NativeState{Name: "Cancelled", Terminal: true}, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Done"}})
	reader := f.hosted.user(t, "productivity_reader", "viewer", "reader@example.test", "", "")
	if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), reader.identity.Subject)
		if err != nil {
			return err
		}
		for i, project := range projects {
			if i > 0 {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: states}); err != nil {
					return err
				}
			} else {
				record, err := w.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				record.States = states
				if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
			}
			if i < 2 {
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: project}); err != nil {
					return err
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: member.PrincipalID, ProjectID: project}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.details)
	productivitySeedRunners(t.Context(), t, f.repository, org, to)
	type seed struct {
		name            string
		project         int
		created, closed int
		state, kind     string
		priority        int
		actor           tracker.Actor
	}
	seeds := []seed{
		{"human completed", 0, -20, -2, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"runner completed", 1, -30, -3, "Done", "change_request", 2, tracker.Actor{Kind: "human", PrincipalID: "person_two", Agent: "assistant"}},
		{"cancelled", 0, -18, -4, "Cancelled", "bug", 1, tracker.Actor{Kind: "integration", PrincipalID: "automation_one"}},
		{"open", 1, -10, 0, "Todo", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"prior done", 0, -40, -26, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"prior cancelled", 1, -45, -27, "Cancelled", "change_request", 2, tracker.Actor{Kind: "human", PrincipalID: "person_two"}},
		{"secret", 2, -20, -2, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "secret"}},
	}

	ids := map[string]string{}
	for i, seed := range seeds {
		id := tracker.NativeWorkItemID(fmt.Sprintf("productivity_%d", i))
		ids[seed.name] = string(id)
		created := to.Add(time.Duration(seed.created) * time.Hour)
		steps := []productivityTransition{}
		if seed.closed != 0 {
			steps = append(steps, productivityTransition{created.Add(time.Hour), "Todo", "In Progress", seed.actor, ""})
			if seed.name == "runner completed" {
				steps = append(steps,
					productivityTransition{to.Add(-6 * time.Hour), "In Progress", "Rework", seed.actor, "missing criterion"},
					productivityTransition{to.Add(-5 * time.Hour), "Rework", "In Progress", seed.actor, "missing criterion"})
			}
			actor := seed.actor
			if seed.name == "runner completed" {
				actor = tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview"}
			}
			steps = append(steps, productivityTransition{to.Add(time.Duration(seed.closed) * time.Hour), "In Progress", seed.state, actor, ""})
		}
		productivitySeedItem(t.Context(), t, f.repository, content, org, projects[seed.project], id, int64(i+1), seed.name, seed.state, seed.kind, new(seed.priority), seed.actor, created, steps)
	}
	for _, seed := range []struct {
		name  string
		costs map[int]float64
	}{
		{"human completed", map[int]float64{-25: 3, -1: 4}},
		{"runner completed", map[int]float64{-28: 5}},
		{"prior done", map[int]float64{-28: 2, -1: 10}},
	} {
		project := projects[0]
		if seed.name == "runner completed" {
			project = projects[1]
		}
		id := tracker.NativeWorkItemID(ids[seed.name])
		attempt := "attempt_" + string(id)
		productivitySeedAttempt(t.Context(), t, f, project, id, attempt, "runner_browser_preview", to.Add(-29*time.Hour), to.Add(-3*time.Hour))
		usage := []reportstore.UsageRecord{}
		for hour, cost := range seed.costs {
			usage = append(usage, reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: project, WorkItemID: id, AttemptID: attempt, Period: to.Add(time.Duration(hour) * time.Hour), Provider: "provider", Model: "model", Cost: cost, Currency: "USD"}, UpdatedAt: to})
		}
		if err := f.repository.ImportReportUsageFacts(t.Context(), usage); err != nil {
			t.Fatal(err)
		}
		if err := f.repository.VerifyReportUsageImport(t.Context(), org, attempt, strings.Repeat("a", 64), int64(len(usage))); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"human completed", "runner completed", "prior done"} {
		project, hour := projects[0], -2
		if name == "runner completed" {
			project, hour = projects[1], -3
		}
		if name == "prior done" {
			hour = -26
		}
		productivitySeedLanding(t.Context(), t, f.repository, content, org, project, tracker.NativeWorkItemID(ids[name]), to.Add(time.Duration(hour)*time.Hour))
	}
	read := func(params url.Values) postgresOrganizationProductivity {
		t.Helper()
		params.Set("project", "all")
		params.Set("scope", "organization")
		response := f.hosted.request(t, reader, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?"+params.Encode(), nil)
		if response.Code != http.StatusOK {
			t.Fatalf("report status=%d body=%s", response.Code, response.Body.String())
		}
		var out postgresOrganizationProductivity
		usageDecode(t, response, &out)
		return out
	}
	for _, tt := range []struct {
		name                                                                            string
		params                                                                          url.Values
		filed, completed, cancelled, landed, priorFiled, priorCompleted, priorCancelled int
		cost                                                                            float64
		bucket                                                                          time.Duration
	}{
		{"24h", url.Values{"range": {"24h"}}, 3, 2, 1, 2, 3, 1, 1, 12, time.Hour},
		{"7d", url.Values{"range": {"7d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"30d", url.Values{"range": {"30d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"90d", url.Values{"range": {"90d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"custom", url.Values{"from": {to.AddDate(0, 0, -6).Format(time.DateOnly)}, "to": {to.Format(time.DateOnly)}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.params.Set("labels", "productivity")
			out := read(tt.params)
			if out.Current.Filed != tt.filed || out.Current.Completed != tt.completed || out.Current.Cancelled != tt.cancelled || out.Current.ChangesLanded == nil || *out.Current.ChangesLanded != tt.landed || out.Current.NetBacklog != tt.filed-tt.completed-tt.cancelled || out.Current.CompletionCost == nil || *out.Current.CompletionCost != tt.cost {
				t.Fatalf("current=%+v", out.Current)
			}
			if out.Previous.Filed != tt.priorFiled || out.Previous.Completed != tt.priorCompleted || out.Previous.Cancelled != tt.priorCancelled {
				t.Fatalf("previous=%+v", out.Previous)
			}
			if len(out.Projects) != 2 || out.Window.Bucket != tt.bucket || out.PreviousWindow.To.Sub(out.PreviousWindow.From) != out.Window.To.Sub(out.Window.From) {
				t.Fatalf("projects=%d window=%+v previous=%+v", len(out.Projects), out.Window, out.PreviousWindow)
			}
			for _, row := range out.Projects {
				if row.ProjectID != string(projects[0]) && row.ProjectID != string(projects[1]) {
					t.Fatalf("unreadable project: %s", row.ProjectID)
				}
			}
			assertProductivitySums(t, out)
			if tt.name == "24h" {
				for _, row := range out.Projects {
					var project hostedReportsReport
					response := f.hosted.request(t, reader, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?project="+row.ProjectID+"&range=24h&labels=productivity", nil)
					if response.Code != http.StatusOK {
						t.Fatalf("project report status=%d body=%s", response.Code, response.Body.String())
					}
					usageDecode(t, response, &project)
					if row.Current.Completed != project.Completion.Done || row.Current.Cancelled != project.Completion.Cancelled || row.Current.CompletionCost == nil || *row.Current.CompletionCost != project.Completion.Cost || row.Current.Worked == nil || *row.Current.Worked != project.Analytics.Quality.WorkedItems || row.Current.Reworked == nil || *row.Current.Reworked != project.Analytics.Quality.ReworkedItems {
						t.Fatalf("summary=%+v project completion=%+v quality=%+v", row.Current, project.Completion, project.Analytics.Quality)
					}
				}
				if out.Current.CostPerCompleted == nil || *out.Current.CostPerCompleted != 6 || out.Previous.CostPerCompleted == nil || *out.Previous.CostPerCompleted != 2 || out.Current.CycleP50 == nil || *out.Current.CycleP50 != 22.5*3600 {
					t.Fatalf("completion metrics: %+v / %+v", out.Current, out.Previous)
				}
				if out.Current.Worked == nil || *out.Current.Worked != 3 || out.Current.Reworked == nil || *out.Current.Reworked != 1 || out.Current.ReworkRate == nil || math.Abs(*out.Current.ReworkRate-100.0/3) > 1e-9 {
					t.Fatalf("quality=%+v", out.Current)
				}
				if !slices.ContainsFunc(out.CompletedBy, func(a productivityActor) bool {
					return a.Kind == "runner" && a.ID == "runner_browser_preview" && a.Name != "" && a.Current == 1
				}) || !slices.ContainsFunc(out.CompletedBy, func(a productivityActor) bool {
					return a.Kind == "people" && a.ID == "person_one" && a.Current == 1 && a.Previous == 1
				}) {
					t.Fatalf("completed by=%+v", out.CompletedBy)
				}
				for kind, want := range map[string]int{"people": 2, "automation": 1} {
					if !slices.ContainsFunc(out.FiledBy, func(a productivityActor) bool { return a.Kind == kind && a.Current == want }) {
						t.Fatalf("filed by=%+v", out.FiledBy)
					}
				}
			}
		})
	}
	for _, tt := range []struct {
		name, key, value            string
		filed, completed, cancelled int
	}{
		{"label", "labels", "change_request", 0, 1, 0},
		{"priority", "priorities", "2", 0, 1, 0},
		{"type", "types", "change_request", 0, 1, 0},
		{"filer", "filed_by", "person_one", 2, 1, 0},
		{"runner", "runners", "runner_browser_preview", 1, 2, 0},
		{"project", "projects", string(projects[1]), 1, 1, 0},
		{"unreadable project", "projects", string(projects[2]), 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params := url.Values{"range": {"24h"}, "labels": {"productivity"}}
			params.Set(tt.key, tt.value)
			if tt.name == "unreadable project" {
				params.Set("project", "all")
				params.Set("scope", "organization")
				response := f.hosted.request(t, reader, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?"+params.Encode(), nil)
				if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "secret") {
					t.Fatalf("unreadable project report status=%d body=%s", response.Code, response.Body.String())
				}
				return
			}
			out := read(params)
			if out.Current.Filed != tt.filed || out.Current.Completed != tt.completed || out.Current.Cancelled != tt.cancelled {
				t.Fatalf("filtered=%+v", out.Current)
			}
			assertProductivitySums(t, out)
		})
	}

	for i, tt := range []struct {
		name, body, kind string
		actor            tracker.Actor
	}{
		{"machine origin", issueorigin.Stamp(nativeContractTestBody, issueorigin.Origin{Kind: "doctor", Instance: "example-instance", Fingerprint: "example-fingerprint"}), "automation", tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"human agent", nativeContractTestBody, "agents", tracker.Actor{Kind: "human", PrincipalID: "person_one", Agent: "assistant"}},
		{"human", nativeContractTestBody, "people", tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
	} {
		t.Run("filer attribution/"+tt.name, func(t *testing.T) {
			id := tracker.NativeWorkItemID(fmt.Sprintf("filer_%d", i))
			label := fmt.Sprintf("filer-%d", i)
			productivitySeedItem(t.Context(), t, f.repository, content, org, projects[0], id, int64(100+i), tt.name, "Todo", "", nil, tt.actor, to.Add(-time.Hour), nil, productivitySeedItemFields{Body: tt.body, Labels: []string{label}})
			report := read(url.Values{"range": {"24h"}, "labels": {label}})
			if report.Current.Filed != 1 || len(report.FiledBy) != 1 || report.FiledBy[0].Kind != tt.kind || report.FiledBy[0].Current != 1 {
				t.Fatalf("filer attribution=%+v current=%+v", report.FiledBy, report.Current)
			}
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, org, projects[0], id); err != nil {
				t.Fatal(err)
			}
			response := f.hosted.request(t, reader, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?project=all&scope=organization&range=24h&labels="+label, nil)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("pending filer classification status=%d body=%s", response.Code, response.Body.String())
			}
			if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				version, err := w.CollaborationVersion(t.Context(), string(id), 1)
				if err != nil {
					return err
				}
				return w.BindDoctorProposalIdentities(t.Context(), corestore.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: projects[0], WorkItemID: id, Revision: 1, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(tt.body)})
			}); err != nil {
				t.Fatal(err)
			}
			restored := read(url.Values{"range": {"24h"}, "labels": {label}})
			if restored.Current.Filed != 1 || len(restored.FiledBy) != 1 || restored.FiledBy[0].Kind != tt.kind || restored.FiledBy[0].Current != 1 {
				t.Fatalf("restored filer attribution=%+v", restored.FiledBy)
			}
			owner, status, err := service.hostedSessionCredential(t.Context(), auth.Session{Identity: f.owner.identity.Hosted, Email: f.owner.identity.Email}, apikey.HashToken(f.owner.token))
			if err != nil {
				t.Fatalf("owner credential status=%d: %v", status, err)
			}
			raw, err := service.transitionNativeIssueTenant(t.Context(), hubscope.NativeScope{Organization: org, Project: projects[0], Credential: owner}, string(id), tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "filer-transition-" + string(id)}, ExpectedRevision: 1, State: "Done", Reason: "user_requested"})
			if err != nil {
				t.Fatal(err)
			}
			var transitioned tracker.NativeIssue
			if err := json.Unmarshal(raw, &transitioned); err != nil {
				t.Fatal(err)
			}
			if transitioned.Revision != 2 || transitioned.State != "Done" {
				t.Fatalf("transition=%+v", transitioned)
			}
			afterTransition := read(url.Values{"range": {"24h"}, "labels": {label}})
			if afterTransition.Current.Filed != 1 || len(afterTransition.FiledBy) != 1 || afterTransition.FiledBy[0].Kind != tt.kind || afterTransition.FiledBy[0].Current != 1 {
				t.Fatalf("transition invalidated filer attribution=%+v", afterTransition.FiledBy)
			}
		})
	}
	t.Run("historical actor envelopes", func(t *testing.T) {
		before := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
		tamperPostgresRows(t, f.admin, `UPDATE collaboration_events SET actor_envelope_known=false,actor_agent='',actor_display_name='' WHERE organization_id=$1 AND (type='issue.created' OR type='workflow.transitioned')`, org)
		after := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
		a, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(after)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("historical canonical actor attribution changed: before=%s after=%s", a, b)
		}
	})
	before := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET payload_object_key='fixture/unavailable' WHERE organization_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	after := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
	a, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("read depends on live attempt payloads")
	}
	for _, tt := range []struct{ name, runner, kind, displayName string }{
		{"resolved final runner", "runner_browser_preview_shared", "runner", "Shared runner"},
		{"unknown final runner", "", "unknown", ""},
	} {
		t.Run("completion attribution/"+tt.name, func(t *testing.T) {
			for _, name := range []string{"runner completed", "human completed"} {
				id := tracker.NativeWorkItemID(ids[name])
				project, wantKind, wantID, wantName := projects[1], tt.kind, tt.runner, tt.displayName
				if name == "human completed" {
					project, wantKind, wantID, wantName = projects[0], "people", "person_one", ""
				}
				productivitySeedAttempt(t.Context(), t, f, project, id, "attempt_final_"+tt.kind+"_"+string(id), tt.runner, to.Add(-4*time.Hour), to.Add(-3*time.Hour))
				report := read(url.Values{"range": {"24h"}, "labels": {"productivity"}, "projects": {string(project)}})
				if !slices.ContainsFunc(report.CompletedBy, func(actor productivityActor) bool {
					return actor.Kind == wantKind && actor.ID == wantID && actor.Name == wantName && actor.Current == 1
				}) {
					t.Fatalf("%s attribution=%+v want=(%q,%q,%q)", name, report.CompletedBy, wantKind, wantID, wantName)
				}
			}
		})
	}
	for _, tt := range []struct {
		name          string
		createdHours  [2]int
		selected      string
		priorRecorded bool
	}{
		{"new projects", [2]int{-12, -12}, "", false},
		{"prior end boundary", [2]int{-24, -24}, "", false},
		{"project in prior window", [2]int{-25, -12}, "", true},
		{"second project in prior window", [2]int{-12, -25}, "", true},
		{"old project without activity", [2]int{-72, -72}, "", true},
		{"selected new project", [2]int{-72, -12}, string(projects[1]), false},
		{"unreadable old project", [2]int{-12, -12}, string(projects[2]), false},
	} {
		t.Run("prior availability/"+tt.name, func(t *testing.T) {
			for i, project := range projects {
				hours := -72
				if i < len(tt.createdHours) {
					hours = tt.createdHours[i]
				}
				if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET created_at=$3 WHERE organization_id=$1 AND id=$2`, org, project, to.Add(time.Duration(hours)*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.selected == string(projects[2]) {
				response := f.hosted.request(t, reader, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?project=all&scope=organization&range=24h&labels=no-matching-activity&projects="+url.QueryEscape(tt.selected), nil)
				if response.Code != http.StatusNotFound {
					t.Fatalf("unreadable prior status=%d body=%s", response.Code, response.Body.String())
				}
				return
			}
			out := read(url.Values{"range": {"24h"}, "labels": {"no-matching-activity"}, "projects": {tt.selected}})
			if out.PriorRecorded != tt.priorRecorded || out.Previous.Filed != 0 || out.Previous.Completed != 0 {
				t.Fatalf("prior_recorded=%t want=%t previous=%+v", out.PriorRecorded, tt.priorRecorded, out.Previous)
			}
		})
	}

	t.Run("reporter prior availability", func(t *testing.T) {
		reporter := f.hosted.user(t, "productivity_reporter", "reporter", "reporter@example.test", "", "")
		var principal string
		if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), reporter.identity.Subject)
			if err != nil {
				return err
			}
			principal = member.PrincipalID
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: projects[0]}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: principal, ProjectID: projects[0]})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET created_at=$3 WHERE organization_id=$1 AND id=$2`, org, projects[0], to.Add(-72*time.Hour)); err != nil {
			t.Fatal(err)
		}
		for i, tt := range []struct {
			name    string
			created *int
			own     bool
			want    bool
		}{
			{"old project without visible issues", nil, false, false},
			{"other principal prior issue", new(-40), false, false},
			{"own current issue", new(-12), true, false},
			{"own exact prior-end boundary", new(-24), true, false},
			{"own prior issue", new(-25), true, true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				if tt.created != nil {
					actor := tracker.Actor{Kind: "human", PrincipalID: "another-principal"}
					if tt.own {
						actor.PrincipalID = principal
					}
					productivitySeedItem(t.Context(), t, f.repository, content, org, projects[0], tracker.NativeWorkItemID(fmt.Sprintf("reporter_prior_%d", i)), int64(200+i), "Example reporter issue", "Todo", "", nil, actor, to.Add(time.Duration(*tt.created)*time.Hour), nil, productivitySeedItemFields{Labels: []string{"reporter-prior"}})
				}
				response := f.hosted.request(t, reporter, http.MethodGet, "/api/v2/organizations/"+string(org)+"/reports?project=all&scope=organization&range=24h&labels=no-matching-activity", nil)
				if response.Code != http.StatusOK {
					t.Fatalf("reporter report status=%d body=%s", response.Code, response.Body.String())
				}
				var out postgresOrganizationProductivity
				usageDecode(t, response, &out)
				if out.PriorRecorded != tt.want || out.Current.Filed != 0 || out.Previous.Filed != 0 || len(out.Projects) != 1 || out.Projects[0].ProjectID != string(projects[0]) {
					t.Fatalf("reporter prior recorded=%t want=%t current=%+v previous=%+v projects=%+v", out.PriorRecorded, tt.want, out.Current, out.Previous, out.Projects)
				}
			})
		}
	})
	t.Run("partial quality preserves report and complete projects", func(t *testing.T) {
		actor := tracker.Actor{Kind: "human", PrincipalID: "person_one"}
		state := "Todo"
		steps := make([]productivityTransition, 0, maxAnalyticsPopulation+1)
		for i := range maxAnalyticsPopulation + 1 {
			next := "In Progress"
			if state == "In Progress" {
				next = "Todo"
			}
			steps = append(steps, productivityTransition{to.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond), state, next, actor, ""})
			state = next
		}
		productivitySeedItem(t.Context(), t, f.repository, content, org, projects[0], "busy_quality", 400, "Example busy history", state, "bug", new(1), actor, to.Add(-2*time.Hour), steps)
		out := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
		if out.Current.Filed != 4 || out.Current.Completed != 2 || out.Current.Cancelled != 1 || out.Current.ChangesLanded == nil || *out.Current.ChangesLanded != 2 || out.Current.CompletionCost == nil || *out.Current.CompletionCost != 12 || out.Current.CycleP50 == nil || *out.Current.CycleP50 != 22.5*3600 {
			t.Fatalf("quality clipping changed available report facts: %+v", out.Current)
		}
		if out.Current.Worked != nil || out.Current.Reworked != nil || out.Current.ReworkRate != nil || !slices.Contains(out.Unavailable, "rework_timeline_unavailable") {
			t.Fatalf("partial quality presented as complete: current=%+v unavailable=%v", out.Current, out.Unavailable)
		}
		if out.Previous.Worked == nil || *out.Previous.Worked != 4 || out.Previous.Reworked == nil || *out.Previous.Reworked != 0 || out.Previous.ReworkRate == nil || *out.Previous.ReworkRate != 0 {
			t.Fatalf("complete prior quality lost: %+v", out.Previous)
		}
		for _, row := range out.Projects {
			switch row.ProjectID {
			case string(projects[0]):
				if row.Current.Worked != nil || row.Current.Reworked != nil || row.Current.ReworkRate != nil || row.Previous.Worked == nil || *row.Previous.Worked != 2 {
					t.Fatalf("busy project quality=%+v", row)
				}
			case string(projects[1]):
				if row.Current.Worked == nil || *row.Current.Worked != 1 || row.Current.Reworked == nil || *row.Current.Reworked != 1 || row.Current.ReworkRate == nil || *row.Current.ReworkRate != 100 || row.Previous.Worked == nil || *row.Previous.Worked != 2 {
					t.Fatalf("complete sibling quality lost: %+v", row)
				}
			default:
				t.Fatalf("unreadable project=%s", row.ProjectID)
			}
		}
		if len(out.Projects) != 2 {
			t.Fatalf("projects=%d want2", len(out.Projects))
		}
		t.Run("only prior quality incomplete", func(t *testing.T) {
			credential, status, err := service.hostedSessionCredential(t.Context(), auth.Session{Identity: reader.identity.Hosted, Email: reader.identity.Email}, apikey.HashToken(reader.token))
			if err != nil {
				t.Fatalf("reader credential status=%d: %v", status, err)
			}
			short, err := service.readOrganizationProductivityPostgres(t.Context(), credential, []string{string(projects[0]), string(projects[1])}, operatortool.AnalyticsWindow{From: to.Add(-30 * time.Minute), To: to}, reportsFilter{Labels: []string{"productivity"}})
			if err != nil {
				t.Fatal(err)
			}
			if short.Current.Worked == nil || *short.Current.Worked != 0 || short.Current.Reworked == nil || *short.Current.Reworked != 0 || short.Current.Filed != 0 || short.Current.Completed != 0 {
				t.Fatalf("complete current quality lost: %+v", short.Current)
			}
			if short.Previous.Worked != nil || short.Previous.Reworked != nil || short.Previous.ReworkRate != nil || !slices.Contains(short.Unavailable, "rework_timeline_unavailable") {
				t.Fatalf("incomplete prior quality presented as complete: previous=%+v unavailable=%v", short.Previous, short.Unavailable)
			}
			for _, row := range short.Projects {
				if row.Current.Worked == nil || *row.Current.Worked != 0 || row.Current.Reworked == nil || *row.Current.Reworked != 0 {
					t.Fatalf("complete current project quality lost: %+v", row)
				}
				if row.ProjectID == string(projects[1]) && (row.Previous.Worked == nil || *row.Previous.Worked != 0 || row.Previous.Reworked == nil || *row.Previous.Reworked != 0) {
					t.Fatalf("complete prior sibling quality lost: %+v", row)
				}
			}
		})
	})
}
func TestProductivityRunnerIntervals(t *testing.T) {
	t.Parallel()
	f := diagnosticsPostgresFixtures(t)[0]
	now := f.hosted.service.config.Clock().UTC().Truncate(time.Hour).Add(time.Hour)
	f.hosted.service.config.Clock = func() time.Time { return now }
	org, project := f.organization, f.hosted.project
	productivitySeedRunners(t.Context(), t, f.repository, org, now)
	content := eventstore.NewContent(f.details)
	intervals := []struct{ from, to time.Time }{
		{now.AddDate(-10, 0, 0), now.Add(-100 * time.Hour)},
		{now.AddDate(-5, 0, 0), now.Add(-90 * time.Hour)},
		{now.Add(-2*time.Hour + 10*time.Minute), now.Add(-2*time.Hour + 20*time.Minute)},
		{now.Add(-2*time.Hour + 40*time.Minute), now.Add(-2*time.Hour + 50*time.Minute)},
	}
	for i := range 2 {
		id := tracker.NativeWorkItemID(fmt.Sprintf("interval_%d", i))
		productivitySeedItem(t.Context(), t, f.repository, content, org, project, id, int64(i+1), "Interval issue", "Todo", "", nil, tracker.Actor{Kind: "human", PrincipalID: "person_one"}, now.Add(-2*time.Hour+30*time.Minute), nil)
		for j, span := range intervals {
			productivitySeedAttempt(t.Context(), t, f, project, id, fmt.Sprintf("interval_%d_%d", i, j), "runner_browser_preview", span.from, span.to)
		}
	}
	for range 2 {
		if err := f.repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			facts, err := r.(reportstore.AttemptReader).ReadAttemptFacts(t.Context(), reportstore.AttemptQuery{Projects: []tracker.ProjectID{project}, From: intervals[0].from.Add(-time.Second), To: now, Limit: 20, Filter: reportstore.Filter{Runners: []string{"runner_browser_preview"}}})
			if err != nil {
				return err
			}
			for i := range 2 {
				selected := []reportstore.AttemptFact{}
				for _, fact := range facts {
					if fact.WorkItemID == tracker.NativeWorkItemID(fmt.Sprintf("interval_%d", i)) {
						selected = append(selected, fact)
					}
				}
				if len(selected) != 4 {
					t.Fatalf("item%d intervals=%+v want4 recorded attempts", i, selected)
				}
				first, last := selected[0].StartedAt, selected[0].UpdatedAt
				for _, fact := range selected {
					if fact.StartedAt.Before(first) {
						first = fact.StartedAt
					}
					if fact.UpdatedAt.After(last) {
						last = fact.UpdatedAt
					}
				}
				if !first.Equal(intervals[0].from) || !last.Equal(intervals[3].to) {
					t.Fatalf("item%d interval endpoints first=%v last=%v", i, first, last)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	credential, status, err := f.hosted.service.hostedSessionCredential(t.Context(), auth.Session{Identity: f.owner.identity.Hosted, Email: f.owner.identity.Email}, apikey.HashToken(f.owner.token))
	if err != nil {
		t.Fatalf("credential status=%d: %v", status, err)
	}
	for _, tt := range []struct {
		name  string
		end   time.Duration
		filed int
	}{
		{"gap", 35 * time.Minute, 0}, {"overlap", 45 * time.Minute, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := operatortool.AnalyticsWindow{From: now.Add(-2*time.Hour + 30*time.Minute), To: now.Add(-2*time.Hour + tt.end)}
			report, err := f.hosted.service.readOrganizationProductivityPostgres(t.Context(), credential, []string{string(project)}, w, reportsFilter{Runners: []string{"runner_browser_preview"}})
			if err != nil {
				t.Fatal(err)
			}
			if report.Current.Filed != tt.filed {
				t.Fatalf("filed=%d want=%d", report.Current.Filed, tt.filed)
			}
		})
	}
}
func assertProductivitySums(t *testing.T, out postgresOrganizationProductivity) {
	t.Helper()
	for _, previous := range []bool{false, true} {
		sum := postgresProductivityMetrics{ChangesLanded: new(int), Worked: new(int), Reworked: new(int)}
		total := out.Current
		if previous {
			total = out.Previous
		}
		for _, row := range out.Projects {
			m := row.Current
			if previous {
				m = row.Previous
			}
			sum.Filed += m.Filed
			sum.Completed += m.Completed
			sum.Cancelled += m.Cancelled
			if m.ChangesLanded == nil {
				t.Fatal("landed count unavailable")
			}
			*sum.ChangesLanded += *m.ChangesLanded
			sum.NetBacklog += m.NetBacklog
			if m.Worked == nil || m.Reworked == nil {
				t.Fatal("complete fixture quality counts unavailable")
			}
			*sum.Worked += *m.Worked
			*sum.Reworked += *m.Reworked
		}
		if sum.Filed != total.Filed || sum.Completed != total.Completed || sum.Cancelled != total.Cancelled || sum.NetBacklog != total.NetBacklog || total.ChangesLanded == nil || *sum.ChangesLanded != *total.ChangesLanded || total.Worked == nil || *sum.Worked != *total.Worked || total.Reworked == nil || *sum.Reworked != *total.Reworked {
			t.Fatalf("project counts=%+v totals=%+v", sum, total)
		}
		series := out.Series
		if previous {
			series = out.PreviousSeries
		}
		var filed, completed int
		for _, bucket := range series {
			var split productivityCount
			for _, counts := range bucket.Projects {
				split.Filed += counts.Filed
				split.Completed += counts.Completed
			}
			if split.Filed != bucket.Filed || split.Completed != bucket.Completed {
				t.Fatalf("bucket split=%+v bucket=%+v", split, bucket)
			}
			filed += bucket.Filed
			completed += bucket.Completed
		}
		if filed != total.Filed || completed != total.Completed {
			t.Fatalf("series filed=%d completed=%d totals=%+v", filed, completed, total)
		}
	}
}

func TestOrganizationProductivityTenant(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}})
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "foreign")
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	for _, fixture := range []nativeFixture{f, foreign} {
		productivitySeedItem(t.Context(), t, f.service.config.TenantRepository, eventstore.NewContent(f.service.runDetailStore()), fixture.project.OrganizationID, fixture.project.ID, tracker.NativeWorkItemID("wi_"+string(fixture.project.ID)), 1, fixture.project.Name, "Todo", "", nil, tracker.Actor{Kind: "human", PrincipalID: "person_one"}, now, nil)
	}

	f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatalf("credential status=%d: %v", status, err)
	}
	report, err := f.service.readOrganizationProductivityPostgres(t.Context(), credential, []string{string(f.project.ID), string(foreign.project.ID)}, operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)}, reportsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Current.Filed != 1 {
		t.Fatalf("foreign organization counted: %+v", report.Current)
	}
}

type productivityTransition struct {
	at       time.Time
	from, to string
	actor    tracker.Actor
	reason   string
}

type productivitySeedItemFields struct {
	Body   string
	Labels []string
}

func productivitySeedItem(ctx context.Context, t testing.TB, repository tenantstore.Repository, content *eventstore.Content, org tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID, number int64, title, state, kind string, priority *int, actor tracker.Actor, created time.Time, steps []productivityTransition, options ...productivitySeedItemFields) {
	t.Helper()
	events := []eventstore.Event{}
	prepare := func(value tracker.CollaborationEvent) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(ctx, org, raw)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	prepare(tracker.CollaborationEvent{ID: string(id) + "_created", OrganizationID: org, ProjectID: project, AggregateID: id, Type: "issue.created", SchemaVersion: 1, Actor: actor, RecordedAt: created, Data: tracker.CollaborationData{Revision: 1, ToState: "Todo"}})
	for i, step := range steps {
		prepare(tracker.CollaborationEvent{ID: fmt.Sprintf("%s_transition_%d", id, i), OrganizationID: org, ProjectID: project, AggregateID: id, Type: "workflow.transitioned", SchemaVersion: 1, Actor: step.actor, RecordedAt: step.at, Data: tracker.CollaborationData{Revision: tracker.Revision(i + 2), FromState: step.from, ToState: step.to, ReasonDetail: step.reason}})
	}
	body := nativeContractTestBody
	labels := []string{"productivity"}
	if kind != "" {
		labels = append(labels, kind)
	}
	if len(options) > 0 {
		if options[0].Body != "" {
			body = options[0].Body
		}
		if options[0].Labels != nil {
			labels = options[0].Labels
		}
	}
	revision := int64(len(steps) + 1)
	raw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: int(number), Revision: tracker.Revision(revision), Profile: "native"}, Title: title, Body: body, State: state, Type: kind, Priority: priority, Labels: labels, Actor: actor, CreatedAt: created, UpdatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: revision}, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := content.PreparePayload(ctx, org, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: number, Revision: revision, BodyObjectKey: &bodyRef.Key, Title: title, WorkflowStateName: &state, IssueType: kind, Priority: priority, ActorKind: &actor.Kind, ActorPrincipalID: &actor.PrincipalID, CreatedAt: created, UpdatedAt: created, NativeCreatedAt: created, NativeUpdatedAt: created})
		if err != nil {
			return err
		}
		if err := w.SetWorkItemLabels(ctx, project, id, labels); err != nil {
			return err
		}
		if err := w.PutWorkItemFacts(ctx, claimstore.WorkItemFacts{OrganizationID: org, ProjectID: project, WorkItemID: id, Author: actor.PrincipalID}); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		if err := w.BindDoctorProposalIdentities(ctx, corestore.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: project, WorkItemID: id, Revision: revision, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(body)}); err != nil {
			return err
		}
		for _, event := range events {
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func productivitySeedRunners(ctx context.Context, t testing.TB, repository tenantstore.Repository, org tracker.OrganizationID, now time.Time) {
	t.Helper()
	if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, runner := range []struct{ id, name string }{{"runner_browser_preview", "Example runner"}, {"runner_browser_preview_shared", "Shared runner"}} {
			machine := tracker.MachineID("machine_" + runner.id)
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: org, ID: string(machine), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			expires := now.Add(time.Hour)
			if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: org, ID: runner.id, Name: runner.name, Hash: apikey.HashToken(string(org) + runner.id), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
				return err
			}
			enrollment := runnerstore.Enrollment{OrganizationID: org, ID: "enrollment_" + runner.id, RunnerID: runner.id, MachineID: machine, TokenHash: apikey.HashToken("enrollment_" + string(org) + runner.id), Scope: "organization", Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example"}
			if err := w.CreateRunnerEnrollment(ctx, enrollment); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(ctx, enrollment.ID, runner.id, machine, now); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: org, RunnerID: runner.id, MachineID: machine, TokenID: runner.id, EnrollmentID: enrollment.ID, Scope: "organization", Operations: enrollment.Operations, DisplayName: runner.name, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func productivitySeedAttempt(ctx context.Context, t testing.TB, f diagnosticsPostgresFixture, project tracker.ProjectID, id tracker.NativeWorkItemID, attempt, runner string, started, updated time.Time) {
	t.Helper()
	var lease leasestore.Lease
	machineRunner := runner
	if machineRunner == "" {
		machineRunner = "runner_browser_preview"
	}
	if err := f.repository.WriteTenant(ctx, f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(ctx, project, id)
		if err != nil {
			return err
		}
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_" + machineRunner, SessionID: attempt + "_session", LeaseID: attempt + "_lease", TTL: time.Hour})
		if err != nil {
			return err
		}
		if runner != "" {
			return w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: f.organization, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: started})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.details)
	run := tracker.NativeRunData{AttemptID: attempt, RunID: attempt + "_run", Sequence: 1, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: tracker.MachineID(lease.MachineID), RunnerID: runner, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "code"}}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := content.PreparePayload(ctx, f.organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	actor := tracker.Actor{Kind: "runner", PrincipalID: runner}
	if runner == "" {
		actor = tracker.Actor{Kind: "system", PrincipalID: "example-history-import"}
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: attempt + "_finished", OrganizationID: f.organization, ProjectID: project, AggregateID: id, Type: "run.finished", SchemaVersion: 1, Actor: actor, RecordedAt: updated, Data: tracker.CollaborationData{Run: &run}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(ctx, f.organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
	if err := f.repository.WriteTenant(ctx, f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: attempt, RunID: run.RunID, Fence: fence, Sequence: 1, Status: "succeeded", StartedAt: started, PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		_, err := w.Release(ctx, fence, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_attempts SET updated_at=$3 WHERE organization_id=$1 AND id=$2`, f.organization, attempt, updated); err != nil {
		t.Fatal(err)
	}
}

func productivitySeedLanding(ctx context.Context, t testing.TB, repository tenantstore.Repository, content *eventstore.Content, org tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID, at time.Time) {
	t.Helper()
	changeID, versionID := "change_"+string(id), "version_"+string(id)
	landing := tracker.ChangeLanding{VersionID: versionID, HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40), BaseRef: "refs/heads/develop", LandedAt: at}
	value := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: project, WorkItemID: id, Title: "Example change", CreatedAt: at, UpdatedAt: at, Landed: &landing}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	change, err := content.PrepareChange(ctx, org, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: landing.HeadSHA, PolicyID: "example-policy"}, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareVersion(ctx, org, raw)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := content.PrepareVersionLanding(ctx, eventstore.VersionLanding{OrganizationID: org, ProjectID: project, WorkItemID: id, ChangeID: changeID, VersionID: versionID}, landing)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(ctx, change); err != nil {
			return err
		}
		if _, err := w.CreateVersion(ctx, version); err != nil {
			return err
		}
		return w.RecordVersionLanding(ctx, frozen)
	}); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkOrganizationProductivity(b *testing.B) {
	ctx, f, service, _, project, now := benchmarkPostgresDiagnostics(b)
	org := f.organization
	content := eventstore.NewContent(f.details)
	projects := []string{string(project)}
	token := hubapi.NewID("productivity_benchmark")
	if err := f.repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: org, ID: token, Name: "Example report reader", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for i := range 7 {
			id := tracker.ProjectID(fmt.Sprintf("summary-project-%d", i))
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			projects = append(projects, string(id))
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	credential, status, err := service.authenticateTenantAPIHash(ctx, org, apikey.HashToken(token), "", "")
	if err != nil {
		b.Fatalf("credential status=%d: %v", status, err)
	}
	productivitySeedRunners(ctx, b, f.repository, org, now)
	for i := range 2000 {
		selected := tracker.ProjectID(projects[i%len(projects)])
		id := tracker.NativeWorkItemID(fmt.Sprintf("summary-item-%d", i))
		created, closed := now.AddDate(0, 0, -i%90), now.Add(-time.Duration(i%720)*time.Hour)
		if !created.Before(closed) {
			created = closed.Add(-time.Hour)
		}
		productivitySeedItem(ctx, b, f.repository, content, org, selected, id, int64(i+1), fmt.Sprintf("Summary benchmark %d", i), "Done", "bug", new(1), tracker.Actor{Kind: "human", PrincipalID: "person"}, created, []productivityTransition{{closed, "Todo", "Done", tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview"}, ""}})
		attempt := "summary_attempt_" + string(id)
		productivitySeedAttempt(ctx, b, f, selected, id, attempt, "runner_browser_preview", closed.Add(-48*time.Hour), closed)
		usage := make([]reportstore.UsageRecord, 0, 48)
		for hour := range 48 {
			usage = append(usage, reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: selected, WorkItemID: id, AttemptID: attempt, Period: now.Add(-time.Duration(i%720+hour) * time.Hour), Provider: "provider", Model: "model", Cost: 1, Currency: "USD"}, UpdatedAt: now})
		}
		if err := f.repository.ImportReportUsageFacts(ctx, usage); err != nil {
			b.Fatal(err)
		}
		if err := f.repository.VerifyReportUsageImport(ctx, org, attempt, strings.Repeat("a", 64), 48); err != nil {
			b.Fatal(err)
		}
	}
	w := operatortool.AnalyticsWindow{From: now.AddDate(0, 0, -30), To: now, Bucket: 24 * time.Hour}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		report, err := service.readOrganizationProductivityPostgres(ctx, credential, projects, w, reportsFilter{})
		if err != nil {
			b.Fatal(fmt.Errorf("read summary: %w", err))
		}
		if len(report.Projects) != 8 {
			b.Fatalf("projects=%d want8", len(report.Projects))
		}
	}
}
