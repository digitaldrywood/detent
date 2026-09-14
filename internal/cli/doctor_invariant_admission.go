package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
)

// This is diagnostic scope evidence, not a natural-language admission policy.
var doctorFeatureTitle = regexp.MustCompile(`(?i)^\s*(feat|perf|refactor)(\([^\r\n)]*\))?!?:`)
var doctorMechanismDeclaration = regexp.MustCompile(`(?i)\b(new|add|adds|adding|introduce|introduces|introducing|expand|expands|expanding)\b[^.!?;\n]*\b(config(uration)?\s+keys?|reason\s+codes?|brakes?|breakers?|leases?|parks?|recovery\s+paths?|reservations?)\b`)
var doctorNegatedDeclaration = regexp.MustCompile(`(?i)\b(no|not|never|without|remove|removes|removing|delete|deletes|deleting|consolidate|consolidates|consolidating)\b`)
var doctorDeclarationBoundary = regexp.MustCompile(`(?i)[.!?;,\n]|\b(and|but|then)\b`)

type doctorInvariantIssueReader interface {
	FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error)
}

func doctorInvariantAdmissionCheck(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps, db doctorTelemetryStore, since, until string) (check doctorCheck) {
	check = doctorInvariantCheck(id, "INV-11", "human scope approval", doctorOK, "no applied Todo entries in 7d")
	warn := func(err error) doctorCheck {
		check.Status, check.Detail = doctorWarn, "scope evidence unavailable: "+err.Error()
		return check
	}
	if db == nil {
		return warn(errors.New("runtime store unavailable"))
	}
	// Correlate actor evidence to this write, never to a later human move of
	// the same issue. Tracker mutation timestamps can differ from written_at.
	rows, err := db.QueryContext(ctx, `SELECT l.issue_id, l.origin, l.reason, l.written_at,
 COALESCE((SELECT e.metadata_json FROM workflow_phase_events e
 WHERE e.project_id = l.project_id AND e.issue_id = l.issue_id
 AND e.phase_type = 'lane' AND e.phase_name = l.to_state
 AND e.previous_phase_name = l.from_state AND e.reason = l.reason
 AND julianday(e.started_at) BETWEEN julianday(l.written_at) AND julianday(COALESCE(l.resolved_at, l.written_at))
 ORDER BY e.id DESC LIMIT 1), '{}')
 FROM lane_ledger l WHERE l.project_id = ? AND lower(trim(l.to_state)) = 'todo'
 AND l.result = 'applied' AND julianday(l.written_at) >= julianday(?) AND julianday(l.written_at) <= julianday(?)
 ORDER BY l.written_at, l.id`, id, since, until)
	if err != nil {
		return warn(err)
	}
	defer rows.Close()
	type entry struct{ issueID, origin, reason, at, metadata string }
	var entries []entry
	var ids []string
	seen := make(map[string]bool)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.issueID, &e.origin, &e.reason, &e.at, &e.metadata); err != nil {
			return warn(err)
		}
		entries = append(entries, e)
		if !seen[e.issueID] {
			seen[e.issueID] = true
			ids = append(ids, e.issueID)
		}
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return warn(readErr)
	}
	if closeErr != nil {
		return warn(closeErr)
	}
	if len(entries) == 0 {
		return check
	}
	conn, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return warn(err)
	}
	defer func() {
		if err := closeDoctorAutoPromoteConnector(conn); err != nil {
			if check.Status == doctorOK {
				check.Status = doctorWarn
			}
			check.Detail += "; tracker close failed: " + err.Error()
		}
	}()
	reader, ok := conn.(doctorInvariantIssueReader)
	if !ok {
		return warn(errors.New("tracker cannot read issues by ID"))
	}
	byID := make(map[string]connector.Issue, len(ids))
	// GitHub's issue identity lookup uses nodes(ids:), limited to 100 IDs.
	for batch := range slices.Chunk(ids, 100) {
		issues, err := reader.FetchIssueStatesByIDs(ctx, batch)
		if err != nil {
			return warn(err)
		}
		for _, issue := range issues {
			byID[issue.ID] = issue
		}
	}
	var violations, missing []string
	for _, e := range entries {
		issue, ok := byID[e.issueID]
		if !ok || strings.TrimSpace(issue.Title) == "" {
			missing = append(missing, e.issueID+" at "+e.at)
			continue
		}
		if !doctorIssueRequiresScopeApproval(issue.Title, issue.Description) {
			continue
		}
		if doctorTodoHumanMove(e.origin, e.reason, e.metadata) {
			continue
		}
		identifier := issue.Identifier
		if identifier == "" {
			identifier = e.issueID
		}
		violations = append(violations, fmt.Sprintf("%s origin=%q reason=%q timestamp=%s", identifier, e.origin, e.reason, e.at))
	}
	check.Detail = fmt.Sprintf("%d applied Todo entries in 7d checked against current issue scope", len(entries))
	if len(missing) > 0 {
		check.Status = doctorWarn
		check.Detail += "; issue scope unavailable: " + strings.Join(missing, "; ")
	}
	if len(violations) > 0 {
		check.Status = doctorFail
		check.Detail += "; unapproved feature/mechanism scope: " + strings.Join(violations, "; ")
	}
	if check.Status != doctorOK {
		check.Hint = "See docs/invariants.md INV-11; features and mechanisms require a human scope-approved Todo move."
	}
	return check
}

func doctorIssueRequiresScopeApproval(title, body string) bool {
	if doctorFeatureTitle.MatchString(title) {
		return true
	}
	// A removal or negation in an earlier coordinated clause does not govern
	// a later addition ("remove the lease and add a breaker").
	for _, clause := range doctorDeclarationBoundary.Split(title+"\n"+body, -1) {
		match := doctorMechanismDeclaration.FindStringIndex(clause)
		if match != nil && !doctorNegatedDeclaration.MatchString(clause[:match[0]]) {
			return true
		}
	}
	return false
}

func doctorTodoHumanMove(origin, reason, metadata string) bool {
	if reason == "admission_proposal_accepted" {
		return false
	}
	if origin == "human" {
		return true
	}
	// A routine/agent origin cannot borrow a human actor's account identity.
	if origin != "" && origin != "operator" {
		return false
	}
	switch reason {
	case "operator_move", "operator_kanban_move", "kanban_move", "kanban_move_field":
	default:
		return false
	}
	var evidence provenance.Metadata
	if json.Unmarshal([]byte(metadata), &evidence) != nil {
		return false
	}
	p := evidence.Provenance
	if p.Origin != provenance.OriginHuman || p.Initiator != provenance.InitiatorHuman {
		return false
	}
	// Dashboard sessions authenticate the human without recording a tracker
	// login. That basis is stronger evidence than an account name alone.
	if p.Basis == provenance.BasisAuthenticatedHuman {
		return true
	}
	return p.Actor != nil && strings.TrimSpace(p.Actor.Login) != "" && (strings.EqualFold(p.Actor.Kind, "User") || strings.EqualFold(p.Actor.Kind, "human"))
}
