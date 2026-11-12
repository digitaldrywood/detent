package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

type analyticsQualityInputs struct {
	unavailable []string
	landings    []nativeAnalyticsLanding
	events      []qualityHistoryEvent
	discussions []qualityDiscussion
	partial     bool
}

func projectAnalyticsQualityInputs(inputs analyticsQualityInputs, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (analyticsQuality, error) {
	landings, events, discussions, partial := inputs.landings, inputs.events, inputs.discussions, inputs.partial
	out := summarizeAnalyticsQuality(landings, events, discussions, w)
	out.Partial = partial
	out.Unavailable = append(out.Unavailable, inputs.unavailable...)
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

type analyticsLandingPopulation struct {
	Items                                            []nativeAnalyticsLanding
	Partial, HistoryUnknown, ContinuationUnavailable bool
}

func (s *Service) readAnalyticsLandingPopulationTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, name string, reopened []tracker.NativeWorkItemID, escaped []string) (analyticsLandingPopulation, error) {
	out := analyticsLandingPopulation{Items: []nativeAnalyticsLanding{}}
	after, cursor, key, err := s.analyticsFactSeek(ctx, scope, w, name)
	if err != nil {
		return out, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, workItemStorageUnavailable()
	}
	var page eventstore.AnalyticsLandingPage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(eventstore.AnalyticsLandingReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsLandingPage(ctx, eventstore.AnalyticsLandingQuery{ProjectID: scope.project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, AllVersions: name == "quality_landings", After: after.Landing, ReopenedItems: reopened, EscapedVersions: escaped})
		return err
	})
	if err != nil {
		return out, err
	}
	out.Partial, out.HistoryUnknown = page.Partial, page.HistoryUnknown
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, row := range page.Items {
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		value, err := content.VersionLanding(ctx, row)
		if err != nil {
			return out, err
		}
		if name == "landings" {
			value.Quality = nil
		}
		out.Items = append(out.Items, nativeAnalyticsLanding{ChangeID: row.ChangeID, WorkItemID: row.WorkItemID, Landing: value})
	}
	if page.Next.ChangeID != "" {
		if len(key) == 0 {
			out.Partial = true
			out.ContinuationUnavailable = true
		} else {
			token, err := s.encodeAnalyticsFactCursor(cursor, key, analyticsFactCursor{Landing: page.Next}, name)
			if err != nil {
				return out, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.next[name] = token
			}
		}
	}
	return out, nil
}

func (s *Service) readAnalyticsQualityInputsTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow) (analyticsQualityInputs, error) {
	out := analyticsQualityInputs{landings: []nativeAnalyticsLanding{}, events: []qualityHistoryEvent{}, discussions: []qualityDiscussion{}}
	ctx, _, err := analyticsPagingContext(ctx, "")
	if err != nil {
		return out, err
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, workItemStorageUnavailable()
	}
	after, eventCursor, eventKey, err := s.analyticsFactSeek(ctx, scope, w, "quality_events")
	if err != nil {
		return out, err
	}
	defer clear(eventKey)
	evidenceAfter, evidenceCursor, evidenceKey, err := s.analyticsFactSeek(ctx, scope, w, "quality_evidence")
	if err != nil {
		return out, err
	}
	defer clear(evidenceKey)
	var workflow eventstore.AnalyticsWorkflowPage
	var evidence eventstore.AnalyticsEvidencePage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		workflowOwner, ok := r.(eventstore.AnalyticsWorkflowReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		evidenceOwner, ok := r.(eventstore.AnalyticsEvidenceReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		workflow, err = workflowOwner.ReadAnalyticsWorkflowPage(ctx, eventstore.AnalyticsWorkflowQuery{ProjectID: scope.project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after.Workflow})
		if err != nil {
			return err
		}
		evidence, err = evidenceOwner.ReadAnalyticsEvidencePage(ctx, eventstore.AnalyticsEvidenceQuery{ProjectID: scope.project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: evidenceAfter.Evidence})
		return err
	})
	if err != nil {
		return out, err
	}
	out.partial = workflow.Partial || evidence.Partial
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	reopened := []tracker.NativeWorkItemID{}
	seenReopened := map[tracker.NativeWorkItemID]bool{}
	escaped := []string{}
	seenEscaped := map[string]bool{}
	for _, row := range workflow.Items {
		if row.Event.OrganizationID != scope.organization || row.Event.ProjectID != scope.project {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.Event.WorkItemID) {
			continue
		}
		value, err := content.Event(ctx, row.Event)
		if err != nil {
			return out, err
		}
		out.events = append(out.events, qualityHistoryEvent{ID: value.ID, Item: string(value.AggregateID), Kind: value.Type, ToTerminal: row.ToTerminal, At: value.RecordedAt, Data: value.Data})
		if strings.EqualFold(value.Data.FromState, "Done") && !seenReopened[value.AggregateID] {
			seenReopened[value.AggregateID] = true
			reopened = append(reopened, value.AggregateID)
		}
	}
	for _, row := range evidence.Items {
		if row.Evidence.OrganizationID != scope.organization {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		raw, err := content.Evidence(ctx, row.Evidence)
		if err != nil {
			return out, err
		}
		if row.Evidence.Kind == "review" {
			review, err := eventstore.DecodeChangeReview(row.Evidence, raw)
			if err != nil {
				return out, err
			}
			out.events = append(out.events, qualityHistoryEvent{ID: review.ID, Item: string(row.WorkItemID), Kind: "changes_requested", VersionID: review.VersionID, At: review.CreatedAt})
		} else {
			var value tracker.ChangeDiscussion
			version := ""
			if row.Evidence.VersionID != nil {
				version = *row.Evidence.VersionID
			}
			if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != version || value.Actor.Kind != row.Evidence.ActorKind || value.Actor.PrincipalID != row.Evidence.ActorPrincipalID || !value.CreatedAt.UTC().Truncate(time.Microsecond).Equal(row.Evidence.CreatedAt.UTC().Truncate(time.Microsecond)) || value.Escape == nil || row.Escape == nil || value.Escape.OccurrenceID != row.Escape.OccurrenceID {
				return out, artifact.ErrIntegrity
			}
			value.Escape.Kind, value.Escape.Cause, value.Escape.ObservedAt, value.Escape.InstanceID, value.Escape.HumanOverride = row.Escape.Kind, row.Escape.Cause, row.Escape.ObservedAt, row.Escape.InstanceID, row.Escape.HumanOverride
			out.discussions = append(out.discussions, qualityDiscussion{ChangeID: row.Evidence.ChangeID, ChangeDiscussion: value})
			identity := row.Evidence.ChangeID + "/" + value.VersionID
			if !seenEscaped[identity] {
				seenEscaped[identity] = true
				escaped = append(escaped, identity)
			}
		}
	}
	paging := analyticsPagingFromContext(ctx)
	if workflow.Next.ID != "" {
		if len(eventKey) == 0 {
			out.partial = true
			out.unavailable = append(out.unavailable, "quality_events_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(eventCursor, eventKey, analyticsFactCursor{Workflow: workflow.Next}, "quality_events")
			if err != nil {
				return out, err
			}
			paging.next["quality_events"] = token
		}
	}
	if evidence.Next.ChangeID != "" {
		if len(evidenceKey) == 0 {
			out.partial = true
			out.unavailable = append(out.unavailable, "quality_evidence_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(evidenceCursor, evidenceKey, analyticsFactCursor{Evidence: evidence.Next}, "quality_evidence")
			if err != nil {
				return out, err
			}
			paging.next["quality_evidence"] = token
		}
	}
	landings, err := s.readAnalyticsLandingPopulationTenant(ctx, scope, w, "quality_landings", reopened, escaped)
	if err != nil {
		return out, err
	}
	out.landings = landings.Items
	out.partial = out.partial || landings.Partial
	if landings.HistoryUnknown {
		out.unavailable = append(out.unavailable, "quality_landing_history_unavailable")
	}
	if landings.ContinuationUnavailable {
		out.unavailable = append(out.unavailable, "quality_landings_continuation_requires_secret_keys")
	}
	worked, partial, unavailable, err := s.readAnalyticsWorkedPopulationTenant(ctx, scope, w)
	if err != nil {
		return out, err
	}
	out.events = append(out.events, worked...)
	out.partial = out.partial || partial
	out.unavailable = append(out.unavailable, unavailable...)
	return out, nil
}

func (s *Service) readAnalyticsWorkedPopulationTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow) ([]qualityHistoryEvent, bool, []string, error) {
	out := []qualityHistoryEvent{}
	unavailable := []string{}
	after, cursor, key, err := s.analyticsAttemptCursor(ctx, scope, w, "", "quality_worked")
	if err != nil {
		return out, false, nil, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, false, nil, workItemStorageUnavailable()
	}
	var page reportstore.AnalyticsAttemptPage
	unknownEvents := map[string]eventstore.Event{}
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(reportstore.AnalyticsAttemptReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsAttemptPage(ctx, reportstore.AnalyticsAttemptQuery{ProjectID: scope.project, Principal: principal, RunnerIDs: analyticsCohortRunners(ctx), From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after, StartedInWindow: true, OldestFirst: true})
		if err != nil {
			return err
		}
		unknown := []string{}
		for _, row := range page.Items {
			if analyticsCohortAllows(ctx, row.WorkItemID) && (row.Role == nil || *row.Role == "") {
				unknown = append(unknown, row.AttemptID)
			}
		}
		if len(unknown) == 0 {
			return nil
		}
		batch, ok := r.(eventstore.RunEventBatchReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		events, err := batch.LatestRunEvents(ctx, scope.project, unknown)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.AttemptID == "" {
				return artifact.ErrIntegrity
			}
			if _, duplicate := unknownEvents[event.AttemptID]; duplicate {
				return artifact.ErrIntegrity
			}
			unknownEvents[event.AttemptID] = event
		}
		return nil
	})
	if err != nil {
		return out, false, nil, err
	}
	partial := page.Partial
	for _, row := range page.Items {
		if row.ProjectID != scope.project {
			return out, false, nil, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		role := stringValue(row.Role)
		if role == "" {
			event, ok := unknownEvents[row.AttemptID]
			if !ok {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
			if event.WorkItemID != row.WorkItemID {
				return out, false, nil, artifact.ErrIntegrity
			}
			value, _, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).AnalyticsEvent(ctx, event)
			if errors.Is(err, artifact.ErrMissing) {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
			if err != nil {
				return out, false, nil, err
			}
			if value.Data.Run == nil || value.Data.Run.AttemptID != row.AttemptID {
				return out, false, nil, artifact.ErrIntegrity
			}
			if value.Data.Run.Runtime != nil {
				role = value.Data.Run.Runtime.Identity.Role
			}
			if role == "" && value.Data.Run.Identity != nil {
				role = value.Data.Run.Identity.Role
			}
			if role == "" {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
		}
		if role == "code" || role == "rework" {
			out = append(out, qualityHistoryEvent{Item: string(row.WorkItemID), Kind: "worked", At: row.StartedAt})
		}
	}
	if page.Next.ID != "" {
		if len(key) == 0 {
			partial = true
			unavailable = append(unavailable, "quality_worked_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsAttemptCursor(cursor, key, page.Next, "quality_worked")
			if err != nil {
				return out, false, nil, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.next["quality_worked"] = token
			}
		}
	}
	return out, partial, slices.Compact(unavailable), nil
}

func (s *Service) readAnalyticsQualityTenant(ctx context.Context, scope nativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (analyticsQuality, error) {
	inputs, err := s.readAnalyticsQualityInputsTenant(ctx, scope, w)
	if err != nil {
		return analyticsQuality{}, err
	}
	return projectAnalyticsQualityInputs(inputs, r, w)
}
