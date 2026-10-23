package hubserver

import (
	"cmp"
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
	InfraReturns    int            `json:"infrastructure_returns"`
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
	returns     []qualityReturn
}

type qualityReturn struct {
	item  string
	at    time.Time
	class reportClass
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

func analyticsEventsInRecordedOrder(q nativeQueryer) string {
	if _, projected := q.(analyticsRollupReader); !projected {
		return "collaboration_events INDEXED BY collaboration_events_analytics_order"
	}
	return `(SELECT r.organization_id,r.project_id,r.dimension AS id,r.json_work_item_id AS work_item_id,r.json_sequence AS sequence,r.json_type AS type,r.json_recorded_at AS recorded_at,e.data_json FROM analytics_hourly_rollups r INDEXED BY analytics_event_order CROSS JOIN main.collaboration_events e ON e.id=r.dimension WHERE r.metric='event') AS collaboration_events`
}

func readQualityLandings(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error) {
	issues, issueArgs := reportsIssueClause(ctx, "l.work_item_id")
	rows, err := queryAnalyticsRows(ctx, q, `WITH reopened_items(work_item_id) AS MATERIALIZED (
SELECT e.work_item_id FROM collaboration_events e WHERE e.organization_id=? AND e.project_id=? AND e.type='workflow.transitioned' AND e.recorded_at>=? AND e.recorded_at<? AND julianday(e.recorded_at)>=julianday(?) AND julianday(e.recorded_at)<julianday(?) AND lower(json_extract(e.data_json,'$.from_state'))='done'
),escaped_versions(change_id,version_id) AS MATERIALIZED (
SELECT e.change_id,e.version_id FROM change_evidence e INDEXED BY change_evidence_analytics_escape CROSS JOIN change_requests c ON c.id=e.change_id
WHERE c.organization_id=? AND c.project_id=? AND e.kind='discussion' AND json_extract(e.record_json,'$.escape') IS NOT NULL AND julianday(json_extract(e.record_json,'$.escape.observed_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.escape.observed_at'))<julianday(?)
),relevant_changes(id) AS (
SELECT c.id FROM quality_landings l INDEXED BY quality_landings_analytics_landed CROSS JOIN change_versions v ON v.id=l.version_id CROSS JOIN change_requests c ON c.id=v.change_id
WHERE c.organization_id=? AND c.project_id=? AND json_extract(l.record_json,'$.landed_at')>=? AND json_extract(l.record_json,'$.landed_at')<?
UNION ALL SELECT c.id FROM change_requests c WHERE c.organization_id=? AND c.project_id=? AND json_extract(c.record_json,'$.landed.landed_at')>=? AND json_extract(c.record_json,'$.landed.landed_at')<?
UNION ALL SELECT c.id FROM reopened_items i CROSS JOIN change_requests c ON c.work_item_id=i.work_item_id WHERE c.organization_id=? AND c.project_id=?
UNION ALL SELECT change_id FROM escaped_versions
),landed AS (
SELECT c.id,c.work_item_id,v.id AS version_id,l.record_json AS landing FROM quality_landings l JOIN change_versions v ON v.id=l.version_id JOIN change_requests c ON c.id=v.change_id WHERE c.organization_id=? AND c.project_id=? AND c.id IN (SELECT id FROM relevant_changes)
UNION ALL SELECT c.id,c.work_item_id,v.id,json_extract(c.record_json,'$.landed') FROM change_requests c JOIN change_versions v ON v.change_id=c.id AND v.id=json_extract(c.record_json,'$.landed.version_id')
WHERE c.organization_id=? AND c.project_id=? AND c.id IN (SELECT id FROM relevant_changes) AND NOT EXISTS (SELECT 1 FROM quality_landings l WHERE l.version_id=v.id) AND json_extract(c.record_json,'$.landed.head_sha')=json_extract(v.record_json,'$.head_sha') AND length(json_extract(c.record_json,'$.landed.merge_sha')) IN (40,64)
)
SELECT id,work_item_id,landing,version_id,json_extract(landing,'$.landed_at') FROM landed l WHERE julianday(json_extract(landing,'$.landed_at'))<julianday(?) AND (
julianday(json_extract(landing,'$.landed_at'))>=julianday(?) OR l.work_item_id IN (SELECT work_item_id FROM reopened_items)
OR (l.id,l.version_id) IN (SELECT change_id,version_id FROM escaped_versions))
`+issues+`
`, append([]any{
		scope.organization, scope.project, w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly), formatHubTime(w.From), formatHubTime(w.To),
		scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To),
		scope.organization, scope.project, w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly),
		scope.organization, scope.project, w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly),
		scope.organization, scope.project,
		scope.organization, scope.project, scope.organization, scope.project, formatHubTime(w.To), formatHubTime(w.From)}, issueArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type qualityLandingRow struct {
		id, item, raw, version, at string
	}
	population := newAnalyticsOrderedPopulation(ctx, "quality_landings", func(a, b qualityLandingRow) int {
		if order := cmp.Compare(a.at, b.at); order != 0 {
			return order
		}
		if order := cmp.Compare(a.id, b.id); order != 0 {
			return order
		}
		return cmp.Compare(a.version, b.version)
	})
	for rows.Next() {
		var row qualityLandingRow
		if err := rows.Scan(&row.id, &row.item, &row.raw, &row.version, &row.at); err != nil {
			return nil, err
		}
		population.add(row)
	}
	landings := []nativeAnalyticsLanding{}
	for _, row := range population.page(ctx, "quality_landings") {
		landing := nativeAnalyticsLanding{ChangeID: row.id, WorkItemID: tracker.NativeWorkItemID(row.item)}
		if err := json.Unmarshal([]byte(row.raw), &landing.Landing); err != nil {
			return nil, err
		}
		landings = append(landings, landing)
	}
	return landings, rows.Err()
}

type analyticsQualityInputs struct {
	landings    []nativeAnalyticsLanding
	events      []qualityHistoryEvent
	discussions []qualityDiscussion
	partial     bool
}

func loadAnalyticsQualityInputs(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) (analyticsQualityInputs, error) {
	landings, err := readQualityLandings(ctx, q, scope, w)
	if err != nil {
		return analyticsQualityInputs{}, err
	}
	partial := len(landings) > maxAnalyticsPopulation || analyticsPopulationOffset(ctx, "quality_landings") > 0 || analyticsPopulationOffset(ctx, "quality_events") > 0 || analyticsPopulationOffset(ctx, "quality_evidence") > 0 || analyticsPopulationOffset(ctx, "quality_worked") > 0
	landings = landings[:min(len(landings), maxAnalyticsPopulation)]
	eventIssues, eventArgs := reportsIssueClause(ctx, "work_item_id")
	rows, err := queryAnalyticsPopulation(ctx, q, "quality_events", `SELECT id,work_item_id,type,recorded_at,data_json,coalesce((SELECT terminal FROM workflow_states ws WHERE ws.project_id=collaboration_events.project_id AND ws.detent_state=json_extract(data_json,'$.to_state')),0) FROM `+analyticsEventsInRecordedOrder(q)+`
WHERE organization_id=? AND project_id=? AND type='workflow.transitioned'
AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) AND recorded_at>=? AND recorded_at<?`+eventIssues+` ORDER BY recorded_at,sequence,id LIMIT ? OFFSET ?`, append([]any{scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly)}, eventArgs...)...)
	if err != nil {
		return analyticsQualityInputs{}, err
	}
	defer rows.Close()
	events := []qualityHistoryEvent{}
	for rows.Next() {
		var e qualityHistoryEvent
		var at, raw string
		if err := rows.Scan(&e.ID, &e.Item, &e.Kind, &at, &raw, &e.ToTerminal); err != nil {
			rows.Close()
			return analyticsQualityInputs{}, err
		}
		e.At, err = parseTimeValue(at)
		if err == nil {
			err = json.Unmarshal([]byte(raw), &e.Data)
		}
		if err != nil {
			rows.Close()
			return analyticsQualityInputs{}, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return analyticsQualityInputs{}, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return analyticsQualityInputs{}, err
	}
	partial = partial || len(events) > maxAnalyticsPopulation
	events = events[:min(len(events), maxAnalyticsPopulation)]
	evidenceIssues, evidenceArgs := reportsIssueClause(ctx, "c.work_item_id")
	rows, err = queryAnalyticsRows(ctx, q, `SELECT e.change_id,e.kind,e.record_json,c.work_item_id,e.sequence FROM change_evidence e INDEXED BY change_evidence_analytics_review CROSS JOIN change_requests c ON c.id=e.change_id
WHERE c.organization_id=? AND c.project_id=? AND e.kind='review' AND json_extract(e.record_json,'$.decision')='changes_requested' AND julianday(json_extract(e.record_json,'$.created_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.created_at'))<julianday(?) AND NOT EXISTS (SELECT 1 FROM change_versions current JOIN change_versions newer ON newer.change_id=current.change_id AND newer.number>current.number WHERE current.id=e.version_id AND julianday(json_extract(newer.record_json,'$.created_at'))<=julianday(json_extract(e.record_json,'$.created_at')))
`+evidenceIssues+`
UNION ALL SELECT e.change_id,e.kind,e.record_json,c.work_item_id,e.sequence FROM change_evidence e INDEXED BY change_evidence_analytics_escape CROSS JOIN change_requests c ON c.id=e.change_id
WHERE c.organization_id=? AND c.project_id=? AND e.kind='discussion' AND json_extract(e.record_json,'$.escape') IS NOT NULL AND julianday(json_extract(e.record_json,'$.escape.observed_at'))>=julianday(?) AND julianday(json_extract(e.record_json,'$.escape.observed_at'))<julianday(?)
`+evidenceIssues, append(append(append([]any{scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To)}, evidenceArgs...), scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To)), evidenceArgs...)...)
	if err != nil {
		return analyticsQualityInputs{}, err
	}
	defer rows.Close()
	type qualityEvidenceRow struct {
		id, kind, raw, item string
		sequence            int64
	}
	evidence := newAnalyticsOrderedPopulation(ctx, "quality_evidence", func(a, b qualityEvidenceRow) int { return cmp.Compare(a.sequence, b.sequence) })
	for rows.Next() {
		var value qualityEvidenceRow
		if err := rows.Scan(&value.id, &value.kind, &value.raw, &value.item, &value.sequence); err != nil {
			return analyticsQualityInputs{}, err
		}
		evidence.add(value)
	}
	if err := rows.Err(); err != nil {
		return analyticsQualityInputs{}, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return analyticsQualityInputs{}, err
	}
	discussions := []qualityDiscussion{}
	values := evidence.page(ctx, "quality_evidence")
	partial = partial || len(values) > maxAnalyticsPopulation
	for _, value := range values[:min(len(values), maxAnalyticsPopulation)] {
		id, kind, raw, item := value.id, value.kind, value.raw, value.item
		if kind == "discussion" {
			var discussion qualityDiscussion
			discussion.ChangeID = id
			if err := json.Unmarshal([]byte(raw), &discussion.ChangeDiscussion); err != nil {
				rows.Close()
				return analyticsQualityInputs{}, err
			}
			discussions = append(discussions, discussion)
		} else {
			var review tracker.ChangeReview
			if err := json.Unmarshal([]byte(raw), &review); err != nil {
				rows.Close()
				return analyticsQualityInputs{}, err
			}
			events = append(events, qualityHistoryEvent{ID: review.ID, Item: item, Kind: "changes_requested", VersionID: review.VersionID, At: review.CreatedAt})
		}
	}
	role := "coalesce(json_extract(data_json,'$.runtime.identity.role'),json_extract(data_json,'$.identity.role'))"
	if _, projected := q.(analyticsRollupReader); projected {
		role = "attempt_role"
	}
	workedIssues, workedArgs := reportsIssueClause(ctx, "work_item_id")
	rows, err = queryAnalyticsPopulation(ctx, q, "quality_worked", `SELECT work_item_id,started_at FROM `+analyticsAttemptsInStartedOrder(q)+` a WHERE organization_id=? AND project_id=? AND `+role+` IN ('code','rework') AND julianday(started_at)>=julianday(?) AND julianday(started_at)<julianday(?) AND started_at>=? AND started_at<?`+workedIssues+` ORDER BY started_at,id LIMIT ? OFFSET ?`, append([]any{scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), w.From.UTC().Add(-24 * time.Hour).Format(time.DateOnly), w.To.UTC().Add(48 * time.Hour).Format(time.DateOnly)}, workedArgs...)...)
	if err != nil {
		return analyticsQualityInputs{}, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var item, at string
		if err := rows.Scan(&item, &at); err != nil {
			rows.Close()
			return analyticsQualityInputs{}, err
		}
		count++
		if count > maxAnalyticsPopulation {
			partial = true
			break
		}
		when, err := parseTimeValue(at)
		if err != nil {
			rows.Close()
			return analyticsQualityInputs{}, err
		}
		events = append(events, qualityHistoryEvent{Item: item, Kind: "worked", At: when})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return analyticsQualityInputs{}, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return analyticsQualityInputs{}, err
	}
	return analyticsQualityInputs{landings, events, discussions, partial}, nil
}

func readAnalyticsQuality(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (analyticsQuality, error) {
	inputs, err := reportsQualityInputs(ctx, q, scope, w)
	if err != nil {
		return analyticsQuality{}, err
	}
	landings, events, discussions, partial := inputs.landings, inputs.events, inputs.discussions, inputs.partial
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
	out := analyticsQuality{Source: "native_landing_workflow_review_and_escape_evidence", Coverage: "rework: unique_items_returned_before_landing / unique_items_worked_in_window; escape: unique_landed_versions_in_window_with_non_infrastructure_escape_by_window_end / versions_landed_in_window; buckets_use_landing_cohorts; occurrences_and_causes_use_detection_time; pending_classifications_count_as_escapes; infrastructure_excluded_from_escape_rate; infrastructure_returns_excluded_from_rework_rate", Unavailable: []string{}, Buckets: []analyticsQualityBucket{}}
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
	for _, event := range events {
		if event.Kind == "workflow.transitioned" && event.Data.ToState == "Rework" && qualityReturnedBeforeLanding(landings, event) && nativeAnalyticsBucketIndex(event.At, w) >= 0 {
			out.returns = append(out.returns, qualityReturn{item: event.Item, at: event.At, class: classifyReworkReason(event.Data.FromState, event.Data.ReasonDetail)})
		}
	}
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

func qualityReturnedBeforeLanding(landings []nativeAnalyticsLanding, event qualityHistoryEvent) bool {
	for _, landing := range landings {
		if string(landing.WorkItemID) == event.Item && (event.VersionID == "" || event.VersionID == landing.Landing.VersionID) && !landing.Landing.LandedAt.After(event.At) {
			return false
		}
	}
	return true
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
