package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsQualityCounts struct {
	WorkedItems     int            `json:"worked_items"`
	ReworkedItems   int            `json:"reworked_items"`
	LandedVersions  int            `json:"landed_versions"`
	EscapedVersions int            `json:"escaped_versions"`
	Escapes         int            `json:"escapes"`
	Infrastructure  int            `json:"infrastructure"`
	Pending         int            `json:"pending_classification"`
	ReworkPercent   *float64       `json:"rework_percent"`
	EscapePercent   *float64       `json:"escape_percent"`
	Causes          map[string]int `json:"causes"`
}

type analyticsQualityBucket struct {
	analyticsQualityCounts
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type analyticsQualityEscape struct {
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

type analyticsQuality struct {
	analyticsQualityCounts
	Source      string                                        `json:"source"`
	Coverage    string                                        `json:"coverage"`
	Partial     bool                                          `json:"partial"`
	Unavailable []string                                      `json:"unavailable"`
	Buckets     []analyticsQualityBucket                      `json:"buckets"`
	Occurrences operatortool.ReadPage[analyticsQualityEscape] `json:"occurrences"`
}

type qualityHistoryEvent struct {
	ID, Item, Kind, VersionID string
	ToTerminal                bool
	At                        time.Time
	Data                      tracker.CollaborationData
}

type qualityDiscussion struct {
	ChangeID string
	tracker.ChangeDiscussion
}

func readQualityLandings(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error) {
	rows, err := q.QueryContext(ctx, `WITH landed AS (
SELECT c.id,c.work_item_id,v.id AS version_id,l.record_json AS landing FROM quality_landings l JOIN change_versions v ON v.id=l.version_id JOIN change_requests c ON c.id=v.change_id WHERE c.organization_id=? AND c.project_id=?
UNION ALL SELECT c.id,c.work_item_id,v.id,json_extract(c.record_json,'$.landed') FROM change_requests c JOIN change_versions v ON v.change_id=c.id AND v.id=json_extract(c.record_json,'$.landed.version_id')
WHERE c.organization_id=? AND c.project_id=? AND NOT EXISTS (SELECT 1 FROM quality_landings l WHERE l.version_id=v.id) AND json_extract(c.record_json,'$.landed.head_sha')=json_extract(v.record_json,'$.head_sha') AND length(json_extract(c.record_json,'$.landed.merge_sha')) IN (40,64)
)
SELECT id,work_item_id,landing FROM landed l WHERE julianday(json_extract(landing,'$.landed_at'))<julianday(?) AND (
julianday(json_extract(landing,'$.landed_at'))>=julianday(?) OR EXISTS (SELECT 1 FROM collaboration_events e WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=l.work_item_id AND e.type='workflow.transitioned' AND lower(json_extract(e.data_json,'$.from_state'))='done' AND julianday(e.recorded_at)>=julianday(?) AND julianday(e.recorded_at)<julianday(?))
OR EXISTS (SELECT 1 FROM change_evidence e WHERE e.change_id=l.id AND e.version_id=l.version_id AND e.kind='discussion' AND julianday(json_extract(e.record_json,'$.escape.observed_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.escape.observed_at'))<julianday(?)))
ORDER BY json_extract(landing,'$.landed_at'),id,version_id LIMIT ?`, scope.organization, scope.project, scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From), scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	landings := []nativeAnalyticsLanding{}
	for rows.Next() {
		var landing nativeAnalyticsLanding
		var raw string
		if err := rows.Scan(&landing.ChangeID, &landing.WorkItemID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &landing.Landing); err != nil {
			return nil, err
		}
		landings = append(landings, landing)
	}
	return landings, rows.Err()
}

func readAnalyticsQuality(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (analyticsQuality, error) {
	landings, err := readQualityLandings(ctx, q, scope, w)
	if err != nil {
		return analyticsQuality{}, err
	}
	partial := len(landings) > maxAnalyticsPopulation
	landings = landings[:min(len(landings), maxAnalyticsPopulation)]
	rows, err := q.QueryContext(ctx, `SELECT id,work_item_id,type,recorded_at,data_json,coalesce((SELECT terminal FROM workflow_states ws WHERE ws.project_id=collaboration_events.project_id AND ws.detent_state=json_extract(data_json,'$.to_state')),0) FROM collaboration_events
WHERE organization_id=? AND project_id=? AND type='workflow.transitioned'
AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) ORDER BY recorded_at,sequence,id LIMIT ?`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1)
	if err != nil {
		return analyticsQuality{}, err
	}
	defer rows.Close()
	events := []qualityHistoryEvent{}
	for rows.Next() {
		var e qualityHistoryEvent
		var at, raw string
		if err := rows.Scan(&e.ID, &e.Item, &e.Kind, &at, &raw, &e.ToTerminal); err != nil {
			rows.Close()
			return analyticsQuality{}, err
		}
		e.At, err = parseTimeValue(at)
		if err == nil {
			err = json.Unmarshal([]byte(raw), &e.Data)
		}
		if err != nil {
			rows.Close()
			return analyticsQuality{}, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return analyticsQuality{}, err
	}
	if err := rows.Close(); err != nil {
		return analyticsQuality{}, err
	}
	partial = partial || len(events) > maxAnalyticsPopulation
	events = events[:min(len(events), maxAnalyticsPopulation)]
	rows, err = q.QueryContext(ctx, `SELECT e.change_id,e.kind,e.record_json,c.work_item_id FROM change_evidence e JOIN change_requests c ON c.id=e.change_id
WHERE c.organization_id=? AND c.project_id=? AND ((e.kind='review' AND json_extract(e.record_json,'$.decision')='changes_requested' AND julianday(json_extract(e.record_json,'$.created_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.created_at'))<julianday(?) AND NOT EXISTS (SELECT 1 FROM change_versions current JOIN change_versions newer ON newer.change_id=current.change_id AND newer.number>current.number WHERE current.id=e.version_id AND julianday(json_extract(newer.record_json,'$.created_at'))<=julianday(json_extract(e.record_json,'$.created_at'))))
OR (e.kind='discussion' AND json_extract(e.record_json,'$.escape') IS NOT NULL AND julianday(json_extract(e.record_json,'$.escape.observed_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.escape.observed_at'))<julianday(?)))
ORDER BY e.sequence LIMIT ?`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1)
	if err != nil {
		return analyticsQuality{}, err
	}
	defer rows.Close()
	discussions := []qualityDiscussion{}
	count := 0
	for rows.Next() {
		var id, kind, raw, item string
		if err := rows.Scan(&id, &kind, &raw, &item); err != nil {
			rows.Close()
			return analyticsQuality{}, err
		}
		count++
		if count > maxAnalyticsPopulation {
			partial = true
			break
		}
		if kind == "discussion" {
			var discussion qualityDiscussion
			discussion.ChangeID = id
			if err := json.Unmarshal([]byte(raw), &discussion.ChangeDiscussion); err != nil {
				rows.Close()
				return analyticsQuality{}, err
			}
			discussions = append(discussions, discussion)
		} else {
			var review tracker.ChangeReview
			if err := json.Unmarshal([]byte(raw), &review); err != nil {
				rows.Close()
				return analyticsQuality{}, err
			}
			events = append(events, qualityHistoryEvent{ID: review.ID, Item: item, Kind: "changes_requested", VersionID: review.VersionID, At: review.CreatedAt})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return analyticsQuality{}, err
	}
	if err := rows.Close(); err != nil {
		return analyticsQuality{}, err
	}
	rows, err = q.QueryContext(ctx, `SELECT work_item_id,started_at FROM native_attempts WHERE organization_id=? AND project_id=? AND coalesce(json_extract(data_json,'$.runtime.identity.role'),json_extract(data_json,'$.identity.role')) IN ('code','rework') AND julianday(started_at)>=julianday(?) AND julianday(started_at)<julianday(?) ORDER BY started_at,id LIMIT ?`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1)
	if err != nil {
		return analyticsQuality{}, err
	}
	defer rows.Close()
	count = 0
	for rows.Next() {
		var item, at string
		if err := rows.Scan(&item, &at); err != nil {
			rows.Close()
			return analyticsQuality{}, err
		}
		count++
		if count > maxAnalyticsPopulation {
			partial = true
			break
		}
		when, err := parseTimeValue(at)
		if err != nil {
			rows.Close()
			return analyticsQuality{}, err
		}
		events = append(events, qualityHistoryEvent{Item: item, Kind: "worked", At: when})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return analyticsQuality{}, err
	}
	if err := rows.Close(); err != nil {
		return analyticsQuality{}, err
	}
	out := summarizeAnalyticsQuality(landings, events, discussions, w)
	out.Partial = partial
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
			return analyticsQuality{}, err
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

func summarizeAnalyticsQuality(landings []nativeAnalyticsLanding, events []qualityHistoryEvent, discussions []qualityDiscussion, w operatortool.AnalyticsWindow) analyticsQuality {
	out := analyticsQuality{Source: "native_landing_workflow_review_and_escape_evidence", Coverage: "rework: unique_items_returned_before_landing / unique_items_worked_in_window; escape: unique_landed_versions_in_window_with_non_infrastructure_escape_by_window_end / versions_landed_in_window; buckets_use_landing_cohorts; occurrences_and_causes_use_detection_time; pending_classifications_count_as_escapes; infrastructure_excluded_from_escape_rate", Unavailable: []string{}, Buckets: []analyticsQualityBucket{}}
	byVersion := map[string]nativeAnalyticsLanding{}
	byItem := map[string][]nativeAnalyticsLanding{}
	for _, landing := range landings {
		byVersion[landing.ChangeID+"/"+landing.Landing.VersionID] = landing
		byItem[string(landing.WorkItemID)] = append(byItem[string(landing.WorkItemID)], landing)
	}
	occurrences := map[string]analyticsQualityEscape{}
	for _, event := range events {
		if event.Kind != "workflow.transitioned" || event.ToTerminal || !strings.EqualFold(event.Data.FromState, "Done") || strings.EqualFold(event.Data.ToState, "Done") {
			continue
		}
		var latest *nativeAnalyticsLanding
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
		if !ok || assessment == nil || discussion.VersionID != landing.Landing.VersionID || assessment.ObservedAt.Before(landing.Landing.LandedAt) || nativeAnalyticsBucketIndex(assessment.ObservedAt, w) < 0 || !discussion.CreatedAt.Before(w.To) {
			continue
		}
		key := discussion.ChangeID + "/" + discussion.VersionID + "\x00" + assessment.OccurrenceID
		if prior, exists := occurrences[key]; exists && prior.HumanOverride && !assessment.HumanOverride {
			continue
		}
		occurrences[key] = qualityOccurrence(landing, *assessment, discussion.ID, discussion.Actor)
	}
	items := make([]analyticsQualityEscape, 0, len(occurrences))
	for _, escape := range occurrences {
		items = append(items, escape)
	}
	slices.SortFunc(items, func(a, b analyticsQualityEscape) int {
		if order := a.ObservedAt.Compare(b.ObservedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ChangeID+"/"+a.VersionID+"/"+a.OccurrenceID, b.ChangeID+"/"+b.VersionID+"/"+b.OccurrenceID)
	})
	out.Occurrences.Items = items
	out.analyticsQualityCounts = qualityCounts(landings, events, items, w.From, w.To, w.To)
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		to := minTime(from.Add(w.Bucket), w.To)
		out.Buckets = append(out.Buckets, analyticsQualityBucket{From: from, To: to, analyticsQualityCounts: qualityCounts(landings, events, items, from, to, w.To)})
	}
	return out
}

func qualityOccurrence(landing nativeAnalyticsLanding, escape tracker.QualityEscape, discussion string, actor tracker.Actor) analyticsQualityEscape {
	item := string(landing.WorkItemID)
	attribution := "change"
	if escape.Cause == "infrastructure" {
		item, attribution = "", "instance"
	}
	return analyticsQualityEscape{QualityEscape: escape, ChangeID: landing.ChangeID, VersionID: landing.Landing.VersionID, WorkItemID: item, MergeSHA: landing.Landing.MergeSHA, LandedAt: landing.Landing.LandedAt, DiscussionID: discussion, Actor: actor, Attribution: attribution, LandingContextAvailable: landing.Landing.Quality != nil}
}

func qualityCounts(landings []nativeAnalyticsLanding, events []qualityHistoryEvent, escapes []analyticsQualityEscape, from, to, observedTo time.Time) analyticsQualityCounts {
	out := analyticsQualityCounts{Causes: map[string]int{"underspecified_issue": 0, "missing_criterion": 0, "validator_miss": 0, "infrastructure": 0}}
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
		returned := event.Kind == "changes_requested" || event.Data.ToState == "Rework"
		if returned {
			for _, landing := range landings {
				if string(landing.WorkItemID) == event.Item && (event.VersionID == "" || event.VersionID == landing.Landing.VersionID) && !landing.Landing.LandedAt.After(event.At) {
					returned = false
				}
			}
		}
		if returned {
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
