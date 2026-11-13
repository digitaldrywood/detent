package reports

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestQualityEscapeHistories(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	w := operatortool.AnalyticsWindow{From: base, To: at(48), Bucket: 24 * time.Hour}
	landing := NativeAnalyticsLanding{ChangeID: "change", WorkItemID: "wi_work", Landing: tracker.ChangeLanding{VersionID: "version", MergeSHA: strings.Repeat("a", 40), LandedAt: at(10)}}
	assessment := func(kind, cause string, hour int) QualityDiscussion {
		return QualityDiscussion{ChangeID: landing.ChangeID, ChangeDiscussion: tracker.ChangeDiscussion{ID: "assessment", VersionID: "version", CreatedAt: at(hour), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence", Kind: kind, Cause: cause, ObservedAt: at(hour), EvidenceReference: "record", EvidenceQuote: "failure"}}}
	}
	for _, tt := range []struct {
		name              string
		landings          []NativeAnalyticsLanding
		events            []QualityHistoryEvent
		discussions       []QualityDiscussion
		escapes, affected int
		cause             string
		pending, infra    int
	}{
		{name: "revert", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("revert", "missing_criterion", 30)}, escapes: 1, affected: 1, cause: "missing_criterion"},
		{name: "attributed red suite", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("scheduled_failure", "validator_miss", 30)}, escapes: 1, affected: 1, cause: "validator_miss"},
		{name: "repeat occurrence does not inflate rate", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("revert", "missing_criterion", 30), assessment("revert", "missing_criterion", 30)}, escapes: 1, affected: 1, cause: "missing_criterion"},
		{name: "reopen detected without assessment", landings: []NativeAnalyticsLanding{landing}, events: []QualityHistoryEvent{{ID: "reopen", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}, escapes: 1, affected: 1, pending: 1},
		{name: "Done to another terminal state is not reopening", landings: []NativeAnalyticsLanding{landing}, events: []QualityHistoryEvent{{ID: "cancel", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), ToTerminal: true, Data: tracker.CollaborationData{FromState: "Done", ToState: "Cancelled"}}}},
		{name: "reopen assessment replaces pending detection", landings: []NativeAnalyticsLanding{landing}, events: []QualityHistoryEvent{{ID: "occurrence", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}, discussions: []QualityDiscussion{assessment("reopened", "validator_miss", 30)}, escapes: 1, affected: 1, cause: "validator_miss"},
		{name: "terminal item without landed source is not escape", events: []QualityHistoryEvent{{ID: "reopen", Item: "wi_work", Kind: "workflow.transitioned", At: at(30), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}}}},
		{name: "infrastructure stays instance owned", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("scheduled_failure", "infrastructure", 30)}, escapes: 1, cause: "infrastructure", infra: 1},
		{name: "outside window", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("revert", "missing_criterion", 48)}},
		{name: "unlanded change is not escape", discussions: []QualityDiscussion{assessment("revert", "missing_criterion", 30)}},
		{name: "detection before landing is not escape", landings: []NativeAnalyticsLanding{landing}, discussions: []QualityDiscussion{assessment("revert", "missing_criterion", 5)}},
		{name: "old landing contributes detection counts but not window cohort", landings: []NativeAnalyticsLanding{{ChangeID: "change", WorkItemID: "wi_work", Landing: tracker.ChangeLanding{VersionID: "version", LandedAt: at(-10)}}}, discussions: []QualityDiscussion{assessment("revert", "underspecified_issue", 30)}, escapes: 1, cause: "underspecified_issue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := SummarizeAnalyticsQuality(tt.landings, tt.events, tt.discussions, w)
			inputs := qualityInputsInWindow(AnalyticsQualityInputs{Landings: tt.landings, Events: tt.events, Discussions: tt.discussions}, w)
			shared := SummarizeAnalyticsQuality(inputs.Landings, inputs.Events, inputs.Discussions, w)
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
	got := SummarizeAnalyticsQuality([]NativeAnalyticsLanding{landing}, []QualityHistoryEvent{
		{Item: "wi_work", Kind: "changes_requested", At: at(5)},
		{Item: "wi_work", Kind: "changes_requested", At: at(6)},
		{Item: "wi_other", Kind: "worked", At: at(6)},
		{Item: "wi_other", Kind: "workflow.transitioned", At: at(7), Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: "landing refused: base_protected"}},
		{Item: "wi_work", Kind: "changes_requested", At: at(35)},
	}, []QualityDiscussion{first, override, first}, w)
	if got.ReworkedItems != 1 || got.WorkedItems != 2 || got.ReworkPercent == nil || *got.ReworkPercent != 50 || got.InfraReturns != 1 || got.Causes["underspecified_issue"] != 1 || got.Causes["missing_criterion"] != 0 {
		t.Fatalf("dedupe, pre-landing rework, infrastructure return or human override = %+v", got)
	}
}
