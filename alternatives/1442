package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
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
	err = json.Unmarshal([]byte(raw), &landing)
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
	rows, err := q.QueryContext(ctx, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence DESC LIMIT ?`, change.ID, version.ID, maxAnalyticsPopulation+1)
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
		if err := json.Unmarshal([]byte(raw), &review); err != nil {
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

func validateQualityEscape(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, request tracker.DiscussChange, now time.Time) error {
	escape := request.Escape
	landing, err := readQualityLanding(ctx, tx, change, request.VersionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if landing == nil || request.VersionID != landing.VersionID || escape.ObservedAt.Before(landing.LandedAt) || escape.ObservedAt.After(now) {
		return nativeInvalid("Escape must identify a landed version and an occurrence between landing and now")
	}
	if err := escape.Validate(); err != nil {
		return nativeInvalid(err.Error())
	}
	if escape.HumanOverride && (scope.credential.SessionHash == "" || scope.actor().Kind != "human") {
		return nativeInvalid("Human classification overrides require a human browser session")
	}
	if escape.Cause != "infrastructure" && !escape.HumanOverride {
		if err := validateQualityBasis(landing.Quality, *escape); err != nil {
			return err
		}
	}
	if escape.Kind == "revert" && ((!changerequest.ValidHash(escape.OccurrenceID, 40) && !changerequest.ValidHash(escape.OccurrenceID, 64)) || !strings.Contains(escape.EvidenceQuote, landing.MergeSHA)) {
		return nativeInvalid("Revert occurrence must identify the revert commit and quote its reference to the landed commit")
	}
	if escape.Kind == "scheduled_failure" {
		if err := validateScheduledEscape(ctx, tx, scope, *escape); err != nil {
			return err
		}
	}
	if escape.Kind == "reopened" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND id=? AND type='workflow.transitioned' AND lower(json_extract(data_json,'$.from_state'))='done' AND EXISTS (SELECT 1 FROM workflow_states ws WHERE ws.project_id=collaboration_events.project_id AND ws.detent_state=json_extract(data_json,'$.to_state') AND ws.terminal=0) AND julianday(recorded_at)=julianday(?)`, scope.organization, scope.project, change.WorkItemID, escape.OccurrenceID, formatHubTime(escape.ObservedAt)).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return nativeInvalid("Reopen occurrence must reference its recorded Done transition")
		}
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='discussion' AND json_extract(record_json,'$.escape.occurrence_id')=? ORDER BY sequence DESC LIMIT 1`, change.ID, landing.VersionID, escape.OccurrenceID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var discussion tracker.ChangeDiscussion
		if err := json.Unmarshal([]byte(previous), &discussion); err != nil {
			return err
		}
		prior := discussion.Escape
		if prior.Kind != escape.Kind || !prior.ObservedAt.Equal(escape.ObservedAt) || prior.EvidenceReference != escape.EvidenceReference {
			return nativeInvalid("Escape occurrence identity cannot change")
		}
		if prior.HumanOverride && !escape.HumanOverride {
			return nativeInvalid("An agent assessment cannot replace a human override")
		}
	}
	return nil
}

func validateQualityBasis(snapshot *tracker.LandingQualitySnapshot, escape tracker.QualityEscape) error {
	if snapshot == nil {
		return nativeInvalid("Landing contract snapshot unavailable; a human must classify historical evidence")
	}
	switch escape.Cause {
	case "validator_miss":
		for _, evidence := range snapshot.Evidence {
			if evidence.Criterion == escape.Criterion && (strings.Contains(evidence.Reference, escape.BasisQuote) || strings.Contains(evidence.Behavior, escape.BasisQuote)) {
				return nil
			}
		}
		return nativeInvalid("Validator miss must name a covered criterion and quote its landing evidence")
	case "missing_criterion":
		if len(snapshot.Missing) > 0 || len(snapshot.Criteria) == 0 || escape.Criterion != "" {
			return nativeInvalid("Missing criterion requires a complete contract and no covered criterion")
		}
	case "underspecified_issue":
		for _, section := range snapshot.Missing {
			if escape.BasisQuote == "Missing required section: "+section {
				return nil
			}
		}
	}
	if !strings.Contains(snapshot.IssueBody, escape.BasisQuote) {
		return nativeInvalid("Classification must quote the frozen issue contract")
	}
	return nil
}

func validateScheduledEscape(ctx context.Context, q nativeQueryer, scope nativeScope, escape tracker.QualityEscape) error {
	rows, err := q.QueryContext(ctx, `SELECT body FROM issues WHERE organization_id=? AND project_id=? AND instr(body,'detent-checks')>0 AND instr(body,?)>0
UNION ALL SELECT body FROM native_comments WHERE organization_id=? AND project_id=? AND instr(body,'detent-checks')>0 AND instr(body,?)>0 LIMIT ?`, scope.organization, scope.project, escape.EvidenceReference, scope.organization, scope.project, escape.EvidenceReference, maxAnalyticsPopulation+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return err
		}
		origin, ok := issueorigin.Parse(body)
		if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !strings.Contains(body, escape.EvidenceQuote) {
			continue
		}
		for _, scheduled := range gate.ParseScheduledEvidence(body) {
			if scheduled.OccurrenceKey == escape.OccurrenceID && scheduled.RunURL == escape.EvidenceReference && origin.Source == scheduled.RunURL && scheduled.Conclusion == "failure" {
				return nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nativeInvalid("Scheduled escape must quote an existing red scheduled-suite occurrence; candidate commits alone do not establish attribution")
}
