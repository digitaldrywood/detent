package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsQualityEvidencePopulationOrder(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}})
	scope := analyticsTestScope(t, f)
	now := f.service.config.now().UTC().Truncate(time.Hour)
	content := eventstore.NewContent(f.service.runDetailStore())
	for index, name := range []string{"first", "second"} {
		issue := seedAnalyticsTestWorkItem(t, f.service, scope.Organization, scope.Project, "quality-"+name, index+1, name, "Todo", now.Add(-time.Hour))
		change, version := seedAnalyticsTestChange(t, f.service, scope, issue.WorkItemID, "change-"+name, now.Add(-time.Hour), nil)
		for number := 1; number <= maxAnalyticsPopulation+2; number++ {
			review := tracker.ChangeReview{ID: fmt.Sprintf("%s-%d", name, number), VersionID: version.ID, Decision: "changes_requested", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now.Add(-time.Duration(number) * time.Second)}
			row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, Kind: "review", Sequence: int64(number*2 - 1 + index)}, historyTestJSON(t, review))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now, Bucket: time.Hour}
	token := ""
	for _, test := range []struct {
		offset, count int
		first, last   string
		partial       bool
	}{
		{0, 1000, "first-1", "second-500", true},
		{1000, 1000, "first-501", "second-1000", true},
		{2000, 4, "first-1001", "second-1002", true},
	} {
		t.Run(fmt.Sprintf("offset-%d", test.offset), func(t *testing.T) {
			ctx, paging, err := analyticsPagingContext(t.Context(), token)
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := f.service.readAnalyticsQualityInputsTenant(ctx, scope, w)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, event := range inputs.Events {
				if event.Kind == "changes_requested" {
					ids = append(ids, event.ID)
				}
			}
			if len(ids) != test.count || inputs.Partial != test.partial {
				t.Fatalf("evidence count=%d partial=%v", len(ids), inputs.Partial)
			}
			if ids[0] != test.first || ids[len(ids)-1] != test.last {
				t.Fatalf("evidence page begins %s and ends %s", ids[0], ids[len(ids)-1])
			}
			token = paging.Next["quality_evidence"]
			if (token != "") != (test.offset < 2000) {
				t.Fatalf("evidence continuation=%v", paging.Next)
			}
		})
	}
}

func TestQualityEscapeApplicationAndRead(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	details := &memoryRunDetails{}
	f := newChangeFixture(t, newPostgresNativeService(t, Config{now: func() time.Time { return now }, RunDetails: details}))
	scope := analyticsTestScope(t, f.nativeFixture)
	version := f.publish(t, "v1", "")
	landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("d", 40), BaseRef: "develop", LandedAt: now.Add(-time.Hour), Quality: &tracker.LandingQualitySnapshot{IssueBody: "Must accept valid input", Criteria: []string{"Accept valid input"}, Evidence: []gate.CriterionEvidence{}}}
	frozenRow, err := eventstore.NewContent(details).PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: f.issue.WorkItemID, ChangeID: f.change.ID, VersionID: version.ID}, landing)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RecordVersionLanding(t.Context(), frozenRow) }); err != nil {
		t.Fatal(err)
	}
	assessment := tracker.QualityEscape{OccurrenceID: strings.Repeat("e", 40), Kind: "revert", ObservedAt: now.Add(-30 * time.Minute), Cause: "missing_criterion", EvidenceReference: "revert_commit", EvidenceQuote: "This reverts commit " + landing.MergeSHA, BasisQuote: "Must accept valid input"}
	for _, tt := range []struct {
		name   string
		mutate func(*tracker.QualityEscape)
		status int
	}{
		{"valid", func(*tracker.QualityEscape) {}, http.StatusOK},
		{"same occurrence", func(*tracker.QualityEscape) {}, http.StatusOK},
		{"invalid cause", func(e *tracker.QualityEscape) { e.Cause = "unknown" }, http.StatusUnprocessableEntity},
		{"fabricated basis", func(e *tracker.QualityEscape) { e.BasisQuote = "fake" }, http.StatusUnprocessableEntity},
		{"missing instance", func(e *tracker.QualityEscape) { e.Cause = "infrastructure" }, http.StatusUnprocessableEntity},
		{"fake human override", func(e *tracker.QualityEscape) { e.HumanOverride = true }, http.StatusUnprocessableEntity},
		{"revert targets wrong change", func(e *tracker.QualityEscape) { e.EvidenceQuote = "reverted other commit" }, http.StatusUnprocessableEntity},
		{"future", func(e *tracker.QualityEscape) { e.ObservedAt = now.Add(time.Hour) }, http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			escape := assessment
			tt.mutate(&escape)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: tt.name}, VersionID: version.ID, Body: "Read-only assessment", Escape: &escape})
			requireNativeStatus(t, response, tt.status)
		})
	}

	scheduled := gate.ScheduledEvidence{Schema: 1, Repository: "example/repo", OccurrenceKey: strings.Repeat("f", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Lint", RunURL: "https://github.com/example/repo/actions/runs/123/attempts/1", JobURL: "https://github.com/example/repo/actions/jobs/456", HeadSHA: landing.MergeSHA, Conclusion: "failure", Checks: []gate.CheckObservation{}}
	suiteEscape := tracker.QualityEscape{OccurrenceID: scheduled.OccurrenceKey, Kind: "scheduled_failure", ObservedAt: now.Add(-20 * time.Minute), Cause: "infrastructure", InstanceID: "github-actions", EvidenceReference: scheduled.RunURL, EvidenceQuote: "Backend startup failed", BasisQuote: "Backend startup failed"}
	request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "unrecorded-scheduled"}, VersionID: version.ID, Body: "Infrastructure assessment", Escape: &suiteEscape}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusUnprocessableEntity)
	body := issueorigin.Stamp("Backend startup failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: strings.TrimSuffix(scheduled.RunURL, "/attempts/1"), Fingerprint: strings.Repeat("a", 64)})
	replaceAnalyticsTestIssueBody(t, f.service, scope, f.issue.WorkItemID, body)
	request.IdempotencyKey = "recorded-scheduled"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusOK)
	request.IdempotencyKey = "changed-scheduled-source"
	injected := false
	request.Body = "Source edit race"
	details.afterStore = func(ref attachment.ArchiveReference) {
		details.mu.Lock()
		raw := append([]byte(nil), details.data[ref.Key]...)
		details.mu.Unlock()
		var discussion tracker.ChangeDiscussion
		if json.Unmarshal(raw, &discussion) != nil || discussion.Body != request.Body || discussion.Escape == nil {
			return
		}
		if held := f.service.config.TenantRepository.(*postgres.Repository).Stats().AcquiredConns(); held != 0 {
			t.Fatalf("quality preparation retained %d PostgreSQL connections", held)
		}
		injected = true
		details.afterStore = nil
		replaceAnalyticsTestIssueBody(t, f.service, scope, f.issue.WorkItemID, "Evidence withdrawn")
	}
	defer func() { details.afterStore = nil }()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request)
	requireNativeStatus(t, response, http.StatusConflict)
	if !injected {
		t.Fatal("scheduled evidence edit was not injected")
	}
	row, err := f.service.readChangeVersionRecord(t.Context(), scope.Organization, f.change.ID, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := f.service.hydrateChangeVersion(t.Context(), scope, f.issue.WorkItemID, row)
	if err != nil || frozen.LandingQuality == nil || frozen.LandingQuality.IssueBody != landing.Quality.IssueBody {
		t.Fatalf("later issue edits changed landing classification context: %+v, %v", frozen.LandingQuality, err)
	}
	w := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Hour), To: now.Add(time.Second), Bucket: time.Hour}
	got, err := f.service.readAnalyticsQualityTenant(t.Context(), scope, operatortool.AnalyticsRequest{Limit: 10}, w)
	if err != nil || got.Escapes != 2 || got.LandedVersions != 1 || got.Causes["infrastructure"] != 1 || got.Causes["missing_criterion"] != 1 || got.EscapePercent == nil || *got.EscapePercent != 100 {
		t.Fatalf("quality read = %+v, error = %v", got, err)
	}
}

func replaceAnalyticsTestIssueBody(t *testing.T, service *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, body string) {
	t.Helper()
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(t.Context(), scope.Project, item)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(t.Context(), string(item), row.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	issue, err := service.hydrateCanonicalWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	issue.Body = body
	issue.Revision++
	issue.UpdatedAt = service.config.now().UTC().Truncate(time.Microsecond)
	fields, err := service.prepareWorkItemFields(t.Context(), scope.Organization, hubrecords.WorkItemRecordPlan{Issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	version, err = content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, RecordID: string(item), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(t.Context(), scope.Project); err != nil {
			return err
		}
		row.BodyObjectKey = &fields.Body
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQualityCauseEvidence(t *testing.T) {
	snapshot := &tracker.LandingQualitySnapshot{IssueBody: "Contract: inputs must be valid", Criteria: []string{"Reject invalid input"}, Evidence: []gate.CriterionEvidence{{Criterion: "Reject invalid input", Kind: gate.EvidenceTest, Reference: "TestInvalidInput", Behavior: "Asserts invalid input rejected"}}}
	for _, tt := range []struct {
		name, cause, criterion, quote string
		missing, valid                bool
	}{
		{"covered criterion and evidence", "validator_miss", "Reject invalid input", "TestInvalidInput", false, true},
		{"invented criterion", "validator_miss", "Reject unknown input", "TestInvalidInput", false, false},
		{"invented evidence", "validator_miss", "Reject invalid input", "TestOther", false, false},
		{"missing criterion grounded in contract", "missing_criterion", "", "inputs must be valid", false, true},
		{"missing criterion cannot name a covered criterion", "missing_criterion", "Reject invalid input", "inputs must be valid", false, false},
		{"missing section is not complete contract", "missing_criterion", "", "inputs must be valid", true, false},
		{"weak contract quoted", "underspecified_issue", "", "inputs must be valid", false, true},
		{"invented contract", "underspecified_issue", "", "Invented contract", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := *snapshot
			if tt.missing {
				copy.Missing = []string{"Must not break"}
			}
			err := validateQualityBasis(&copy, tracker.QualityEscape{Cause: tt.cause, Criterion: tt.criterion, BasisQuote: tt.quote})
			if (err == nil) != tt.valid {
				t.Fatalf("classification error = %v", err)
			}
		})
	}
}
