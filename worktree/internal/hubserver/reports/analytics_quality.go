package reports

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AnalyticsQualityCounts struct {
	WorkedItems     int            `json:"worked_items"`
	ReworkedItems   int            `json:"reworked_items"`
	LandedVersions  int            `json:"landed_versions"`
	EscapedVersions int            `json:"escaped_versions"`
	Escapes         int            `json:"escapes"`
	Infrastructure  int            `json:"infrastructure"`
	InfraReturns    int            `json:"infrastructure_returns"`
	Pending         int            `json:"pending_classification"`
	ReworkPercent   *float64       `json:"rework_percent"`
	EscapePercent   *float64       `json:"escape_percent"`
	Causes          map[string]int `json:"causes"`
}

type analyticsQualityBucket struct {
	AnalyticsQualityCounts
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type AnalyticsQualityEscape struct {
	tracker.QualityEscape
	ChangeID                string        `json:"change_id"`
	VersionID               string        `json:"version_id"`
	WorkItemID              string        `json:"work_item_id,omitempty"`
	MergeSHA                string        `json:"merge_sha"`
	LandedAt                time.Time     `json:"landed_at"`
	DiscussionID            string        `json:"discussion_id,omitempty"`
	Actor                   tracker.Actor `json:"actor"`
	Attribution             string        `json:"attribution"`
	LandingContextAvailable bool          `json:"landing_context_available"`
}

type AnalyticsQuality struct {
	AnalyticsQualityCounts
	Source      string                                        `json:"source"`
	Coverage    string                                        `json:"coverage"`
	Partial     bool                                          `json:"partial"`
	Unavailable []string                                      `json:"unavailable"`
	Buckets     []analyticsQualityBucket                      `json:"buckets"`
	Occurrences operatortool.ReadPage[AnalyticsQualityEscape] `json:"occurrences"`
	returns     []qualityReturn
}

type qualityReturn struct {
	item  string
	at    time.Time
	class ReportClass
}

type QualityHistoryEvent struct {
	ID, Item, Kind, VersionID string
	ToTerminal                bool
	At                        time.Time
	Data                      tracker.CollaborationData
}

type QualityDiscussion struct {
	ChangeID string
	tracker.ChangeDiscussion
}

type AnalyticsQualityInputs struct {
	Unavailable []string                 `json:"-"`
	Landings    []NativeAnalyticsLanding `json:"-"`
	Events      []QualityHistoryEvent    `json:"-"`
	Discussions []QualityDiscussion      `json:"-"`
	Partial     bool                     `json:"-"`
}

func ProjectAnalyticsQualityInputs(inputs AnalyticsQualityInputs, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (AnalyticsQuality, error) {
	landings, events, discussions, partial := inputs.Landings, inputs.Events, inputs.Discussions, inputs.Partial
	out := SummarizeAnalyticsQuality(landings, events, discussions, w)
	out.Partial = partial
	out.Unavailable = append(out.Unavailable, inputs.Unavailable...)
	if partial {
		out.Unavailable = append(out.Unavailable, "quality_complete_population")
		out.ReworkPercent, out.EscapePercent = nil, nil
		for i := range out.Buckets {
			out.Buckets[i].ReworkPercent, out.Buckets[i].EscapePercent = nil, nil
		}
	}
	out.Occurrences = operatortool.OffsetPage(out.Occurrences.Items, r.RowOffset, r.Limit)
	budget := operatortool.MaxResultBytes / 8
	for i, occurrence := range out.Occurrences.Items {
		raw, err := json.Marshal(occurrence)
		if err != nil {
			return AnalyticsQuality{}, err
		}
		if len(raw) > budget {
			next := r.RowOffset + i
			out.Occurrences.Items = out.Occurrences.Items[:i]
			out.Occurrences.NextOffset = &next
			break
		}
		budget -= len(raw) + 1
	}
	return out, nil
}

func SummarizeAnalyticsQuality(landings []NativeAnalyticsLanding, events []QualityHistoryEvent, discussions []QualityDiscussion, w operatortool.AnalyticsWindow) AnalyticsQuality {
	out := AnalyticsQuality{Source: "native_landing_workflow_review_and_escape_evidence", Coverage: "rework: unique_items_returned_before_landing / unique_items_worked_in_window; escape: unique_landed_versions_in_window_with_non_infrastructure_escape_by_window_end / versions_landed_in_window; buckets_use_landing_cohorts; occurrences_and_causes_use_detection_time; pending_classifications_count_as_escapes; infrastructure_excluded_from_escape_rate; infrastructure_returns_excluded_from_rework_rate", Unavailable: []string{}, Buckets: []analyticsQualityBucket{}}
	byVersion := map[string]NativeAnalyticsLanding{}
	byItem := map[string][]NativeAnalyticsLanding{}
	for _, landing := range landings {
		byVersion[landing.ChangeID+"/"+landing.Landing.VersionID] = landing
		byItem[string(landing.WorkItemID)] = append(byItem[string(landing.WorkItemID)], landing)
	}
	occurrences := map[string]AnalyticsQualityEscape{}
	for _, event := range events {
		if event.Kind != "workflow.transitioned" || event.ToTerminal || !strings.EqualFold(event.Data.FromState, "Done") || strings.EqualFold(event.Data.ToState, "Done") {
			continue
		}
		var latest *NativeAnalyticsLanding
		for _, landing := range byItem[event.Item] {
			if !landing.Landing.LandedAt.After(event.At) && (latest == nil || landing.Landing.LandedAt.After(latest.Landing.LandedAt)) {
				copy := landing
				latest = &copy
			}
		}
		if latest == nil {
			continue
		}
		escape := qualityOccurrence(*latest, tracker.QualityEscape{OccurrenceID: event.ID, Kind: "reopened", ObservedAt: event.At, EvidenceReference: "workflow_event:" + event.ID, EvidenceQuote: event.Data.FromState + " → " + event.Data.ToState}, "", tracker.Actor{})
		occurrences[latest.ChangeID+"/"+latest.Landing.VersionID+"\x00"+event.ID] = escape
	}
	for _, discussion := range discussions {
		assessment := discussion.Escape
		landing, ok := byVersion[discussion.ChangeID+"/"+discussion.VersionID]
		if !ok || assessment == nil || discussion.VersionID != landing.Landing.VersionID || assessment.ObservedAt.Before(landing.Landing.LandedAt) || NativeAnalyticsBucketIndex(assessment.ObservedAt, w) < 0 || !discussion.CreatedAt.Before(w.To) {
			continue
		}
		key := discussion.ChangeID + "/" + discussion.VersionID + "\x00" + assessment.OccurrenceID
		if prior, exists := occurrences[key]; exists && prior.HumanOverride && !assessment.HumanOverride {
			continue
		}
		occurrences[key] = qualityOccurrence(landing, *assessment, discussion.ID, discussion.Actor)
	}
	items := make([]AnalyticsQualityEscape, 0, len(occurrences))
	for _, escape := range occurrences {
		items = append(items, escape)
	}
	slices.SortFunc(items, func(a, b AnalyticsQualityEscape) int {
		if order := a.ObservedAt.Compare(b.ObservedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ChangeID+"/"+a.VersionID+"/"+a.OccurrenceID, b.ChangeID+"/"+b.VersionID+"/"+b.OccurrenceID)
	})
	out.Occurrences.Items = items
	out.AnalyticsQualityCounts = QualityCounts(landings, events, items, w.From, w.To, w.To)
	for _, event := range events {
		if event.Kind == "workflow.transitioned" && event.Data.ToState == "Rework" && qualityReturnedBeforeLanding(landings, event) && NativeAnalyticsBucketIndex(event.At, w) >= 0 {
			out.returns = append(out.returns, qualityReturn{item: event.Item, at: event.At, class: classifyReworkReason(event.Data.FromState, event.Data.ReasonDetail)})
		}
	}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		to := hubapi.MinTime(from.Add(w.Bucket), w.To)
		out.Buckets = append(out.Buckets, analyticsQualityBucket{From: from, To: to, AnalyticsQualityCounts: QualityCounts(landings, events, items, from, to, w.To)})
	}
	return out
}

func qualityOccurrence(landing NativeAnalyticsLanding, escape tracker.QualityEscape, discussion string, actor tracker.Actor) AnalyticsQualityEscape {
	item := string(landing.WorkItemID)
	attribution := "change"
	if escape.Cause == "infrastructure" {
		item, attribution = "", "instance"
	}
	return AnalyticsQualityEscape{QualityEscape: escape, ChangeID: landing.ChangeID, VersionID: landing.Landing.VersionID, WorkItemID: item, MergeSHA: landing.Landing.MergeSHA, LandedAt: landing.Landing.LandedAt, DiscussionID: discussion, Actor: actor, Attribution: attribution, LandingContextAvailable: landing.Landing.Quality != nil}
}

func qualityReturnedBeforeLanding(landings []NativeAnalyticsLanding, event QualityHistoryEvent) bool {
	for _, landing := range landings {
		if string(landing.WorkItemID) == event.Item && (event.VersionID == "" || event.VersionID == landing.Landing.VersionID) && !landing.Landing.LandedAt.After(event.At) {
			return false
		}
	}
	return true
}

func QualityCounts(landings []NativeAnalyticsLanding, events []QualityHistoryEvent, escapes []AnalyticsQualityEscape, from, to, observedTo time.Time) AnalyticsQualityCounts {
	out := AnalyticsQualityCounts{Causes: map[string]int{"underspecified_issue": 0, "missing_criterion": 0, "validator_miss": 0, "infrastructure": 0}}
	worked, reworked, cohort, affected := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, landing := range landings {
		if !landing.Landing.LandedAt.Before(from) && landing.Landing.LandedAt.Before(to) {
			cohort[landing.ChangeID+"/"+landing.Landing.VersionID] = true
			worked[string(landing.WorkItemID)] = true
		}
	}
	for _, event := range events {
		if event.At.Before(from) || !event.At.Before(to) {
			continue
		}
		returned := (event.Kind == "changes_requested" || event.Data.ToState == "Rework") && qualityReturnedBeforeLanding(landings, event)
		if returned && event.Kind != "changes_requested" && classifyReworkReason(event.Data.FromState, event.Data.ReasonDetail).Kind == reportKindInfrastructure {
			out.InfraReturns++
		} else if returned {
			reworked[event.Item] = true
		}
		if returned || event.Kind == "worked" || event.Data.ToState == "In Progress" || event.Data.ToState == "Human Review" || event.Data.ToState == "Merging" {
			worked[event.Item] = true
		}
	}
	for _, escape := range escapes {
		if !escape.ObservedAt.Before(observedTo) {
			continue
		}
		if escape.Cause != "infrastructure" && cohort[escape.ChangeID+"/"+escape.VersionID] {
			affected[escape.ChangeID+"/"+escape.VersionID] = true
		}
		if escape.ObservedAt.Before(from) || !escape.ObservedAt.Before(to) {
			continue
		}
		out.Escapes++
		if escape.Cause == "" {
			out.Pending++
		} else {
			out.Causes[escape.Cause]++
		}
		if escape.Cause == "infrastructure" {
			out.Infrastructure++
		}
	}
	out.WorkedItems, out.ReworkedItems = len(worked), len(reworked)
	out.LandedVersions, out.EscapedVersions = len(cohort), len(affected)
	if out.WorkedItems > 0 {
		out.ReworkPercent = new(100 * float64(out.ReworkedItems) / float64(out.WorkedItems))
	}
	if out.LandedVersions > 0 {
		out.EscapePercent = new(100 * float64(out.EscapedVersions) / float64(out.LandedVersions))
	}
	return out
}

type AnalyticsLandingPopulation struct {
	Items                                            []NativeAnalyticsLanding
	Partial, HistoryUnknown, ContinuationUnavailable bool
}
