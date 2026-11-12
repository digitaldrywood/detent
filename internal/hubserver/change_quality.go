package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readQualityLanding(ctx context.Context, q nativeQueryer, change tracker.ChangeRequest, version string) (*tracker.ChangeLanding, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT l.record_json FROM change_versions v JOIN quality_landings l ON l.version_id=v.id WHERE v.id=? AND v.change_id=?`, version, change.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if change.Landed != nil && change.Landed.VersionID == version {
			return change.Landed, nil
		}
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var landing tracker.ChangeLanding
	err = decodeChangeColumnsJSON(ctx, []byte(raw), &landing)
	return &landing, err
}

func readLandingQualitySnapshot(ctx context.Context, q nativeQueryer, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, at time.Time) (*tracker.LandingQualitySnapshot, error) {
	issue, _, err := readNativeIssue(ctx, q, scope, string(change.WorkItemID))
	if err != nil {
		return nil, err
	}
	contract, err := config.ResolvePolicyIssueContract(version.Policy)
	if err != nil {
		return nil, err
	}
	snapshot := &tracker.LandingQualitySnapshot{IssueRevision: issue.Revision, IssueBody: issue.Body, Criteria: contract.AcceptanceCriteria(issue.Body), Missing: contract.Evaluate(nativeContractIssue(issue)).Missing, Evidence: []gate.CriterionEvidence{}}
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence DESC LIMIT ?`, change.ID, version.ID, maxAnalyticsPopulation+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var review tracker.ChangeReview
		if err := decodeStorageJSON(ctx, []byte(raw), &review); err != nil {
			return nil, err
		}
		if review.CreatedAt.After(at) || review.Decision != "approved" || review.Validator == nil {
			continue
		}
		snapshot.ReviewID = review.ID
		snapshot.Evidence = review.Validator.CriteriaEvidence
		break
	}
	return snapshot, rows.Err()
}

func validateQualityEscape(ctx context.Context, q nativeQueryer, scope nativeScope, change tracker.ChangeRequest, request tracker.DiscussChange, now time.Time, prepared *preparedQualityEscape) error {
	escape := request.Escape
	landing, err := readQualityLanding(ctx, q, change, request.VersionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if landing == nil || request.VersionID != landing.VersionID || escape.ObservedAt.Before(landing.LandedAt) || escape.ObservedAt.After(now) {
		return nativePublicInvalid("Escape must identify a landed version and an occurrence between landing and now")
	}
	if err := escape.Validate(); err != nil {
		return nativeInvalid(err.Error())
	}
	if escape.HumanOverride && (scope.credential.SessionHash == "" || scope.actor().Kind != "human") {
		return nativePublicInvalid("Human classification overrides require a human browser session")
	}
	if escape.Cause != "infrastructure" && !escape.HumanOverride {
		if err := validateQualityBasis(landing.Quality, *escape); err != nil {
			return err
		}
	}
	if escape.Kind == "revert" && ((!changerequest.ValidHash(escape.OccurrenceID, 40) && !changerequest.ValidHash(escape.OccurrenceID, 64)) || !strings.Contains(escape.EvidenceQuote, landing.MergeSHA)) {
		return nativePublicInvalid("Revert occurrence must identify the revert commit and quote its reference to the landed commit")
	}
	if escape.Kind == "scheduled_failure" {
		if err := validateScheduledEscape(ctx, q, scope, *escape, prepared); err != nil {
			return err
		}
	}
	if escape.Kind == "reopened" {
		valid, err := qualityReopenOccurrence(ctx, q, scope, string(change.WorkItemID), *escape)
		if err != nil {
			return err
		}
		if !valid {
			return nativePublicInvalid("Reopen occurrence must reference its recorded Done transition")
		}
	}
	previous, err := readEscapeOccurrence(ctx, q, change.ID, landing.VersionID, escape.OccurrenceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var discussion tracker.ChangeDiscussion
		if err := decodeChangeColumnsJSON(ctx, []byte(previous), &discussion); err != nil {
			return err
		}
		prior := discussion.Escape
		if prior == nil {
			return nativePublicInvalid("Escape occurrence assessment is unavailable")
		}
		if prior.Kind != escape.Kind || !prior.ObservedAt.Equal(escape.ObservedAt) || prior.EvidenceReference != escape.EvidenceReference {
			return nativePublicInvalid("Escape occurrence identity cannot change")
		}
		if prior.HumanOverride && !escape.HumanOverride {
			return nativePublicInvalid("An agent assessment cannot replace a human override")
		}
	}
	return nil
}

func validateQualityBasis(snapshot *tracker.LandingQualitySnapshot, escape tracker.QualityEscape) error {
	if snapshot == nil {
		return nativePublicInvalid("Landing contract snapshot unavailable; a human must classify historical evidence")
	}
	switch escape.Cause {
	case "validator_miss":
		for _, evidence := range snapshot.Evidence {
			if evidence.Criterion == escape.Criterion && (strings.Contains(evidence.Reference, escape.BasisQuote) || strings.Contains(evidence.Behavior, escape.BasisQuote)) {
				return nil
			}
		}
		return nativePublicInvalid("Validator miss must name a covered criterion and quote its landing evidence")
	case "missing_criterion":
		if len(snapshot.Missing) > 0 || len(snapshot.Criteria) == 0 || escape.Criterion != "" {
			return nativePublicInvalid("Missing criterion requires a complete contract and no covered criterion")
		}
	case "underspecified_issue":
		for _, section := range snapshot.Missing {
			if escape.BasisQuote == "Missing required section: "+section {
				return nil
			}
		}
	}
	if !strings.Contains(snapshot.IssueBody, escape.BasisQuote) {
		return nativePublicInvalid("Classification must quote the frozen issue contract")
	}
	return nil
}

func validateScheduledEscape(ctx context.Context, q nativeQueryer, scope nativeScope, escape tracker.QualityEscape, prepared *preparedQualityEscape) error {
	rows, err := q.QueryContext(ctx, `SELECT id,body,0 FROM issues WHERE organization_id=? AND project_id=? AND instr(body,'detent-checks')>0 AND instr(body,?)>0
UNION ALL SELECT id,body,1 FROM native_comments WHERE organization_id=? AND project_id=? AND instr(body,'detent-checks')>0 AND instr(body,?)>0 LIMIT ?`, scope.organization, scope.project, escape.EvidenceReference, scope.organization, scope.project, escape.EvidenceReference, maxAnalyticsPopulation+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body string
		var comment bool
		if err := rows.Scan(&id, &body, &comment); err != nil {
			return err
		}
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !strings.Contains(body, escape.EvidenceQuote) {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if scheduled.OccurrenceKey == escape.OccurrenceID && scheduled.RunURL == escape.EvidenceReference && scheduledRepairOriginMatches(origin.Source, scheduled) && scheduled.Conclusion == "failure" {
				prepared.scheduled = &preparedScheduledEscape{id: id, body: body, comment: comment}
				return nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nativePublicInvalid("Scheduled escape must quote an existing red scheduled-suite occurrence; candidate commits alone do not establish attribution")
}

type preparedQualityEscape struct {
	version, occurrence string
	snapshot            qualityEscapeSnapshot
	scheduled           *preparedScheduledEscape
}

type preparedScheduledEscape struct {
	id, body string
	comment  bool
}

type qualityEscapeSnapshot struct {
	version, landing, event, from, to, at string
}

func readQualityEscapeSnapshot(ctx context.Context, q nativeQueryer, scope nativeScope, change string, prepared *preparedQualityEscape) (qualityEscapeSnapshot, error) {
	var snapshot qualityEscapeSnapshot
	err := q.QueryRowContext(ctx, `SELECT v.record_json,COALESCE(l.record_json,''),COALESCE(e.data_json,''),COALESCE(e.from_state,''),COALESCE(e.to_state,''),COALESCE(e.recorded_at,'')
FROM change_versions v LEFT JOIN quality_landings l ON l.version_id=v.id
LEFT JOIN collaboration_events e ON e.organization_id=? AND e.project_id=? AND e.work_item_id=(SELECT work_item_id FROM change_requests WHERE id=v.change_id) AND e.id=? AND e.type='workflow.transitioned'
WHERE v.change_id=? AND v.id=?`, scope.organization, scope.project, prepared.occurrence, change, prepared.version).Scan(&snapshot.version, &snapshot.landing, &snapshot.event, &snapshot.from, &snapshot.to, &snapshot.at)
	return snapshot, err
}

func recheckQualityEscape(ctx context.Context, q nativeQueryer, scope nativeScope, prepared *changeMutationPreparation) error {
	quality := prepared.quality
	current, err := readQualityEscapeSnapshot(ctx, q, scope, prepared.change.ID, quality)
	if err != nil {
		return err
	}
	if current != quality.snapshot {
		return nativeConflict(prepared.change.Revision)
	}
	if witness := quality.scheduled; witness != nil {
		statement := `SELECT body FROM issues WHERE organization_id=? AND project_id=? AND id=?`
		if witness.comment {
			statement = `SELECT body FROM native_comments WHERE organization_id=? AND project_id=? AND id=?`
		}
		var body string
		err := q.QueryRowContext(ctx, statement, scope.organization, scope.project, witness.id).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) || err == nil && body != witness.body {
			return nativeConflict(prepared.change.Revision)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type tenantQualityWitness struct {
	item    corestore.WorkItem
	comment *corestore.Comment
	version eventstore.CollaborationVersion
}

type preparedTenantQualityEscape struct {
	landing   eventstore.VersionLanding
	previous  *eventstore.Evidence
	scheduled *tenantQualityWitness
	reopen    *eventstore.Event
}

func (s *Service) prepareQualityEscapeTenant(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, request tracker.DiscussChange, now time.Time) (*preparedTenantQualityEscape, error) {
	escape := request.Escape
	if escape == nil {
		return nil, nil
	}
	if err := escape.Validate(); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	prepared := &preparedTenantQualityEscape{}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, workItemStorageUnavailable()
	}
	var previous eventstore.Evidence
	var project projectstore.Record
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		owner, ok := r.(eventstore.VersionLandingReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		prepared.landing, err = owner.VersionLanding(ctx, change.ID, request.VersionID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nativePublicInvalid("Escape must identify a landed version and an occurrence between landing and now")
		}
		if err != nil {
			return err
		}
		if prepared.landing.ProjectID != scope.project || prepared.landing.WorkItemID != change.WorkItemID {
			return artifact.ErrIntegrity
		}
		occurrences, ok := r.(eventstore.QualityEscapeReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		previous, err = occurrences.QualityEscapeOccurrence(ctx, change.ID, request.VersionID, escape.OccurrenceID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if previous.ChangeID != "" {
			prepared.previous = &previous
		}
		if escape.Kind == "reopened" {
			event, err := r.RunEvent(ctx, scope.project, change.WorkItemID, escape.OccurrenceID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativePublicInvalid("Reopen occurrence must reference its recorded Done transition")
				}
				return err
			}
			prepared.reopen = &event
			project, err = r.ProjectRecord(ctx, scope.project)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	landing, err := content.VersionLanding(ctx, prepared.landing)
	if err != nil {
		return nil, err
	}
	if escape.ObservedAt.Before(landing.LandedAt) || escape.ObservedAt.After(now) {
		return nil, nativePublicInvalid("Escape must identify a landed version and an occurrence between landing and now")
	}
	if escape.HumanOverride && (scope.credential.SessionHash == "" || scope.actor().Kind != "human") {
		return nil, nativePublicInvalid("Human classification overrides require a human browser session")
	}
	if escape.Cause != "infrastructure" && !escape.HumanOverride {
		if err := validateQualityBasis(landing.Quality, *escape); err != nil {
			return nil, err
		}
	}
	if escape.Kind == "revert" && ((!changerequest.ValidHash(escape.OccurrenceID, 40) && !changerequest.ValidHash(escape.OccurrenceID, 64)) || !strings.Contains(escape.EvidenceQuote, landing.MergeSHA)) {
		return nil, nativePublicInvalid("Revert occurrence must identify the revert commit and quote its reference to the landed commit")
	}
	if prepared.reopen != nil {
		event, err := content.Event(ctx, *prepared.reopen)
		if err != nil {
			return nil, err
		}
		valid := event.Type == "workflow.transitioned" && strings.EqualFold(event.Data.FromState, "Done") && event.RecordedAt.Equal(escape.ObservedAt)
		target := false
		for _, state := range project.States {
			if state.Name == event.Data.ToState && !state.Terminal {
				target = true
			}
		}
		if !valid || !target {
			return nil, nativePublicInvalid("Reopen occurrence must reference its recorded Done transition")
		}
	}
	if prepared.previous != nil {
		raw, err := content.Evidence(ctx, *prepared.previous)
		if err != nil {
			return nil, err
		}
		var discussion tracker.ChangeDiscussion
		if json.Unmarshal(raw, &discussion) != nil || discussion.Escape == nil {
			return nil, artifact.ErrIntegrity
		}
		prior := discussion.Escape
		if prior.Kind != escape.Kind || !prior.ObservedAt.Equal(escape.ObservedAt) || prior.EvidenceReference != escape.EvidenceReference {
			return nil, nativePublicInvalid("Escape occurrence identity cannot change")
		}
		if prior.HumanOverride && !escape.HumanOverride {
			return nil, nativePublicInvalid("An agent assessment cannot replace a human override")
		}
	}
	if escape.Kind == "scheduled_failure" {
		witness, err := s.prepareScheduledQualityWitness(ctx, scope, *escape)
		if err != nil {
			return nil, err
		}
		prepared.scheduled = witness
	}
	return prepared, nil
}

func (s *Service) prepareScheduledQualityWitness(ctx context.Context, scope nativeScope, escape tracker.QualityEscape) (*tenantQualityWitness, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, workItemStorageUnavailable()
	}
	candidates := []tenantQualityWitness{}
	clipped := false
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		cursor := corestore.ScanCursor{}
		items := []corestore.WorkItem{}
		for len(items) < maxAnalyticsPopulation {
			page, err := r.ScanWorkItems(ctx, scope.project, cursor, min(corestore.MaxPageSize, maxAnalyticsPopulation-len(items)))
			if err != nil {
				return err
			}
			items = append(items, page.Items...)
			if page.Next == (corestore.ScanCursor{}) {
				break
			}
			cursor = page.Next
			if len(items) == maxAnalyticsPopulation {
				clipped = true
			}
		}
		for _, item := range items {
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				continue
			}
			candidates = append(candidates, tenantQualityWitness{item: item})
		}
		budget := maxAnalyticsPopulation - len(items)
		for _, item := range items {
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				continue
			}
			cursor = corestore.ScanCursor{}
			for {
				if budget == 0 {
					clipped = true
					break
				}
				page, err := r.ScanComments(ctx, scope.project, item.NativeID, cursor, min(corestore.MaxPageSize, budget))
				if err != nil {
					return err
				}
				budget -= len(page.Items)
				for _, comment := range page.Items {
					if comment.Deleted {
						continue
					}
					candidates = append(candidates, tenantQualityWitness{item: item, comment: new(comment)})
				}
				if page.Next == (corestore.ScanCursor{}) {
					break
				}
				cursor = page.Next
			}
		}
		owner, ok := r.(eventstore.CollaborationVersionsReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		for start := 0; start < len(candidates); start += corestore.MaxPageSize {
			end := min(len(candidates), start+corestore.MaxPageSize)
			keys := make([]eventstore.CollaborationVersionKey, 0, end-start)
			for _, candidate := range candidates[start:end] {
				key := eventstore.CollaborationVersionKey{RecordID: string(candidate.item.NativeID), Revision: candidate.item.Revision}
				if candidate.comment != nil {
					key.RecordID, key.Revision = candidate.comment.ID, candidate.comment.Revision
				}
				keys = append(keys, key)
			}
			versions, err := owner.CollaborationVersions(ctx, keys)
			if err != nil {
				return err
			}
			byKey := map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion{}
			for _, version := range versions {
				byKey[eventstore.CollaborationVersionKey{RecordID: version.RecordID, Revision: version.Revision}] = version
			}
			for index, key := range keys {
				version, ok := byKey[key]
				if !ok {
					return artifact.ErrIntegrity
				}
				candidates[start+index].version = version
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		body := ""
		if candidate.comment == nil {
			issue, err := s.hydrateCanonicalWorkItemRecord(ctx, candidate.item, candidate.version)
			if err != nil {
				return nil, err
			}
			body = issue.Body
		} else {
			comment, err := s.hydrateCommentRecord(ctx, *candidate.comment, candidate.version)
			if err != nil {
				return nil, err
			}
			body = comment.Body
		}
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !strings.Contains(body, escape.EvidenceQuote) {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if scheduled.OccurrenceKey == escape.OccurrenceID && scheduled.RunURL == escape.EvidenceReference && scheduledRepairOriginMatches(origin.Source, scheduled) && scheduled.Conclusion == "failure" {
				return &candidate, nil
			}
		}
	}
	if clipped {
		return nil, reportstore.ErrPopulationExceeded
	}
	return nil, nativePublicInvalid("Scheduled escape must quote an existing red scheduled-suite occurrence; candidate commits alone do not establish attribution")
}

func recheckTenantQualityEscape(ctx context.Context, w tenantstore.Writer, scope nativeScope, change tracker.ChangeRequest, escape tracker.QualityEscape, prepared *preparedTenantQualityEscape) error {
	owner, ok := w.(eventstore.VersionLandingReader)
	if !ok {
		return workItemStorageUnavailable()
	}
	landing, err := owner.VersionLanding(ctx, change.ID, prepared.landing.VersionID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(landing, prepared.landing) {
		return corestore.ErrRevisionConflict
	}
	occurrences, ok := w.(eventstore.QualityEscapeReader)
	if !ok {
		return workItemStorageUnavailable()
	}
	previous, err := occurrences.QualityEscapeOccurrence(ctx, change.ID, prepared.landing.VersionID, escape.OccurrenceID)
	if prepared.previous == nil {
		if err == nil {
			return corestore.ErrRevisionConflict
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
	} else if err != nil {
		return err
	} else if !reflect.DeepEqual(previous, *prepared.previous) {
		return corestore.ErrRevisionConflict
	}
	if witness := prepared.scheduled; witness != nil {
		item, err := w.WorkItem(ctx, scope.project, witness.item.NativeID)
		if err != nil {
			return err
		}
		if item.Revision != witness.item.Revision || !reflect.DeepEqual(item.BodyObjectKey, witness.item.BodyObjectKey) || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
			return corestore.ErrRevisionConflict
		}
		if witness.comment != nil {
			comment, err := w.Comment(ctx, scope.project, item.NativeID, witness.comment.ID)
			if err != nil {
				return err
			}
			if comment.Revision != witness.comment.Revision || comment.BodyObjectKey != witness.comment.BodyObjectKey || comment.Deleted {
				return corestore.ErrRevisionConflict
			}
		}
	}
	if prepared.reopen != nil {
		event, err := w.RunEvent(ctx, scope.project, change.WorkItemID, prepared.reopen.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(event, *prepared.reopen) {
			return corestore.ErrRevisionConflict
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		for _, state := range project.States {
			if state.Name == event.ToState && !state.Terminal {
				return nil
			}
		}
		return corestore.ErrRevisionConflict
	}
	return nil
}
