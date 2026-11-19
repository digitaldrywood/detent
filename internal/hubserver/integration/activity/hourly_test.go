package activity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/labstack/echo/v4"
)

func TestHourlyActivity(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	to := now.Truncate(time.Hour)
	from := to.Add(-2 * time.Hour)
	f := newActivityPostgresFixture(t, "org_activity_hourly", now)
	newIssue := func(title string) corestore.WorkItem {
		clock := f.now
		f.now = from
		defer func() { f.now = clock }()
		return f.issue(t, f.project, title)
	}
	issue := newIssue("Example hourly item")
	runtime := &tracker.NativeRuntimeObservation{Phase: "implementation"}
	early := f.storeAttempt(t, issue, "before_window", "failed", from.Add(-2*time.Minute), from.Add(-time.Minute), runtime, true)
	f.storeAttempt(t, issue, "at_lower_bound", "failed", from.Add(-time.Minute), from, runtime, true)
	landed := f.storeAttempt(t, issue, "at_hour_boundary", "succeeded", from.Add(58*time.Minute), from.Add(time.Hour), runtime, true)
	f.storeAttempt(t, issue, "at_upper_bound", "cancelled", to.Add(-time.Minute), to, runtime, true)

	land := func(change string, source, settlement activityFixtureAttempt, at time.Time, replay bool) {
		t.Helper()
		prep := eventstore.NewContent(f.service.runDetailStore())
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			raw, err := json.Marshal(tracker.ChangeRequest{ID: change, OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: settlement.issue.NativeID, Title: "Example change", Revision: 1, CreatedAt: source.attempt.StartedAt, UpdatedAt: source.attempt.StartedAt})
			if err != nil {
				return err
			}
			row, err := prep.PrepareChange(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateChange(t.Context(), row); err != nil {
				return err
			}
			policy, err := w.ProjectPolicy(t.Context(), issue.ProjectID)
			if err != nil {
				return err
			}
			sha, merge := strings.Repeat("a", 40), strings.Repeat("b", 40)
			raw, err = json.Marshal(tracker.ChangeVersion{ID: "version_" + change, ChangeID: change, Number: 1, CreatedAt: source.attempt.UpdatedAt, Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{AttemptID: source.attempt.ID, HeadSHA: sha, PolicyID: policy.PolicyID, Repository: "example-org/example-project"}})
			if err != nil {
				return err
			}
			version, err := prep.PrepareVersion(t.Context(), f.organization, raw)
			if err != nil {
				return err
			}
			if _, err := w.CreateVersion(t.Context(), version); err != nil {
				return err
			}
			value := tracker.NativeLandingReceipt{ChangeID: change, VersionID: version.ID, HeadSHA: sha, BaseRef: "main", MergeSHA: merge, Landed: true, ObservedAt: at}
			proof, err := prep.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: f.organization, AttemptID: settlement.attempt.ID, VersionID: version.ID, Repository: version.Repository, BaseRef: value.BaseRef}, value)
			if err != nil {
				return err
			}
			if _, err := w.CreateLandingReceipt(t.Context(), proof); err != nil {
				return err
			}
			if replay {
				if _, err := w.CreateLandingReceipt(t.Context(), proof); err != nil {
					return err
				}
			}
			frozen, err := prep.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: f.organization, ProjectID: issue.ProjectID, WorkItemID: settlement.issue.NativeID, ChangeID: change, VersionID: version.ID}, tracker.ChangeLanding{VersionID: version.ID, LandedAt: at, HeadSHA: sha, MergeSHA: merge, BaseRef: value.BaseRef, Method: "merge", Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}})
			if err != nil {
				return err
			}
			return w.RecordVersionLanding(t.Context(), frozen)
		}); err != nil {
			t.Fatal(err)
		}
	}
	land("hourly_landed", early, landed, from.Add(time.Hour), true)
	missing := newIssue("Example missing history")
	missingLanding := f.storeAttempt(t, missing, "without_run_history", "succeeded", from.Add(70*time.Minute), from.Add(72*time.Minute), nil, false)
	land("hourly_missing", missingLanding, missingLanding, from.Add(72*time.Minute), false)
	noChange := newIssue("Example accepted no-change")
	acceptedAt := from.Add(32 * time.Minute)
	accepted := &tracker.NativeRuntimeObservation{Phase: "completed", Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: true, ObservedAt: acceptedAt}}
	f.storeAttempt(t, noChange, "accepted_no_change", "failed", acceptedAt.Add(-time.Minute), acceptedAt, accepted, true,
		activityAttemptOptions{finalization: &tracker.NativeFinalization{Settled: true, NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: strings.Repeat("c", 40)}}})
	unaccepted := *accepted
	unaccepted.Completion = &tracker.NativeCompletionObservation{ObservedAt: acceptedAt}
	f.storeAttempt(t, noChange, "unaccepted_no_change", "succeeded", acceptedAt.Add(-time.Minute), acceptedAt, &unaccepted, true,
		activityAttemptOptions{finalization: &tracker.NativeFinalization{Settled: true, NoChange: &tracker.NativeNoChangeCompletion{VerifiedSHA: strings.Repeat("c", 40)}}})

	read := func(t *testing.T, project, account string, start, end time.Time, wantStatus int) map[string]json.RawMessage {
		t.Helper()
		q := url.Values{"hourly": {"true"}, "project_id": {project}, "from": {start.Format(time.RFC3339Nano)}, "to": {end.Format(time.RFC3339Nano)}, "limit": {"1"}}
		response := f.page(t, account, "/api/v2/organizations/"+string(f.organization)+"/activity?"+q.Encode())
		if response.Code != wantStatus {
			t.Fatalf("HTTP %d, want %d: %s", response.Code, wantStatus, response.Body.String())
		}
		var report map[string]json.RawMessage
		usageDecode(t, response, &report)
		return report
	}
	t.Run("half-open hours and independent drill-down limit", func(t *testing.T) {
		report := read(t, f.project, "viewer", from, to, http.StatusOK)
		var hours []operatortool.ActivityHour
		if err := json.Unmarshal(report["hours"], &hours); err != nil {
			t.Fatal(err)
		}
		var ids []struct {
			AttemptID string `json:"attempt_id"`
		}
		if err := json.Unmarshal(report["attempts"], &ids); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].AttemptID != "without_run_history" || string(report["more_attempts"]) != "true" {
			t.Fatalf("attempt drill-down = %s", report["attempts"])
		}
		if len(hours) != 2 || !hours[0].Hour.Equal(from) || !hours[1].Hour.Equal(from.Add(time.Hour)) || hours[0].TerminalAttempts != 3 || hours[1].TerminalAttempts != 2 || hours[0].ElapsedAttemptSeconds != 180 || hours[1].ElapsedAttemptSeconds != 240 {
			t.Fatalf("hourly attempts = %+v", hours)
		}
	})
	t.Run("sub-microsecond edges preserve the half-open window", func(t *testing.T) {
		report := read(t, f.project, "viewer", from.Add(time.Nanosecond), from.Add(time.Hour+time.Nanosecond), http.StatusOK)
		var hours []operatortool.ActivityHour
		if err := json.Unmarshal(report["hours"], &hours); err != nil {
			t.Fatal(err)
		}
		if len(hours) != 2 || hours[0].TerminalAttempts != 2 || hours[1].TerminalAttempts != 1 || hours[1].VerifiedLandings != 1 {
			t.Fatalf("sub-microsecond activity = %s", report["hours"])
		}
	})
	t.Run("verified landings, earlier attempts and history coverage", func(t *testing.T) {
		report := read(t, f.project, "viewer", from, to, http.StatusOK)
		var hours []struct {
			VerifiedLandings int64 `json:"verified_landings"`
			LandingHistory   []struct {
				LinkedAttempts  int64  `json:"linked_attempts"`
				HistoryCoverage string `json:"history_coverage"`
				Landings        int64  `json:"landings"`
			} `json:"landing_history"`
		}
		if err := json.Unmarshal(report["hours"], &hours); err != nil {
			t.Fatal(err)
		}
		var ids []struct {
			LandingID        string `json:"landing_id"`
			LandingAttemptID string `json:"landing_attempt_id"`
		}
		if err := json.Unmarshal(report["landings"], &ids); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].LandingID != "version_hourly_missing" || ids[0].LandingAttemptID != "without_run_history" || string(report["more_landings"]) != "true" {
			t.Fatalf("landing drill-down = %s", report["landings"])
		}
		if len(hours) != 2 || hours[0].VerifiedLandings != 0 || hours[1].VerifiedLandings != 2 || len(hours[1].LandingHistory) != 2 ||
			hours[1].LandingHistory[0].LinkedAttempts != 1 || hours[1].LandingHistory[0].HistoryCoverage != "partial" || hours[1].LandingHistory[0].Landings != 1 ||
			hours[1].LandingHistory[1].LinkedAttempts != 3 || hours[1].LandingHistory[1].HistoryCoverage != "complete" || hours[1].LandingHistory[1].Landings != 1 ||
			string(report["attempts_per_landing"]) != "2.5" {
			t.Fatalf("landing histories = %s", report["hours"])
		}
	})
	t.Run("accepted no-change separately", func(t *testing.T) {
		report := read(t, f.project, "viewer", from, to, http.StatusOK)
		var hours []struct {
			AcceptedNoChange    int64 `json:"accepted_no_change"`
			UnavailableVerdicts int64 `json:"unavailable_verdicts"`
		}
		if err := json.Unmarshal(report["hours"], &hours); err != nil {
			t.Fatal(err)
		}
		if len(hours) != 2 || hours[0].AcceptedNoChange != 1 || hours[1].AcceptedNoChange != 0 || hours[0].UnavailableVerdicts != 3 || hours[1].UnavailableVerdicts != 2 {
			t.Fatalf("no-change and unavailable counts = %s", report["hours"])
		}
	})
	t.Run("zero landings", func(t *testing.T) {
		report := read(t, f.project, "viewer", from, from.Add(time.Hour), http.StatusOK)
		if string(report["attempts_per_landing"]) != "null" {
			t.Fatalf("zero landing ratio = %s", report["attempts_per_landing"])
		}
	})
	t.Run("MCP matches HTTP", func(t *testing.T) {
		request := f.page(t, "viewer", "/api/v2/organizations/"+string(f.organization)+"/activity?hourly=true&project_id="+f.project+"&from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339)+"&limit=1")
		browserHostedStatus(t, request, http.StatusOK)
		mcpRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		mcpRequest.AddCookie(f.cookies["viewer"])
		c := f.service.echo.NewContext(mcpRequest, httptest.NewRecorder())
		c.SetPath("/mcp")
		var ctx context.Context
		if err := f.service.operatorAuthority(func(c echo.Context) error { ctx = c.Request().Context(); return nil })(c); err != nil {
			t.Fatal(err)
		}
		args, err := json.Marshal(map[string]any{"hourly": true, "project_id": f.project, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.service.executeActivityRead(ctx, operatortool.Call{Name: operatortool.Activity, Arguments: args})
		if err != nil || !jsonEqualActivity(result.Content, request.Body.Bytes()) {
			t.Fatalf("MCP = %s, HTTP = %s, error = %v", result.Content, request.Body.Bytes(), err)
		}
	})
	t.Run("same project and attempt IDs in two tenants", func(t *testing.T) {
		other := seedHourlyActivityTenant(t, f, issue.ProjectID, from)
		for _, org := range []tracker.OrganizationID{f.organization, other} {
			if err := f.repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(activitystore.HourlyReader)
				if !ok {
					t.Fatal("hourly activity owner unavailable")
				}
				hourly, err := reader.ActivityHourly(t.Context(), activitystore.HourlyFilter{Project: issue.ProjectID, From: from, To: from.Add(time.Second), Limit: 1})
				if err != nil {
					return err
				}
				want := float64(60)
				if org == other {
					want = 15
				}
				if len(hourly.Outcomes) != 1 || hourly.Outcomes[0].Attempts != 1 || hourly.Outcomes[0].Seconds != want ||
					len(hourly.Attempts) != 1 || hourly.Attempts[0].ID != "at_lower_bound" {
					t.Fatalf("%s hourly activity = %+v", org, hourly)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("typed verdict and pending", func(t *testing.T) {
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET verdict_outcome='pending' WHERE organization_id=$1 AND id='without_run_history'`, f.organization); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET verdict_outcome='reroute',verdict_success=false,verdict_reason='Acceptance receipt missing',verdict_next_step='acceptance',verdict_attempted_step='implementation' WHERE organization_id=$1 AND id='at_lower_bound'`, f.organization); err != nil {
			t.Fatal(err)
		}
		report := read(t, f.project, "viewer", from, to, http.StatusOK)
		var hours []operatortool.ActivityHour
		if err := json.Unmarshal(report["hours"], &hours); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, outcome := range hours[0].Outcomes {
			if outcome.Verdict == "reroute" && outcome.Attempts == 1 && outcome.EndingStage == "implementation" &&
				outcome.Outcome == "failed" && outcome.VerdictSucceeded != nil && !*outcome.VerdictSucceeded && outcome.Reason == "Acceptance receipt missing" && outcome.NextStep == "acceptance" {
				found = true
			}
		}
		if !found || hours[0].UnavailableVerdicts != 2 || hours[1].PendingVerdicts != 1 || hours[1].UnavailableVerdicts != 1 {
			t.Fatalf("verdicts = %s", report["hours"])
		}
	})

	t.Run("project grant", func(t *testing.T) {
		read(t, f.privateProject, "viewer", from, to, http.StatusForbidden)
	})
}

func seedHourlyActivityTenant(t *testing.T, f *activityPostgresFixture, project tracker.ProjectID, finished time.Time) tracker.OrganizationID {
	t.Helper()
	const org = tracker.OrganizationID("org_activity_other")
	const machine = "example_other_machine"
	var item corestore.WorkItem
	if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example other", Slug: "example-other", CreatedAt: finished}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example other project", Slug: "example-other", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: machine, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		var err error
		item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: "other_item", Number: 1, Title: "Example item", CreatedAt: finished.Add(-time.Hour), UpdatedAt: finished, NativeCreatedAt: finished.Add(-time.Hour), NativeUpdatedAt: finished})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	other := *f
	other.organization = org
	started := finished.Add(-15 * time.Second)
	lease := leasestore.Lease{OrganizationID: org, LeaseID: "lease_at_lower_bound", FencingToken: 1, IssueID: item.ID, MachineID: machine, SessionID: "session_at_lower_bound", WorkItemRevision: item.Revision, AcquiredAt: started, RenewedAt: started, ExpiresAt: finished.Add(time.Hour), ReleasedAt: &finished}
	if err := other.repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: other.receipt(t, "core-lease", lease.LeaseID, lease)}); err != nil {
		t.Fatal(err)
	}
	ref, err := other.service.runDetailStore().StoreDetail(t.Context(), string(org), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	attempt := leasestore.Attempt{OrganizationID: org, ID: "at_lower_bound", IssueID: item.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: "at_lower_bound", Sequence: 1, Status: "failed", WorkItemRevision: item.Revision, PayloadObjectKey: ref.Key, StartedAt: started, UpdatedAt: finished}
	if err := other.repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: other.receipt(t, "core-attempt", attempt.ID, attempt)}); err != nil {
		t.Fatal(err)
	}
	return org
}
