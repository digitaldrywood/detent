package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Pull request actions (decisions section 18.6). Opening, updating and merging
// a pull request are executed by the existing merge queue on a runner with a
// checkout, never by the hub against GitHub directly. The hub's part is to
// decide whether the action may be asked for at all, to refuse a head that has
// moved, and to put one work item on the queue the merge lane already drains.
//
// The work item is a native issue in the project's merge lane, labelled
// detent:pull-request-action. There is no second queue: the issue is claimable
// through queue_entries exactly like any other, and the orchestrator's merge
// lane is the state it is created in.

// pullRequestActions are the verbs 18.6 defines. `open` is addressed by the
// issue because there is no number yet; the other two name the pull request.
var pullRequestActions = map[string]bool{"open": true, "update_branch": true, "merge": true}

// PullRequestActionRequest is the body of both action endpoints.
type pullRequestActionRequest struct {
	tracker.Mutation
	Action          string `json:"action"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

// pullRequestActionResponse is the 202 both endpoints answer with.
type pullRequestActionResponse struct {
	WorkItem tracker.NativeIssue `json:"work_item"`
	ActionID string              `json:"action_id"`
}

// pullRequestActionLabelKey marks the hub's own action-item creation. It is
// unexported and never derived from a request, so no API caller can reserve
// the label for itself.
type pullRequestActionLabelKey struct{}

func allowPullRequestActionLabel(ctx context.Context) context.Context {
	return context.WithValue(ctx, pullRequestActionLabelKey{}, true)
}

// pullRequestActionLabelAllowed reports whether this call may set the reserved
// action label.
func pullRequestActionLabelAllowed(ctx context.Context) bool {
	allowed, ok := ctx.Value(pullRequestActionLabelKey{}).(bool)
	return ok && allowed
}

// mergeLaneState reports the workflow state a pull request action is created
// in: the project's merge lane when it has one, so the existing merge queue
// picks the item up, and its first dispatchable state otherwise.
func mergeLaneState(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if !state.Terminal && state.Dispatchable && strings.EqualFold(state.Name, "Merging") {
			return state.Name, true
		}
	}
	return firstDispatchableState(project)
}

func pullRequestActionTitle(action string, number int) string {
	if number > 0 {
		return "Pull request " + action + " for #" + strconv.Itoa(number)
	}
	return "Pull request " + action
}

// pullRequestActionBody is the whole brief a claiming runner sees. It names
// what to do, on what, and what head the actor expected, and it states that
// the runner must refuse a head that moved since -- the hub checked the head
// at acceptance, and the runner checks it again at execution, because a queue
// is a delay.
func pullRequestActionBody(action, subject, expected string, number int) string {
	var body strings.Builder
	body.WriteString("Pull request action requested through the Detent pull request panel.\n\n")
	body.WriteString("Action: " + action + "\n")
	body.WriteString("Subject work item: " + subject + "\n")
	if number > 0 {
		body.WriteString("Pull request: #" + strconv.Itoa(number) + "\n")
	}
	body.WriteString("Expected head: " + expected + "\n\n")
	body.WriteString("Execute this action on a checkout of the subject work item's branch. ")
	body.WriteString("Verify the head still matches the expected head before acting and stop with head_moved if it does not. ")
	body.WriteString("Do not implement anything and do not open a second pull request.\n")
	return body.String()
}

// pullRequestActionRecord is the association between the queued issue and what
// it was asked to do.
type pullRequestActionRecord struct {
	ID              string
	WorkItemID      string
	SubjectID       string
	Action          string
	Number          int
	ExpectedHeadSHA string
	ActorID         string
	CreatedAt       time.Time
}

func createPullRequestAction(ctx context.Context, tx *sql.Tx, scope nativeScope, record pullRequestActionRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO pull_request_actions
(id, work_item_id, organization_id, project_id, subject_work_item_id, action, number, expected_head_sha, actor_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.WorkItemID, scope.organization, scope.project, record.SubjectID,
		record.Action, record.Number, record.ExpectedHeadSHA, record.ActorID, formatHubTime(record.CreatedAt))
	if err != nil {
		return fmt.Errorf("record pull request action: %w", err)
	}
	return nil
}

// currentPullRequestHead reports the head the hub believes the action's
// subject is at, and where it learned it. For an action on a numbered pull
// request that is the connector's projection; for `open` it is the change
// request's current version, because there is no pull request yet.
func (s *Service) currentPullRequestHead(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, number int, repositoryID int64) (string, error) {
	if number > 0 {
		if repositoryID == 0 {
			return "", nativeNotFound()
		}
		summary, found, err := readConnectorPullRequestByNumber(ctx, tx, repositoryID, number)
		if err != nil {
			return "", err
		}
		if !found {
			return "", nativeNotFound()
		}
		return summary.HeadSHA, nil
	}
	changes, err := changeRows[tracker.ChangeRequest](ctx, tx, `SELECT c.record_json FROM change_requests c
JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? ORDER BY c.rowid`, scope.organization, scope.project, item)
	if err != nil {
		return "", err
	}
	for _, change := range changes {
		version, err := readChangeVersion(ctx, tx, change.ID, change.CurrentVersion)
		if err != nil {
			continue
		}
		if version.HeadSHA != "" {
			return version.HeadSHA, nil
		}
	}
	return "", nil
}

// requirePullRequestMergePolicy enforces 18.6's "merge additionally follows
// the change-request merge policy": a merge may only be asked for when the
// change the pull request carries has reached the reviewed status its project
// policy defines.
func (s *Service) requirePullRequestMergePolicy(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, now time.Time) error {
	changes, err := changeRows[tracker.ChangeRequest](ctx, tx, `SELECT c.record_json FROM change_requests c
JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? ORDER BY c.rowid`, scope.organization, scope.project, item)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return &nativeError{Code: "merge_not_permitted", Message: "The work item has no change request to merge", status: http.StatusConflict}
	}
	for _, change := range changes {
		detail, err := readChangeDetail(ctx, tx, scope, item, change.ID, now)
		if err != nil {
			return err
		}
		if detail.Summary.Status == "reviewed" {
			return nil
		}
	}
	return &nativeError{
		Code: "merge_not_permitted", Message: "The change request has not met its review policy", status: http.StatusConflict,
	}
}

// Endpoints.

// openPullRequestAction accepts `open`, which has no number yet and is
// therefore addressed by the issue.
func (s *Service) openPullRequestAction(c echo.Context) error {
	return s.acceptPullRequestAction(c, 0)
}

// numberedPullRequestAction accepts `update_branch` and `merge` on an existing
// pull request.
func (s *Service) numberedPullRequestAction(c echo.Context) error {
	number, err := strconv.Atoi(c.Param("number"))
	if err != nil || number <= 0 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return s.acceptPullRequestAction(c, number)
}

func (s *Service) acceptPullRequestAction(c echo.Context, number int) error {
	var request pullRequestActionRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.Action = strings.TrimSpace(request.Action)
	if !pullRequestActions[request.Action] {
		return s.nativeAPIError(c, nativeInvalid("action must be open, update_branch or merge"))
	}
	if number == 0 && request.Action != "open" {
		return s.nativeAPIError(c, nativeInvalid("update_branch and merge address a pull request by number"))
	}
	if number > 0 && request.Action == "open" {
		return s.nativeAPIError(c, nativeInvalid("open is addressed by the work item, not by a number"))
	}
	request.ExpectedHeadSHA = strings.TrimSpace(request.ExpectedHeadSHA)
	if request.ExpectedHeadSHA == "" || len(request.ExpectedHeadSHA) > tracker.MaxDiffSHABytes {
		return s.nativeAPIError(c, nativeInvalid("expected_head_sha is required"))
	}
	item := c.Param("item")
	return s.nativeMutationStatus(c, http.StatusAccepted, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if _, _, err := readNativeIssue(ctx, tx, scope, item); err != nil {
			return nil, err
		}
		_, repositoryID, err := projectConnector(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		current, err := s.currentPullRequestHead(ctx, tx, scope, item, number, repositoryID)
		if err != nil {
			return nil, err
		}
		if current != "" && !strings.EqualFold(current, request.ExpectedHeadSHA) {
			return nil, pullRequestHeadMoved(request.ExpectedHeadSHA, current)
		}
		if request.Action == "merge" {
			if err := s.requirePullRequestMergePolicy(ctx, tx, scope, item, now); err != nil {
				return nil, err
			}
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, fmt.Errorf("read project for pull request action: %w", err)
		}
		state, found := mergeLaneState(project)
		if !found {
			return nil, nativeInvalid("The project has no dispatchable workflow state for a pull request action")
		}
		created, err := createNativeIssueTx(allowPullRequestActionLabel(ctx), tx, scope, tracker.CreateIssue{
			Title:  pullRequestActionTitle(request.Action, number),
			Body:   pullRequestActionBody(request.Action, item, request.ExpectedHeadSHA, number),
			State:  state,
			Labels: []string{pullRequestActionLabel},
		}, now)
		if err != nil {
			return nil, err
		}
		issue, ok := created.(tracker.NativeIssue)
		if !ok {
			return nil, fmt.Errorf("unexpected pull request action issue result %T", created)
		}
		record := pullRequestActionRecord{
			ID: newNativeID("pra"), WorkItemID: string(issue.WorkItemID), SubjectID: item,
			Action: request.Action, Number: number, ExpectedHeadSHA: request.ExpectedHeadSHA,
			ActorID: scope.credential.ID, CreatedAt: now,
		}
		if err := createPullRequestAction(ctx, tx, scope, record); err != nil {
			return nil, err
		}
		return pullRequestActionResponse{WorkItem: issue, ActionID: record.ID}, nil
	})
}
