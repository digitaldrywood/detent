package hubserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
		{Item: "wi_work", Kind: "changes_requested", At: at(35)},
	}, []qualityDiscussion{first, override, first}, w)
	if got.ReworkedItems != 1 || got.WorkedItems != 2 || got.ReworkPercent == nil || *got.ReworkPercent != 50 || got.Causes["underspecified_issue"] != 1 || got.Causes["missing_criterion"] != 0 {
		t.Fatalf("dedupe, pre-landing rework or human override = %+v", got)
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
	body := issueorigin.Stamp("Backend startup failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: scheduled.RunURL, Fingerprint: strings.Repeat("a", 64)})
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", body, f.issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "recorded-scheduled"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusOK)
	frozen, err := readChangeVersion(t.Context(), f.service.database.db, f.change.ID, version.ID)
	if err != nil || frozen.LandingQuality == nil || frozen.LandingQuality.IssueBody != landing.Quality.IssueBody {
		t.Fatalf("later issue edits changed landing classification context: %+v, %v", frozen.LandingQuality, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	w := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Hour), To: now.Add(time.Second), Bucket: time.Hour}
	got, err := readAnalyticsQuality(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 10}, w)
	if err != nil || got.Escapes != 2 || got.LandedVersions != 1 || got.Causes["infrastructure"] != 1 || got.Causes["missing_criterion"] != 1 || got.EscapePercent == nil || *got.EscapePercent != 100 {
		t.Fatalf("quality read = %+v, error = %v", got, err)
	}
}
