package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsQualityEvidencePopulationOrder(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	now := f.service.config.now().UTC().Truncate(time.Hour)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index, name := range []string{"first", "second"} {
		issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: name, Body: nativeContractTestBody, State: "Todo"}, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		change, version := "change-"+string(issue.WorkItemID), "version-"+string(issue.WorkItemID)
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,'{}')", change, scope.organization, scope.project, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,'{}')", version, change); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO change_evidence(change_id,sequence,kind,version_id,record_json)
SELECT ?,x*2-?,'review',?,json_object('review_id',?||x,'version_id',?,'decision','changes_requested','created_at',strftime('%Y-%m-%dT%H:%M:%SZ',?,'-'||x||' seconds')) FROM n`, hubanalytics.MaxPopulation+2, change, 1-index, version, name+"-", version, formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	projectTestChangeColumns(t, f.service)
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now, Bucket: time.Hour}
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
			ctx, paging, err := hubanalytics.PagingContext(t.Context(), fmt.Sprintf("quality_evidence:%d:0", test.offset))
			if err != nil {
				t.Fatal(err)
			}
			inputs, err := loadAnalyticsQualityInputs(ctx, f.service.database.reader, scope, w)
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
			if test.offset < 2000 && paging.Next["quality_evidence"] != fmt.Sprintf("quality_evidence:%d:0", test.offset+hubanalytics.MaxPopulation) {
				t.Fatalf("evidence continuation=%v", paging.Next)
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
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
	version := f.publish(t, "v1", "")
	landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("d", 40), LandedAt: now.Add(-time.Hour), Quality: &tracker.LandingQualitySnapshot{IssueBody: "Must accept valid input", Criteria: []string{"Accept valid input"}, Evidence: []gate.CriterionEvidence{}}}
	encoded, err := json.Marshal(landing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO quality_landings (version_id,record_json) VALUES (?,?)", version.ID, encoded); err != nil {
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", body, f.issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "recorded-scheduled"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusOK)
	request.IdempotencyKey = "changed-scheduled-source"
	injected := false
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if injected || !strings.Contains(query, "FROM change_requests c JOIN issues i") {
			return
		}
		injected = true
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='Evidence withdrawn' WHERE native_id=?", f.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}})
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, f.path+"/discussion", f.token, request).WithContext(ctx))
	requireNativeStatus(t, response, http.StatusConflict)
	if !injected {
		t.Fatal("scheduled evidence edit was not injected")
	}
	frozen, err := readChangeVersion(f.service.contentContext(t.Context()), f.service.database.db, f.change.ID, version.ID)
	if err != nil || frozen.LandingQuality == nil || frozen.LandingQuality.IssueBody != landing.Quality.IssueBody {
		t.Fatalf("later issue edits changed landing classification context: %+v, %v", frozen.LandingQuality, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	w := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Hour), To: now.Add(time.Second), Bucket: time.Hour}
	got, err := readAnalyticsQuality(f.service.contentContext(t.Context()), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 10}, w)
	if err != nil || got.Escapes != 2 || got.LandedVersions != 1 || got.Causes["infrastructure"] != 1 || got.Causes["missing_criterion"] != 1 || got.EscapePercent == nil || *got.EscapePercent != 100 {
		t.Fatalf("quality read = %+v, error = %v", got, err)
	}
}
