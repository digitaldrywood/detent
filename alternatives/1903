package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const duplicateChallengeLifetime = time.Hour

type possibleDuplicate struct {
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Identifier string                   `json:"identifier"`
	Number     int                      `json:"number"`
	Title      string                   `json:"title"`
	State      string                   `json:"state"`
	WebURL     string                   `json:"web_url"`
	Why        string                   `json:"why"`
}

type duplicateChallenge struct {
	PossibleDuplicates []possibleDuplicate `json:"possible_duplicates"`
	ChallengeToken     string              `json:"challenge_token"`
}

func resolveNativeDuplicatesTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, machine bool) ([]possibleDuplicate, error) {
	window := " AND i.archived = 0"
	args := []any{scope.organization, scope.project}
	if !machine {
		window = " AND ((ws.terminal = 0 AND i.archived = 0) OR (ws.terminal = 1 AND julianday(COALESCE(" + nativeTerminalEnteredAt + ", i.native_created_at)) >= julianday(?)))"
		args = append(args, formatHubTime(now.AddDate(0, 0, -30)))
	}
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	window += " AND (" + visibility + ")"
	args = append(args, visibilityArgs...)
	var matches []possibleDuplicate
	if _, stamped := issueorigin.Parse(request.Body); stamped {
		rows, err := tx.QueryContext(ctx, `SELECT i.native_id, i.number, i.title, ws.detent_state,
i.body || char(10) || COALESCE((SELECT group_concat(c.body, char(10)) FROM native_comments c WHERE c.work_item_id = i.native_id), '')
FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ?`+window+` ORDER BY ws.terminal, i.number DESC`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var match possibleDuplicate
			var body string
			if err := rows.Scan(&match.WorkItemID, &match.Number, &match.Title, &match.State, &body); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			same := issueorigin.SameDefect(body, request.Body)
			if string(scope.project) == issueorigin.DetentCloudProjectID {
				previous, current := issueorigin.ReleaseStabilizationSeries(body), issueorigin.ReleaseStabilizationSeries(request.Body)
				if previous != "" || current != "" {
					same = previous != "" && previous == current
				}
			}
			if same {
				match.Identifier = string(scope.project) + "#" + strconv.Itoa(match.Number)
				match.Why = "Matching machine fingerprint"
				matches = append(matches, match)
				if machine || len(matches) == 5 {
					break
				}
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	if machine || len(matches) == 5 {
		return matches, nil
	}
	for _, content := range []string{request.Title, request.Body} {
		expressions := duplicateSearchExpressions(content)
		if len(expressions) == 0 {
			continue
		}
		encoded, err := json.Marshal(expressions)
		if err != nil {
			return nil, err
		}
		minimum := max(2, (len(expressions)*3+4)/5)
		if len(expressions) == 1 && content == request.Title {
			minimum = 1
		}
		searchArgs := []any{strings.Join(expressions, " OR "), scope.organization, scope.project, string(encoded)}
		searchArgs = append(searchArgs, args...)
		searchArgs = append(searchArgs, -minimum)
		rows, err := tx.QueryContext(ctx, `SELECT i.native_id, i.number, i.title, ws.detent_state FROM issues i
JOIN workflow_states ws ON ws.id = i.workflow_state_id`+nativeSearchJoin+`
WHERE i.organization_id = ? AND i.project_id = ?`+window+`
AND search.score < ? ORDER BY search.score, ws.terminal, i.number DESC LIMIT 5`, searchArgs...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var hits []possibleDuplicate
		for rows.Next() {
			var hit possibleDuplicate
			if err := rows.Scan(&hit.WorkItemID, &hit.Number, &hit.Title, &hit.State); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			hits = append(hits, hit)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		for _, hit := range hits {
			if slices.ContainsFunc(matches, func(match possibleDuplicate) bool { return match.WorkItemID == hit.WorkItemID }) {
				continue
			}
			var terms string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(group_concat(term.value, ', '), '') FROM json_each(?) term
WHERE EXISTS (SELECT 1 FROM native_work_fts JOIN native_work_documents d ON d.id = native_work_fts.rowid
JOIN issues i ON i.id = d.issue_id WHERE native_work_fts MATCH term.value AND i.native_id = ?)`, string(encoded), hit.WorkItemID).Scan(&terms); err != nil {
				return nil, err
			}
			hit.Identifier = string(scope.project) + "#" + strconv.Itoa(hit.Number)
			matched := strings.Split(terms, ", ")
			hit.Why = "Matching terms: " + strings.NewReplacer(`"`, "", "*", "").Replace(strings.Join(matched[:min(6, len(matched))], ", "))
			matches = append(matches, hit)
			if len(matches) == 5 {
				return matches, nil
			}
		}
	}
	return matches, nil
}

func duplicateSearchExpressions(content string) []string {
	var expressions []string
	for _, expression := range nativeSearchExpressions(content) {
		term := strings.Trim(expression, `"*`)
		if len(term) < 3 || len(term) > 64 || strings.Contains(" a an and are as at be been but by for from has have how if in into is it its not of on or our that the their then these this to was we were what when where which who will with would you your ", " "+term+" ") {
			continue
		}
		expressions = append(expressions, expression)
		if len(expressions) == 32 {
			break
		}
	}
	return expressions
}

func duplicateContentHash(request tracker.CreateIssue) (string, error) {
	request.ChallengeToken = ""
	request.NotDuplicateOf = nil
	return nativeCommandHash(request)
}

func challengeNativeIssueTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) (duplicateChallenge, error) {
	var enabled bool
	if err := tx.QueryRowContext(ctx, "SELECT duplicate_challenge FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&enabled); err != nil {
		return duplicateChallenge{}, err
	}
	if !enabled || scope.credential.Scope == apiScopeWorker {
		return duplicateChallenge{}, nil
	}
	matches, err := resolveNativeDuplicatesTx(ctx, tx, scope, request, now, false)
	if err != nil || len(matches) == 0 {
		return duplicateChallenge{}, err
	}
	hash, err := duplicateContentHash(request)
	if err != nil {
		return duplicateChallenge{}, err
	}
	encoded, err := json.Marshal(matches)
	if err != nil {
		return duplicateChallenge{}, err
	}
	token := newNativeID("dup")
	_, err = tx.ExecContext(ctx, `INSERT INTO native_duplicate_challenges
(token, organization_id, project_id, principal_id, request_id, content_hash, matches_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, token, scope.organization, scope.project, scope.actor().PrincipalID, request.IdempotencyKey, hash, string(encoded), formatHubTime(now))
	if err != nil {
		return duplicateChallenge{}, err
	}
	return duplicateChallenge{PossibleDuplicates: matches, ChallengeToken: token}, nil
}

func confirmNativeDuplicateTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time) ([]possibleDuplicate, error) {
	hash, err := duplicateContentHash(request)
	if err != nil {
		return nil, err
	}
	var encoded string
	err = tx.QueryRowContext(ctx, `DELETE FROM native_duplicate_challenges
WHERE token = ? AND organization_id = ? AND project_id = ? AND principal_id = ? AND request_id = ? AND content_hash = ? AND consumed_at IS NULL
AND julianday(created_at) > julianday(?)
RETURNING matches_json`, request.ChallengeToken, scope.organization, scope.project, scope.actor().PrincipalID, request.IdempotencyKey, hash, formatHubTime(now.Add(-duplicateChallengeLifetime))).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nativeInvalid("Duplicate challenge is invalid, expired or already used")
	}
	if err != nil {
		return nil, err
	}
	var matches []possibleDuplicate
	if err := json.Unmarshal([]byte(encoded), &matches); err != nil {
		return nil, err
	}
	var related []possibleDuplicate
	for _, number := range request.NotDuplicateOf {
		if slices.ContainsFunc(related, func(match possibleDuplicate) bool { return match.Number == number }) {
			return nil, nativeInvalid("Not duplicate of must contain unique issue numbers")
		}
		found := false
		for _, match := range matches {
			if match.Number == number {
				related = append(related, match)
				found = true
				break
			}
		}
		if !found {
			return nil, nativeInvalid("Not duplicate of must name a challenged issue number")
		}
	}
	return related, nil
}
