package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type completionDecisionData struct {
	Request     workpad.DecisionRequest
	HumanAction string
}

func completionDecision(data tracker.NativeRunData) (completionDecisionData, bool, error) {
	if data.Disposition != nil && data.Disposition.Status == workpad.StatusInProgress && data.Disposition.ReasonCode == "invalid_workpad_signal" {
		return completionDecisionData{}, false, nil
	}
	signal, reported := workpad.SignalFromComment(data.CompletionBody, "", "")
	humanDisposition := data.Disposition != nil && data.Disposition.Status == workpad.StatusBlocked && data.Disposition.HumanAction
	humanReport := reported && signal != nil && signal.Status == workpad.StatusBlocked && signal.HumanAction != ""
	if !humanDisposition && !humanReport {
		return completionDecisionData{}, false, nil
	}
	if !reported || signal == nil || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || signal.HumanAction == "" {
		return completionDecisionData{}, false, errors.New("blocked completion with human_action requires a valid decision_request in detent-status")
	}
	if err := signal.DecisionRequest.Validate(); err != nil {
		return completionDecisionData{}, false, err
	}
	return completionDecisionData{Request: *signal.DecisionRequest, HumanAction: signal.HumanAction}, true, nil
}

func persistCompletionDecision(ctx context.Context, tx *sql.Tx, data tracker.NativeRunData) error {
	decision, found, err := completionDecision(data)
	if err != nil || !found {
		return err
	}
	d := decision.Request
	if _, err := tx.ExecContext(ctx, `INSERT INTO decision_requests (attempt_id, summary, question, human_action, custom_allowed, custom_return_state)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (attempt_id) DO NOTHING`, data.AttemptID, d.Summary, d.Question, decision.HumanAction, d.CustomAllowed, d.CustomReturnState); err != nil {
		return err
	}
	for i, option := range d.Options {
		if _, err := tx.ExecContext(ctx, `INSERT INTO decision_options (attempt_id, position, label, consequence, recommended, return_state)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (attempt_id, position) DO NOTHING`, data.AttemptID, i, option.Label, option.Consequence, option.Recommended, option.ReturnState); err != nil {
			return err
		}
	}
	return nil
}

func readIssueDecision(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (*workpad.DecisionRequest, bool, error) {
	var d workpad.DecisionRequest
	var attempt string
	err := q.QueryRowContext(ctx, `SELECT d.attempt_id, d.summary, d.question, d.custom_allowed, d.custom_return_state
FROM decision_requests d JOIN native_attempts a ON a.id = d.attempt_id
WHERE d.answered_comment_id IS NULL AND a.id = (SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY fencing_token DESC LIMIT 1)`, scope.organization, scope.project, item).Scan(&attempt, &d.Summary, &d.Question, &d.CustomAllowed, &d.CustomReturnState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d.AttemptID = attempt
	rows, err := q.QueryContext(ctx, `SELECT label, consequence, recommended, return_state FROM decision_options WHERE attempt_id = ? ORDER BY position`, attempt)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var option workpad.DecisionOption
		if err := rows.Scan(&option.Label, &option.Consequence, &option.Recommended, &option.ReturnState); err != nil {
			return nil, false, err
		}
		d.Options = append(d.Options, option)
	}
	return &d, true, rows.Err()
}

func readIssueHumanAction(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (string, error) {
	var action, body string
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN d.answered_comment_id IS NULL THEN COALESCE(d.human_action, '') ELSE '' END,
CASE WHEN d.answered_comment_id IS NULL THEN a.data_json ELSE '' END
FROM native_attempts a LEFT JOIN decision_requests d ON d.attempt_id = a.id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? ORDER BY a.fencing_token DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&action, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if action != "" {
		return action, nil
	}
	var data tracker.NativeRunData
	if body != "" {
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return "", err
		}
	}
	if signal, reported := workpad.SignalFromComment(data.CompletionBody, "", ""); reported {
		if signal != nil && signal.Invalid == nil {
			return signal.HumanAction, nil
		}
		return "", nil
	}
	return "", nil
}

func recordDecisionAnswer(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, request tracker.Transition, now time.Time) error {
	a := request.DecisionAnswer
	d := issue.DecisionRequest
	if scope.actor().Kind != "human" || issue.Profile != "native" || !strings.EqualFold(issue.State, "Blocked") || d == nil || a.AttemptID != d.AttemptID || request.Reason != "user_requested" {
		return nativeInvalid("Decision answer requires the current blocked request and a human actor")
	}
	var answer, state string
	if a.Option != nil {
		if a.Custom != "" || *a.Option < 0 || *a.Option >= len(d.Options) {
			return nativeInvalid("Select one decision option or supply a custom answer")
		}
		option := d.Options[*a.Option]
		answer, state = option.Label+"\n\n"+option.Consequence, option.ReturnState
	} else {
		answer, state = strings.TrimSpace(a.Custom), d.CustomReturnState
		if !d.CustomAllowed || answer == "" || len(answer) > 8192 || !utf8.ValidString(answer) {
			return nativeInvalid("Custom decision answer requires bounded UTF-8 text")
		}
	}
	if state != request.State {
		return nativeInvalid("Decision answer destination differs from the selected option")
	}
	body := fmt.Sprintf("## Decision answer\n\n%s\n\n%s\n\nReturn to %s.", d.Question, answer, state)
	comment, err := insertNativeComment(ctx, tx, scope, issue, body, nil, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE decision_requests SET answered_comment_id = ? WHERE attempt_id = ?", comment.ID, a.AttemptID)
	return err
}
