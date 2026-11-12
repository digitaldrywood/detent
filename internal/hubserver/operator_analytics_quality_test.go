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
		issue := seedAnalyticsTestWorkItem(t, f.service, scope.organization, scope.project, "quality-"+name, index+1, name, "Todo", now.Add(-time.Hour))
		change, version := seedAnalyticsTestChange(t, f.service, scope, issue.WorkItemID, "change-"+name, now.Add(-time.Hour), nil)
		for number := 1; number <= maxAnalyticsPopulation+2; number++ {
			review := tracker.ChangeReview{ID: fmt.Sprintf("%s-%d", name, number), VersionID: version.ID, Decision: "changes_requested", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now.Add(-time.Duration(number) * time.Second)}
			row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.organization, ChangeID: change.ID, Kind: "review", Sequence: int64(number*2 - 1 + index)}, historyTestJSON(t, review))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
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
			for _, event := range inputs.events {
				if event.Kind == "changes_requested" {
					ids = append(ids, event.ID)
				}
			}
			if len(ids) != test.count || inputs.partial != test.partial {
				t.Fatalf("evidence count=%d partial=%v", len(ids), inputs.partial)
			}
			if ids[0] != test.first || ids[len(ids)-1] != test.last {
				t.Fatalf("evidence page begins %s and ends %s", ids[0], ids[len(ids)-1])
			}
			token = paging.next["quality_evidence"]
			if (token != "") != (test.offset < 2000) {
				t.Fatalf("evidence continuation=%v", paging.next)
			}
		})
	}
}

func TestQualityEscapeHistories(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	w := operatortool.AnalyticsWindow{From: base, To: at(48), Bucket: 24 * time.Hour}
	landing := nativeAnalyticsLanding{ChangeID: "change", WorkItemID: "wi_work", Landing: tracker.ChangeLanding{VersionID: "version", MergeSHA: strings.Repeat("a", 40), LandedAt: at(10)}}
	assessment := func(kind, cause string, hour int) qualityDiscussion {
		return qualityDiscussion{ChangeID: landing.ChangeID, ChangeDiscussion: tracker.ChangeDiscussion{ID: "assessment", VersionID: "version", CreatedAt: at(hour), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence", Kind: kind, Cause: cause, ObservedAt: at(hour), EvidenceReference: "record", EvidenceQuote: "failure"}}}
	}
	for _, tt := range []struct {
		name              string
		landings          []nativeAnalyticsLanding
		events            []qualityHistoryEvent
		discussions       []qualityDiscussion
		escapes, affected int
		cause             string
		pending, infra    int
	}{
		{name: "revert", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("revert", "missing_criterion", 30)}, escapes: 1, affected: 1, cause: "missing_criterion"},
		{name: "attributed red suite", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("scheduled_failure", "validator_miss", 30)}, escapes: 1, affected: 1, cause: "validator_miss"},
		{name: "repeat occurrence does not inflate rate", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("revert", "missing_criterion", 30), assessment("revert", "missing_criterion", 30)}, escapes: 1, affected: 1, cause: "missing_criterion"},
		{name: "reopen detected without assessment", landings: []nativeAnalyticsLanding{landing}, events: []qualityHistoryEvent{{ID: "reopen", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}, escapes: 1, affected: 1, pending: 1},
		{name: "Done to another terminal state is not reopening", landings: []nativeAnalyticsLanding{landing}, events: []qualityHistoryEvent{{ID: "cancel", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), ToTerminal: true, Data: tracker.CollaborationData{FromState: "Done", ToState: "Cancelled"}}}},
		{name: "reopen assessment replaces pending detection", landings: []nativeAnalyticsLanding{landing}, events: []qualityHistoryEvent{{ID: "occurrence", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}, discussions: []qualityDiscussion{assessment("reopened", "validator_miss", 30)}, escapes: 1, affected: 1, cause: "validator_miss"},
		{name: "terminal item without landed source is not escape", events: []qualityHistoryEvent{{ID: "reopen", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}},
		{name: "infrastructure stays instance owned", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("scheduled_failure", "infrastructure", 30)}, escapes: 1, cause: "infrastructure", infra: 1},
		{name: "outside window", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("revert", "missing_criterion", 48)}},
		{name: "unlanded change is not escape", discussions: []qualityDiscussion{assessment("revert", "missing_criterion", 30)}},
		{name: "detection before landing is not escape", landings: []nativeAnalyticsLanding{landing}, discussions: []qualityDiscussion{assessment("revert", "missing_criterion", 5)}},
		{name: "old landing contributes detection counts but not window cohort", landings: []nativeAnalyticsLanding{{ChangeID: "change", WorkItemID: "wi_work", Landing: tracker.ChangeLanding{VersionID: "version", LandedAt: at(-10)}}}, discussions: []qualityDiscussion{assessment("revert", "underspecified_issue", 30)}, escapes: 1, cause: "underspecified_issue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeAnalyticsQuality(tt.landings, tt.events, tt.discussions, w)
			inputs := qualityInputsInWindow(analyticsQualityInputs{landings: tt.landings, events: tt.events, discussions: tt.discussions}, w)
			shared := summarizeAnalyticsQuality(inputs.landings, inputs.events, inputs.discussions, w)
			assertAnalyticsJSONEqual(t, got, shared)
			if got.Escapes != tt.escapes || got.EscapedVersions != tt.affected || got.Pending != tt.pending || got.Infrastructure != tt.infra || (tt.cause != "" && got.Causes[tt.cause] != 1) {
				t.Fatalf("quality = %+v", got)
			}
			if tt.infra > 0 && (got.Occurrences.Items[0].WorkItemID != "" || got.Occurrences.Items[0].Attribution != "instance") {
				t.Fatalf("infrastructure attribution = %+v", got.Occurrences.Items[0])
			}
			if tt.affected > 0 && (got.EscapePercent == nil || *got.EscapePercent != 100 || got.Buckets[0].EscapedVersions != 1 || got.Buckets[1].Escapes != 1) {
				t.Fatalf("landing cohort and detection buckets = %+v", got)
			}
		})
	}
	first := assessment("revert", "missing_criterion", 30)
	override := assessment("revert", "underspecified_issue", 30)
	override.Escape.HumanOverride = true
	got := summarizeAnalyticsQuality([]nativeAnalyticsLanding{landing}, []qualityHistoryEvent{
		{Item: "wi_work", Kind: "changes_requested", At: at(5)},
		{Item: "wi_work", Kind: "changes_requested", At: at(6)},
		{Item: "wi_other", Kind: "worked", At: at(6)},
		{Item: "wi_other", Kind: "workflow.transitioned", At: at(7), Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: "landing refused: base_protected"}},
		{Item: "wi_work", Kind: "changes_requested", At: at(35)},
	}, []qualityDiscussion{first, override, first}, w)
	if got.ReworkedItems != 1 || got.WorkedItems != 2 || got.ReworkPercent == nil || *got.ReworkPercent != 50 || got.InfraReturns != 1 || got.Causes["underspecified_issue"] != 1 || got.Causes["missing_criterion"] != 0 {
		t.Fatalf("dedupe, pre-landing rework, infrastructure return or human override = %+v", got)
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

func TestQualityEscapeApplicationAndRead(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	details := &memoryRunDetails{}
	f := newChangeFixture(t, newPostgresNativeService(t, Config{now: func() time.Time { return now }, RunDetails: details}))
	scope := analyticsTestScope(t, f.nativeFixture)
	version := f.publish(t, "v1", "")
	landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("d", 40), BaseRef: "develop", LandedAt: now.Add(-time.Hour), Quality: &tracker.LandingQualitySnapshot{IssueBody: "Must accept valid input", Criteria: []string{"Accept valid input"}, Evidence: []gate.CriterionEvidence{}}}
	frozenRow, err := eventstore.NewContent(details).PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: f.issue.WorkItemID, ChangeID: f.change.ID, VersionID: version.ID}, landing)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RecordVersionLanding(t.Context(), frozenRow) }); err != nil {
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
	row, err := f.service.readChangeVersionRecord(t.Context(), scope.organization, f.change.ID, version.ID)
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

func replaceAnalyticsTestIssueBody(t *testing.T, service *Service, scope nativeScope, item tracker.NativeWorkItemID, body string) {
	t.Helper()
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(t.Context(), scope.project, item)
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
	fields, err := service.prepareWorkItemFields(t.Context(), scope.organization, workItemRecordPlan{issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	version, err = content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, RecordID: string(item), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(t.Context(), scope.project); err != nil {
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
