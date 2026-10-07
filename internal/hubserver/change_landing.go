package hubserver

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/connector"
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
// under a policy that requires no review. An issue in a dispatchable lane
// belongs to the run working it, whose completion moves it, so it is left
// for that run; so is an issue the workflow cannot move. The evidence is
// recorded either way.
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
		return invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		change, err := readChange(ctx, tx, scope, c.Param("item"), c.Param("change"))
		if err != nil {
			return nil, err
		}
		if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
			return nil, nativeInvalid("Land through the Change Request's primary issue")
		}
		version, err := readChangeVersion(ctx, tx, change.ID, c.Param("version"))
		if err != nil {
			return nil, err
		}
		if !changerequest.ValidHash(request.MergeSHA, 40) && !changerequest.ValidHash(request.MergeSHA, 64) {
			return nil, nativeInvalid("The landed commit must be a lowercase commit identity")
		}
		if strings.TrimSpace(request.BaseRef) == "" || len(request.BaseRef) > 256 || !slices.Contains([]string{"squash", "merge", "rebase"}, request.Method) {
			return nil, nativeInvalid("Landing names the base branch and a merge method of squash, merge or rebase")
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
		if err := recordBarrierLanding(ctx, tx, scope, version, change, request, now); err != nil {
			return nil, err
		}
		change.UpdatedAt = now
		change.Revision++
		raw, err := marshalNative(change)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE change_requests SET record_json = ? WHERE id = ?", raw, change.ID); err != nil {
			return nil, err
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

func nativeLandingCandidateReady(ctx context.Context, query *sql.Tx, scope *nativeScope, id tracker.WorkItemID, machine tracker.MachineID, now time.Time, routeStale bool) (ready, evaluated bool, err error) {
	if scope == nil {
		return true, false, nil
	}
	var item, state string
	if err := query.QueryRowContext(ctx, "SELECT i.native_id, ws.detent_state FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.id = ?", id).Scan(&item, &state); err != nil {
		return false, false, err
	}
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		return true, false, nil
	}
	change, found, err := readLatestNativeChangeRequest(ctx, query, *scope, item)
	if err != nil || !found {
		return false, true, err
	}
	if change.CurrentLanding() != nil || change.CurrentVersion == "" {
		return false, true, nil
	}
	detail, err := readCurrentChangeDetail(ctx, query, *scope, change, now)
	if err != nil {
		return false, true, err
	}
	if detail.Summary.Status == "stale_policy" {
		if !routeStale {
			return true, true, nil
		}
		issue, _, err := readNativeIssue(ctx, query, *scope, item)
		if err != nil {
			return false, true, err
		}
		project, err := readNativeProject(ctx, query, *scope)
		if err != nil {
			return false, true, err
		}
		states := make([]connector.WorkflowState, len(project.States))
		for i, lane := range project.States {
			states[i] = connector.WorkflowState{Name: lane.Name, Dispatchable: lane.Dispatchable, Terminal: lane.Terminal, OperatorOnly: lane.OperatorOnly, Transitions: lane.Transitions}
		}
		target, ok := connector.LandingRefusalLane(states, issue.State, "Rework", true)
		if !ok {
			return false, true, nativeInvalid("The workflow allows no move from " + issue.State + " to a landing refusal lane")
		}
		from := issue.State
		issue.State = target
		_, err = persistNativeIssue(ctx, query, *scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: target, Reason: "completed_active_review_transition"}, now)
		return false, true, err
	}
	if !nativeChangeLandingReady(state, &detail) {
		return false, true, nil
	}
	version, err := readChangeVersion(ctx, query, change.ID, change.CurrentVersion)
	if err != nil {
		return false, true, err
	}
	allowed, err := barrierLandingAllowed(ctx, query, *scope, version.Repository, tracker.NativeWorkItemID(item))
	if err != nil || !allowed {
		return false, true, err
	}
	if version.External != nil || version.AttemptID == "" {
		return true, true, nil
	}
	attempt, err := readNativeAttempt(ctx, query, *scope, item, version.AttemptID, now)
	if err != nil {
		return false, true, err
	}
	checkpoint := attempt.Checkpoint
	if checkpoint == nil || checkpoint.Storage != "local_only" || checkpoint.WorktreeState != "unpushed" || checkpoint.HeadSHA != version.HeadSHA {
		return true, true, nil
	}
	return attempt.MachineID == machine && (attempt.RunnerID == "" || attempt.RunnerID == scope.credential.Runner.RunnerID), true, nil
}

func nativeChangeLandingReady(state string, detail *tracker.ChangeDetail) bool {
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "reviewed"
}
