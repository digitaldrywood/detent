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

const duplicateCandidateLimit = 100

type possibleDuplicate struct {
	revision   tracker.Revision
	terminal   bool
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

type nativeDuplicateSearch struct {
	latestNumber int
	matches      []possibleDuplicate
	skipped      bool
}

func prepareNativeDuplicates(ctx context.Context, query nativeQueryer, scope nativeScope, request tracker.CreateIssue, now time.Time, machine bool) (*nativeDuplicateSearch, error) {
	var search nativeDuplicateSearch
	if !machine {
		var enabled bool
		if err := query.QueryRowContext(ctx, "SELECT duplicate_challenge FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&enabled); err != nil {
			return nil, err
		}
		if !enabled || scope.credential.Scope == apiScopeWorker {
			search.skipped = true
			return &search, nil
		}
	}
	if err := query.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0) FROM issues WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&search.latestNumber); err != nil {
		return nil, err
	}
	matches, err := resolveNativeDuplicates(ctx, query, scope, request, now, machine)
	search.matches = matches
	return &search, err
}

func duplicateWindow(scope nativeScope, now time.Time, machine bool) (string, []any) {
	window := " AND i.archived = 0"
	args := []any{scope.organization, scope.project}
	if !machine {
		window = " AND i.archived = 0 AND (ws.terminal = 0 OR (ws.terminal = 1 AND julianday(COALESCE(" + nativeTerminalEnteredAt + ", i.native_created_at)) >= julianday(?)))"
		args = append(args, formatHubTime(now.AddDate(0, 0, -30)))
	}
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	return window + " AND (" + visibility + ")", append(args, visibilityArgs...)
}

func resolveNativeDuplicates(ctx context.Context, query nativeQueryer, scope nativeScope, request tracker.CreateIssue, now time.Time, machine bool) ([]possibleDuplicate, error) {
	if err := requireNativeSearchIndex(ctx, query, scope); err != nil {
		return nil, err
	}
	pageQuery, pageErr := prepareNativePageQuery(ctx, query, scope, "", "native_issue_actors", "native_transition_facts")
	if pageErr != nil {
		return nil, pageErr
	}
	query = pageQuery
	window, args := duplicateWindow(scope, now, machine)
	var matches []possibleDuplicate
	origin, stamped := issueorigin.Parse(request.Body)
	if stamped {
		fingerprints := []string{origin.Fingerprint}
		if canonical := issueorigin.DefectFingerprint(request.Body); canonical != "" && canonical != origin.Fingerprint {
			fingerprints = append(fingerprints, canonical)
		}
		var searches []string
		for _, fingerprint := range fingerprints {
			searches = append(searches, nativeScopedExpression(nativeSearchScopes(scope, ""), nativeSearchExpressions(fingerprint), " AND "))
		}
		if expressions := duplicateSearchExpressions(request.Body); len(expressions) > 0 {
			searches = append(searches, nativeScopedExpression(nativeSearchScopes(scope, ""), expressions, " OR "))
		}
		for _, expression := range searches {
			searchArgs := []any{expression}
			searchArgs = append(searchArgs, args...)
			searchArgs = append(searchArgs, args...)
			rows, err := query.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
SELECT d.issue_id,d.comment_id FROM native_work_scoped_fts JOIN native_work_documents d ON d.id=native_work_scoped_fts.rowid/2
JOIN issues i ON i.id=d.issue_id JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE native_work_scoped_fts MATCH ? AND i.organization_id=? AND i.project_id=?`+window+`
 )
SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision,ws.terminal,i.body || char(10) || COALESCE(c.body,'')
FROM candidates d JOIN issues i ON i.id=d.issue_id
JOIN workflow_states ws ON ws.id=i.workflow_state_id LEFT JOIN native_comments c ON c.id=d.comment_id
WHERE i.organization_id=? AND i.project_id=?`+window+` ORDER BY ws.terminal,i.number DESC`, searchArgs...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			for rows.Next() {
				var match possibleDuplicate
				var body string
				if err := rows.Scan(&match.WorkItemID, &match.Number, &match.Title, &match.State, &match.revision, &match.terminal, &body); err != nil {
					return nil, errors.Join(err, rows.Close())
				}
				same := issueorigin.SameDefect(body, request.Body)
				if string(scope.project) == issueorigin.DetentCloudProjectID {
					previous, current := issueorigin.ReleaseStabilizationSeries(body), issueorigin.ReleaseStabilizationSeries(request.Body)
					if previous != "" || current != "" {
						same = previous != "" && previous == current
					}
				}
				if same && !slices.ContainsFunc(matches, func(m possibleDuplicate) bool { return m.WorkItemID == match.WorkItemID }) {
					match.Identifier = string(scope.project) + "#" + strconv.Itoa(match.Number)
					match.Why = "Matching machine fingerprint"
					matches = append(matches, match)
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
		}
		slices.SortFunc(matches, func(a, b possibleDuplicate) int {
			if a.terminal != b.terminal {
				if a.terminal {
					return 1
				}
				return -1
			}
			return b.Number - a.Number
		})
		limit := 5
		if machine {
			limit = 1
		}
		matches = matches[:min(limit, len(matches))]
	}
	if machine || len(matches) == 5 {
		return matches, nil
	}
	for _, content := range []string{request.Title, request.Body} {
		expressions := duplicateSearchExpressions(content)
		if len(expressions) == 0 {
			continue
		}
		minimum := max(2, (len(expressions)*3+4)/5)
		if len(expressions) == 1 && content == request.Title {
			minimum = 1
		}
		ctes, searchArgs := nativeSearchHits(scope, "", expressions)
		searchArgs = append(searchArgs, args...)
		searchArgs = append(searchArgs, minimum)
		rows, err := query.QueryContext(ctx, ctes+`
SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision,search.matched
FROM ranked search JOIN issues i ON i.id=search.issue_id JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE search.hit=1 AND i.organization_id=? AND i.project_id=?`+window+`
AND search.terms>=? ORDER BY search.search_score,ws.terminal,i.number DESC LIMIT 5`, searchArgs...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var hit possibleDuplicate
			var terms string
			if err := rows.Scan(&hit.WorkItemID, &hit.Number, &hit.Title, &hit.State, &hit.revision, &terms); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			if slices.ContainsFunc(matches, func(m possibleDuplicate) bool { return m.WorkItemID == hit.WorkItemID }) {
				continue
			}
			hit.Identifier = string(scope.project) + "#" + strconv.Itoa(hit.Number)
			matched := strings.Split(terms, ", ")
			hit.Why = "Matching terms: " + strings.NewReplacer(`"`, "", "*", "").Replace(strings.Join(matched[:min(6, len(matched))], ", "))
			matches = append(matches, hit)
			if len(matches) == 5 {
				break
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		if len(matches) == 5 {
			break
		}
	}
	return matches, nil
}

func resolveNativeDuplicatesTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, now time.Time, machine bool) ([]possibleDuplicate, error) {
	if scope.duplicateSearch == nil {
		return resolveNativeDuplicates(ctx, tx, scope, request, now, machine)
	}
	if scope.duplicateSearch.skipped {
		return nil, nativeConflict(0)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0) FROM issues WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&latest); err != nil {
		return nil, err
	}
	if latest != scope.duplicateSearch.latestNumber {
		return nil, nativeConflict(0)
	}
	window, args := duplicateWindow(scope, now, machine)
	var matches []possibleDuplicate
	for _, candidate := range scope.duplicateSearch.matches {
		match := candidate
		err := tx.QueryRowContext(ctx, `SELECT i.number,i.title,ws.detent_state,i.revision FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=?`+window+` AND i.native_id=?`, append(slices.Clone(args), candidate.WorkItemID)...).Scan(&match.Number, &match.Title, &match.State, &match.revision)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if match.revision != candidate.revision {
			return nil, nativeConflict(match.revision)
		}
		matches = append(matches, match)
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
		return nil, nativePublicInvalid("Duplicate challenge is invalid, expired or already used")
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
			return nil, nativePublicInvalid("Not duplicate of must contain unique issue numbers")
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
			return nil, nativePublicInvalid("Not duplicate of must name a challenged issue number")
		}
	}
	return related, nil
}
