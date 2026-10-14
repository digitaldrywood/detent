package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	testPlanLabel     = "test-plan"
	testPlanItemLimit = 1000
	testPlanNoteLimit = 2000
	testItemField     = "test_item"
)

var testPlanChecklistItem = regexp.MustCompile(`^ {0,3}(?:[-*+]|\d{1,9}[.)])\s+\[[ xX]\]\s+(\S.*?)\s*$`)

func isTestPlan(labels []string) bool {
	return slices.ContainsFunc(labels, func(label string) bool { return strings.EqualFold(strings.TrimSpace(label), testPlanLabel) })
}

func markdownFenceRun(line string) (byte, int) {
	if len(line) == 0 || line[0] != '`' && line[0] != '~' {
		return 0, 0
	}
	marker := line[0]
	length := 0
	for length < len(line) && line[length] == marker {
		length++
	}
	if length < 3 {
		return 0, 0
	}
	return marker, length
}

func parseTestPlanChecklist(body string) []string {
	items := []string{}
	var fence byte
	fenceLength := 0
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimRight(strings.ReplaceAll(raw, "\t", "    "), "\r")
		indented := strings.TrimLeft(line, " ")
		indent := len(line) - len(indented)
		marker, length := markdownFenceRun(indented)
		if fence != 0 {
			if indent < 4 && marker == fence && length >= fenceLength && strings.TrimSpace(indented[length:]) == "" {
				fence, fenceLength = 0, 0
			}
			continue
		}
		if indent < 4 && marker != 0 && (marker == '~' || !strings.Contains(indented[length:], "`")) {
			fence, fenceLength = marker, length
			continue
		}
		if match := testPlanChecklistItem.FindStringSubmatch(line); match != nil {
			items = append(items, match[1])
			if len(items) == testPlanItemLimit {
				break
			}
		}
	}
	return items
}

func testPlanLanes(states []tracker.NativeState) []string {
	lanes := []string{}
	for _, state := range states {
		if !state.Dispatchable && !state.Terminal {
			lanes = append(lanes, state.Name)
		}
	}
	return lanes
}

func defaultTestPlanLane(states []tracker.NativeState) string {
	lanes := testPlanLanes(states)
	for _, lane := range lanes {
		if strings.EqualFold(lane, "Backlog") {
			return lane
		}
	}
	for _, lane := range lanes {
		if lane != "Triage" {
			return lane
		}
	}
	return ""
}

func defaultNativeIssueStateFor(states []tracker.NativeState, labels []string) string {
	if isTestPlan(labels) {
		if lane := defaultTestPlanLane(states); lane != "" {
			return lane
		}
	}
	return defaultNativeIssueState(states)
}

func testPlanLaneRefusal(lanes []string, message string) error {
	if len(lanes) == 0 {
		message = "This project has no non-dispatchable, nonterminal lane for test plans"
	}
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func requireTestPlanFilingLane(states []tracker.NativeState, labels []string, state string) error {
	lanes := testPlanLanes(states)
	if !isTestPlan(labels) || slices.Contains(lanes, state) {
		return nil
	}
	return testPlanLaneRefusal(lanes, "File a test plan in a non-dispatchable, nonterminal lane: "+strings.Join(lanes, ", "))
}

func requireTestPlanLabelLane(states []tracker.NativeState, before, after []string, state string) error {
	lanes := testPlanLanes(states)
	if isTestPlan(before) || !isTestPlan(after) || slices.Contains(lanes, state) {
		return nil
	}
	return testPlanLaneRefusal(lanes, "Move the issue to a non-dispatchable, nonterminal lane ("+strings.Join(lanes, ", ")+") before labeling it test-plan")
}

func requireTestPlanTransition(states []tracker.NativeState, issue tracker.NativeIssue, target string) error {
	lanes := testPlanLanes(states)
	if !isTestPlan(issue.Labels) || !slices.Contains(lanes, issue.State) || slices.Contains(lanes, target) {
		return nil
	}
	return testPlanLaneRefusal(lanes, "Test plans stay in a non-dispatchable, nonterminal lane ("+strings.Join(lanes, ", ")+"); remove the test-plan label before moving it to "+target)
}

func (s *Service) recordNativeTestItem(c echo.Context) error {
	var request tracker.RecordTestItem
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	result, err := s.recordNativeTestItemCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) recordNativeTestItemCommand(ctx context.Context, scope nativeScope, item string, request tracker.RecordTestItem) (json.RawMessage, error) {
	scope.action = operatortool.UpdateTestItem
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+item+"/test-items"), Item: item, RequireLease: true, Feature: "collaboration"}
	return s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return recordNativeTestItemTx(ctx, tx, scope, item, request, now)
	})
}

func recordNativeTestItemTx(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, request tracker.RecordTestItem, now time.Time) (tracker.NativeIssue, error) {
	if !slices.Contains([]string{"pass", "fail", "blocked"}, request.Result) {
		return tracker.NativeIssue{}, publicInvalid("result must be pass, fail or blocked")
	}
	if !utf8.ValidString(request.Note) || utf8.RuneCountInString(request.Note) > testPlanNoteLimit {
		return tracker.NativeIssue{}, publicInvalid(fmt.Sprintf("note must be UTF-8 text of at most %d characters", testPlanNoteLimit))
	}
	if strings.TrimSpace(request.Text) == "" {
		return tracker.NativeIssue{}, publicInvalid("text must be the checklist item's text as read")
	}
	if request.RelatedIssueNumber < 0 {
		return tracker.NativeIssue{}, publicInvalid("The related issue number must be positive")
	}
	if request.RelatedIssueNumber > 0 && request.Result != "fail" {
		return tracker.NativeIssue{}, publicInvalid("A related issue number is accepted only with a fail result")
	}
	issue, planID, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if issue.Profile != "native" || !isTestPlan(issue.Labels) {
		return tracker.NativeIssue{}, publicInvalid("Item results are recorded only on native issues labeled test-plan")
	}
	items := parseTestPlanChecklist(issue.Body)
	if request.Item < 1 || request.Item > len(items) {
		return tracker.NativeIssue{}, publicInvalid(fmt.Sprintf("item must be between 1 and %d, the number of checklist items in the plan", len(items)))
	}
	if items[request.Item-1] != request.Text {
		return tracker.NativeIssue{}, &nativeError{Code: "test_item_changed", Message: "The checklist item changed since it was read; reload the plan and record the result again", status: http.StatusConflict, publicMessage: true}
	}
	data := tracker.CollaborationData{Fields: []string{testItemField}, TestItem: &tracker.TestItemResult{Index: request.Item, Text: items[request.Item-1], Result: request.Result, Note: strings.TrimSpace(request.Note), RelatedNumber: request.RelatedIssueNumber}}
	if request.RelatedIssueNumber > 0 {
		var relatedID tracker.WorkItemID
		err := tx.QueryRowContext(ctx, "SELECT id, native_id FROM issues WHERE organization_id = ? AND project_id = ? AND number = ?", scope.organization, scope.project, request.RelatedIssueNumber).Scan(&relatedID, &data.RelatedWorkItemID)
		if errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, publicInvalid("The related issue number does not name an issue in this project")
		}
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if relatedID == planID {
			return tracker.NativeIssue{}, publicInvalid("A test plan cannot be related to itself")
		}
		if err := changeRelatedIssuesTx(ctx, tx, planID, relatedID, "add", now); err != nil {
			return tracker.NativeIssue{}, err
		}
		issue, err = persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{RelatedWorkItemID: data.RelatedWorkItemID, Operation: "add", Fields: []string{"related"}}, now)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
	}
	if err := appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), "issue.edited", data, now); err != nil {
		return tracker.NativeIssue{}, err
	}
	err = attachNativeTestPlan(ctx, tx, scope, &issue)
	return issue, err
}

func publicInvalid(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func attachNativeTestPlan(ctx context.Context, query nativeQueryer, scope nativeScope, issue *tracker.NativeIssue) error {
	if !isTestPlan(issue.Labels) {
		return nil
	}
	plan, err := readNativeTestPlan(ctx, query, scope, *issue)
	issue.TestPlan = plan
	return err
}

func readNativeTestPlan(ctx context.Context, query nativeQueryer, scope nativeScope, issue tracker.NativeIssue) (*tracker.NativeTestPlan, error) {
	plan := &tracker.NativeTestPlan{Items: []tracker.NativeTestItem{}}
	for index, text := range parseTestPlanChecklist(issue.Body) {
		plan.Items = append(plan.Items, tracker.NativeTestItem{Index: index + 1, Text: text, LinkedIssues: []tracker.NativeTestItemLink{}})
	}
	rows, err := query.QueryContext(ctx, `SELECT e.actor_json, `+nativeActorDisplayName("json_extract(e.actor_json, '$.principal_id')", "e.organization_id")+`, e.data_json, e.recorded_at
FROM collaboration_events e WHERE e.organization_id = ? AND e.project_id = ? AND e.work_item_id = ? AND e.type = 'issue.edited' AND json_extract(e.data_json, '$.test_item') IS NOT NULL ORDER BY e.sequence`, scope.organization, scope.project, issue.WorkItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type recordedResult struct {
		data        tracker.CollaborationData
		actorJSON   string
		displayName string
		recorded    string
	}
	var results []recordedResult
	for rows.Next() {
		var entry recordedResult
		var dataJSON string
		if err := rows.Scan(&entry.actorJSON, &entry.displayName, &dataJSON, &entry.recorded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(dataJSON), &entry.data); err != nil {
			return nil, err
		}
		results = append(results, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	related, err := readNativeRelated(ctx, query, scope, issue.WorkItemID)
	if err != nil {
		return nil, err
	}
	for _, recorded := range results {
		data, actorJSON, displayName := recorded.data, recorded.actorJSON, recorded.displayName
		result := data.TestItem
		if result == nil {
			continue
		}
		position := testPlanItemPosition(plan.Items, result.Index, result.Text)
		if position < 0 {
			continue
		}
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil {
			return nil, err
		}
		actor.DisplayName = displayName
		at, err := parseTimeValue(recorded.recorded)
		if err != nil {
			return nil, err
		}
		entry := &plan.Items[position]
		entry.Result, entry.Note, entry.Tester, entry.RecordedAt = result.Result, result.Note, &actor, &at
		linked := slices.ContainsFunc(related, func(link tracker.NativeDependency) bool { return link.ID == data.RelatedWorkItemID })
		if data.RelatedWorkItemID != "" && linked && !slices.ContainsFunc(entry.LinkedIssues, func(link tracker.NativeTestItemLink) bool { return link.WorkItemID == data.RelatedWorkItemID }) {
			entry.LinkedIssues = append(entry.LinkedIssues, tracker.NativeTestItemLink{WorkItemID: data.RelatedWorkItemID, Number: result.RelatedNumber})
		}
	}
	for _, entry := range plan.Items {
		switch entry.Result {
		case "pass":
			plan.Passed++
		case "fail":
			plan.Failed++
		case "blocked":
			plan.Blocked++
		default:
			plan.Untested++
		}
	}
	return plan, nil
}

func testPlanItemPosition(items []tracker.NativeTestItem, index int, text string) int {
	if index >= 1 && index <= len(items) && items[index-1].Text == text {
		return index - 1
	}
	position := -1
	for i, item := range items {
		if item.Text != text {
			continue
		}
		if position >= 0 {
			return -1
		}
		position = i
	}
	return position
}
