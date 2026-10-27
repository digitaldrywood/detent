package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// landingLane is the lane a reviewed Change Request waits in for the runner
// that lands it. It is dispatchable so the runner claims the item, and it is
// named rather than inferred so a workflow without one keeps its reviewed
// changes in review, where a person lands them by hand.
const landingLane = "Merging"

// landingTarget is the lane an approved review moves the primary issue to:
// the landing lane, when the issue's current lane may move there. It reports
// "" when the workflow has no such move.
func landingTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && strings.EqualFold(target.Name, landingLane) && target.Dispatchable && !target.Terminal && !target.OperatorOnly {
					return target.Name
				}
			}
		}
	}
	return ""
}

// terminalTarget is the lane a landed issue finishes in: the first terminal
// lane the current lane may move to.
func terminalTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && target.Terminal {
					return target.Name
				}
			}
		}
	}
	return ""
}

// promoteReviewedChange moves the change's primary issue to the landing lane
// once its current version is reviewed. It runs after every write that can
// complete the evidence: an approval, a check result, and a version publish
func promoteReviewedChange(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, now time.Time) error {
	detail, err := readChangeDetail(ctx, tx, scope, string(change.WorkItemID), change.ID, now)
	if err != nil {
		return err
	}
	if detail.Summary.Status != "reviewed" {
		return nil
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
	if err != nil {
		return err
	}
	reviewSource, err := nativeChangeReviewSource(ctx, tx, scope, issue.State)
	if err != nil {
		return err
	}
	if issue.Archived || issue.Terminal || !issue.DependencyReady || !reviewSource {
		return nil
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	for _, state := range project.States {
		if state.Name == issue.State && state.Dispatchable {
			return nil
		}
	}
	target := landingTarget(project, issue.State)
	if target == "" || target == issue.State {
		return nil
	}
	from := issue.State
	issue.State = target
	_, err = persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "user_requested"}, now)
	return err
}

// reworkTarget is the lane a request for changes moves the primary issue
// to: a dispatchable, non-terminal lane the current lane may move to, where
// the runner picks the item up again. "In Progress" is preferred when the
// workflow has it, so the item resumes rather than restarts.
func reworkTarget(project tracker.NativeProject, current string) string {
	first := ""
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name != name || !target.Dispatchable || target.Terminal || target.OperatorOnly {
					continue
				}
				if strings.EqualFold(target.Name, "In Progress") {
					return target.Name
				}
				if first == "" {
					first = target.Name
				}
			}
		}
	}
	return first
}

// returnChangeForRework sends the change's primary issue back to work when
// changes are requested on the current version: the review text lands on
// the issue as the next run's instructions, and an issue waiting in review
// or in the landing lane moves to a working lane. A decision on an older
// version is recorded on the change and moves nothing; a finished issue is
// left alone; an issue already being worked stays where it is.
func returnChangeForRework(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, review tracker.ChangeReview, now time.Time) error {
	if review.VersionID != change.CurrentVersion {
		return nil
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
	if err != nil {
		return err
	}
	if issue.Terminal {
		return nil
	}
	if err := appendReviewInstructions(ctx, tx, scope, issue, change, review, now); err != nil {
		return err
	}
	return routeChangeForRework(ctx, tx, scope, issue, now)
}

func routeChangeForRework(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, now time.Time) error {
	reviewSource, err := nativeChangeReviewSource(ctx, tx, scope, issue.State)
	if err != nil {
		return err
	}
	if issue.Archived || !issue.DependencyReady || !reviewSource && !strings.EqualFold(issue.State, landingLane) {
		return nil
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	// An item already being worked stays; one waiting to land leaves the
	// landing lane, since the review it was landing on is withdrawn.
	for _, state := range project.States {
		if state.Name == issue.State && state.Dispatchable && !strings.EqualFold(state.Name, landingLane) {
			return nil
		}
	}
	target := reworkTarget(project, issue.State)
	if target == "" || target == issue.State {
		return nil
	}
	from := issue.State
	issue.State = target
	_, err = persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "user_requested"}, now)
	return err
}

func nativeChangeReviewSource(ctx context.Context, q nativeQueryer, scope nativeScope, state string) (bool, error) {
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(state, cfg.Agent.AutoPromote.SourceState) {
		return true, nil
	}
	approval, err := requireProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return false, err
	}
	return approval.Policy.Configuration == nil && approval.Policy.Authored == nil && (strings.EqualFold(state, "In Review") || strings.EqualFold(state, "Review")), nil
}

// appendReviewInstructions puts the review text on the primary issue as a
// comment by the reviewer, where the next run reads it with the rest of the
// discussion. The change's own review record stays the decision of record.
func appendReviewInstructions(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, change tracker.ChangeRequest, review tracker.ChangeReview, now time.Time) error {
	_, err := insertNativeComment(ctx, tx, scope, issue, reviewInstructions(change, review), nil, now)
	return err
}

// maxNativeCommentBytes is the comment body limit createNativeComment
// enforces; the generated header counts against it.
const maxNativeCommentBytes = 64 << 10

// reviewInstructions is the comment a request for changes leaves on the
// issue: a header naming the change and version, then the review text, cut
// at a character boundary so the whole comment stays within the limit a
// comment may be edited under.
func reviewInstructions(change tracker.ChangeRequest, review tracker.ChangeReview) string {
	body := "Changes requested on Change Request " + change.ID
	if review.VersionID != "" {
		body += " (version " + review.VersionID + ")"
	}
	body += "."
	text := strings.TrimSpace(review.Body)
	if text == "" {
		return body
	}
	body += "\n\n"
	room := maxNativeCommentBytes - len(body)
	if len(text) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return body + text
}

func (s *Service) landChange(c echo.Context) error {
	var request tracker.LandChangeVersion
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		change, err := readChange(ctx, tx, scope, c.Param("item"), c.Param("change"))
		if err != nil {
			return nil, err
		}
		if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
			return nil, nativePublicInvalid("Land through the Change Request's primary issue")
		}
		version, err := readChangeVersion(ctx, tx, change.ID, c.Param("version"))
		if err != nil {
			return nil, err
		}
		if !changerequest.ValidHash(request.MergeSHA, 40) && !changerequest.ValidHash(request.MergeSHA, 64) {
			return nil, nativePublicInvalid("The landed commit must be a lowercase commit identity")
		}
		if strings.TrimSpace(request.BaseRef) == "" || len(request.BaseRef) > 256 || !slices.Contains([]string{"squash", "merge", "rebase"}, request.Method) {
			return nil, nativePublicInvalid("Landing names the base branch and a merge method of squash, merge or rebase")
		}
		delivery, associated, err := readReleaseDelivery(ctx, tx, scope, string(change.WorkItemID))
		if err != nil {
			return nil, err
		}
		if associated && (delivery.ChangeID != change.ID || delivery.VersionID != version.ID || delivery.HeadSHA != version.HeadSHA || delivery.BaseRef != request.BaseRef) {
			return nil, nativePublicInvalid("Landing must cover the associated release version and branch")
		}
		if landed := change.CurrentLanding(); landed != nil {
			if landed.VersionID == version.ID && landed.MergeSHA == request.MergeSHA {
				return change, nil
			}
			return nil, nativeConflict(change.Revision)
		}
		if change.CurrentVersion != version.ID {
			return nil, nativeConflict(change.Revision)
		}
		detail, err := readChangeDetail(ctx, tx, scope, string(change.WorkItemID), change.ID, now)
		if err != nil {
			return nil, err
		}
		if detail.Summary.Status != "reviewed" {
			return nil, &nativeError{Code: "not_reviewed", Message: "Only a reviewed current version lands: " + strings.Join(detail.Summary.Messages, " "), status: 409}
		}
		if associated && (request.Receipt == nil || request.Receipt.BaseSHA != delivery.BaseSHA || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA) {
			return nil, nativePublicInvalid("Release landing must retain the reviewed base and head")
		}
		if version.Policy.Gates.SecurityAudit && !version.Policy.Gates.GitHubPullRequest && !associated {
			if request.Receipt == nil || request.Receipt.SecurityAudit == nil || request.Receipt.BaseSHA == "" || request.Receipt.MergeSHA != request.MergeSHA {
				return nil, nativePublicInvalid("landing requires security audit evidence for its exact base and head")
			}
			audit := request.Receipt.SecurityAudit
			securityaudit.NormalizeAttribution(&audit.Run)
			run := audit.Run
			evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: string(scope.project), Repository: version.Repository, PRNumber: run.PRNumber, BaseSHA: request.Receipt.BaseSHA, HeadSHA: request.MergeSHA}, "", []string{"none"})
			auditRaw, err := marshalNative(audit)
			if err != nil {
				return nil, err
			}
			recorded := false
			for _, review := range detail.Reviews {
				if review.VersionID != version.ID || review.SecurityAudit == nil || !review.SecurityAudit.Landing || review.Decision != "approved" {
					continue
				}
				reviewRaw, err := marshalNative(review.SecurityAudit)
				if err != nil {
					return nil, err
				}
				if auditRaw == reviewRaw {
					recorded = true
				}
			}
			if !evaluation.Allowed || audit.Verdict != "pass" || !audit.Landing || !recorded {
				return nil, nativePublicInvalid("landing security audit does not match recorded approval for the integrated change")
			}
		}
		quality, err := readLandingQualitySnapshot(ctx, tx, scope, change, version, now)
		if err != nil {
			return nil, err
		}
		change.Landed = &tracker.ChangeLanding{Quality: quality, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: request.MergeSHA, BaseRef: request.BaseRef, Method: request.Method, Actor: scope.actor(), LandedAt: now, Rebased: request.Rebased}
		qualityRaw, err := marshalNative(change.Landed)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO quality_landings (version_id,record_json) VALUES (?,?)", version.ID, qualityRaw); err != nil {
			return nil, err
		}
		change.UpdatedAt = now
		change.Revision++
		raw, err := marshalNative(change)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE change_requests SET record_json = ?, claim_version_id = ?, claim_head_sha = ?, landed_version_id=? WHERE id = ?", raw, version.ID, version.HeadSHA, change.Landed.VersionID, change.ID); err != nil {
			return nil, err
		}
		if err := writeArchiveChangeFacts(ctx, tx, change, ""); err != nil {
			return nil, err
		}
		if err := recordPolicyLandingProvenance(ctx, tx, string(scope.organization)+"/"+string(scope.project), 0, validationRepository(version.Repository), request.MergeSHA, policyLanding{ChangeID: change.ID}); err != nil {
			return nil, fmt.Errorf("record change policy landing: %w", err)
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
		if err != nil {
			return nil, err
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if !issue.Terminal {
			target := terminalTarget(project, issue.State)
			if target == "" {
				return nil, nativeInvalid("The workflow allows no move from " + issue.State + " to a terminal lane")
			}
			from := issue.State
			issue.State = target
			if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "worker_progress"}, now); err != nil {
				return nil, err
			}
		}
		return change, nil
	})
}

func routeStaleNativeLandingChanges(ctx context.Context, tx *sql.Tx, policyScope, actor, preservedPolicyID string, revoking bool, now time.Time) error {
	organization, projectID, native := strings.Cut(policyScope, "/")
	if !native || strings.HasPrefix(policyScope, "repository:") {
		return nil
	}
	scope := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(projectID), credential: apiCredential{ID: actor, Scope: apiScopeAdmin}}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if project.Profile != "native" {
		return nil
	}
	items, err := nativePageIDs(ctx, tx, `SELECT i.native_id FROM issues i
JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0 AND ws.terminal = 0
AND lower(trim(ws.detent_state)) = 'merging' ORDER BY i.id`, scope.organization, scope.project)
	if err != nil {
		return err
	}
	currentPolicy, err := readChangeSummaryPolicy(ctx, tx, scope)
	if err != nil {
		return err
	}
	if revoking {
		currentPolicy.approval = policy.Approval{}
	}
	states := make([]connector.WorkflowState, len(project.States))
	for i, lane := range project.States {
		states[i] = connector.WorkflowState{Name: lane.Name, Dispatchable: lane.Dispatchable, Terminal: lane.Terminal, OperatorOnly: lane.OperatorOnly, Transitions: lane.Transitions}
	}
	for _, item := range items {
		change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
		if err != nil {
			return err
		}
		if !found || change.CurrentLanding() != nil || change.CurrentVersion == "" {
			continue
		}
		version, err := readChangeVersionSnapshot(ctx, tx, change.ID, change.CurrentVersion)
		if err != nil {
			return err
		}
		if preservedPolicyID != "" && version.PolicyID == preservedPolicyID {
			continue
		}
		policyID, reviewID := currentPolicy.identities(&version)
		if policyID == version.PolicyID && reviewID == version.ReviewPolicy.ID {
			continue
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return err
		}
		target, ok := connector.LandingRefusalLane(states, issue.State, "Rework", true)
		if !ok {
			return nativeInvalid("The workflow allows no move from " + issue.State + " to a landing refusal lane")
		}
		from := issue.State
		issue.State = target
		if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "stale_policy"}, now); err != nil {
			return err
		}
	}
	return nil
}

func nativeLandingCandidateReady(ctx context.Context, query *sql.Tx, scope *nativeScope, id tracker.WorkItemID, machine tracker.MachineID, capacity int, now time.Time) (ready, evaluated bool, landingCapacity int, err error) {
	if scope == nil {
		return true, false, 0, nil
	}
	var item, state string
	if err := query.QueryRowContext(ctx, "SELECT i.native_id, ws.detent_state FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.id = ?", id).Scan(&item, &state); err != nil {
		return false, false, 0, err
	}
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		_, pending, err := readPendingLandingFact(ctx, query, id)
		if err != nil {
			return false, false, 0, err
		}
		if !pending {
			return true, false, 0, nil
		}
	}
	change, found, err := readClaimChange(ctx, query, *scope, item)
	if err != nil || !found {
		return false, true, 0, err
	}
	if change.CurrentLanding() != nil || change.CurrentVersion == "" {
		return false, true, 0, nil
	}
	version, err := readClaimVersion(ctx, query, change.ID, change.CurrentVersion)
	if err != nil {
		return false, true, 0, err
	}
	detail, err := readClaimLandingDetail(ctx, query, *scope, change, version, now)
	if err != nil {
		return false, true, 0, err
	}
	if !nativeChangeLandingReady("Merging", &detail) {
		return false, true, 0, nil
	}
	if version.External == nil && version.AttemptID != "" {
		checkpoint, err := readClaimCheckpoint(ctx, query, *scope, item, version.AttemptID)
		if err != nil {
			return false, true, 0, err
		}
		if checkpoint != nil && checkpoint.Storage == "local_only" && checkpoint.WorktreeState == "unpushed" && checkpoint.HeadSHA == version.HeadSHA {
			stored, err := readClaimSourceStored(ctx, query, version)
			if err != nil {
				return false, true, 0, err
			}
			var owner tracker.MachineID
			var ownerRunner string
			if err := query.QueryRowContext(ctx, `SELECT l.machine_id,COALESCE(lr.runner_id,'') FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE a.id=? AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, version.AttemptID, scope.organization, scope.project, item).Scan(&owner, &ownerRunner); err != nil {
				return false, true, 0, err
			}
			if !stored && (owner != machine || ownerRunner != "" && ownerRunner != scope.credential.Runner.RunnerID) {
				return false, true, 0, nil
			}
		}
	}
	ready, landingCapacity, err = nativeLandingClaimCapacity(ctx, query, id, capacity, now)
	return ready, true, landingCapacity, err
}

func nativeChangeLandingReady(state string, detail *tracker.ChangeDetail) bool {
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "reviewed"
}
