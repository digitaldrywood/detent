package templates

import (
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type cardFactView struct{ Name, Text, Detail string }

func boardCardFacts(data DashboardData, card projectKanbanCard) []cardFactView {
	if !telemetry.ActiveCardLane(card.Stage, projectKanbanCardKanbanData(data, card).ActiveStates...) {
		return nil
	}
	issue := telemetry.Issue{ID: card.IssueID, Identifier: card.Identifier, ProjectID: card.ProjectID, State: card.Stage}
	for _, candidate := range telemetry.CardIssues(data.Snapshot) {
		if boardCardMatchesIssue(candidate, card) {
			issue = candidate
			break
		}
	}
	facts := telemetry.IssueCardFacts(data.Snapshot, issue)
	now := data.Snapshot.GeneratedAt
	push := cardFactView{Name: "push", Text: "no PR"}
	if facts.HasPR {
		push.Text = "push unknown"
		if facts.HeadCommittedAt != nil {
			push.Text = "push " + cardFactAge(now, *facts.HeadCommittedAt)
			push.Detail = "PR head " + facts.HeadSHA + " committed " + facts.HeadCommittedAt.UTC().Format(time.RFC3339)
		}
	}
	ci, merge := "—", "—"
	if facts.HasPR {
		ci, merge = facts.CI, facts.Mergeability
		if merge == "" {
			merge = "unknown"
		}
	}
	session := cardFactView{Name: "session", Text: "no session"}
	if facts.HasSession && facts.SessionStartedAt == nil {
		session.Text = fmt.Sprintf("session starting · %d tok", facts.SessionTokens)
	}
	if facts.SessionStartedAt != nil {
		session.Text = cardFactAge(now, *facts.SessionStartedAt) + fmt.Sprintf(" · %d tok", facts.SessionTokens)
		session.Detail = "Started " + facts.SessionStartedAt.UTC().Format(time.RFC3339) + fmt.Sprintf("; %d tokens", facts.SessionTokens)
	}
	attempts := "? today"
	if facts.AttemptsToday != nil {
		attempts = fmt.Sprintf("%d today", *facts.AttemptsToday)
	}
	session.Text += " · " + attempts
	session.Detail += "; attempts today (UTC): " + attempts
	reason := cardFactView{Name: "reason", Text: "reason unknown"}
	if facts.LaneReason != "" {
		reason.Text = facts.LaneReason
		if facts.LaneReasonAt != nil {
			reason.Detail = reason.Text + " · " + facts.LaneReasonAt.UTC().Format(time.RFC3339)
		}
		if reason.Detail == "" {
			reason.Detail = reason.Text
		}
	}
	if facts.Question != nil {
		age := "age unknown"
		if facts.Question.AskedAt != nil {
			age = cardFactAge(now, *facts.Question.AskedAt)
		}
		reason.Text = "waiting for a human reply · " + age
		reason.Detail = facts.Question.Question
	}
	return []cardFactView{push, {Name: "ci", Text: "CI " + ci}, {Name: "mergeability", Text: merge}, session, reason}
}

func cardFactAge(now, at time.Time) string {
	age := max(time.Duration(0), now.Sub(at))
	if age < time.Minute {
		return "<1m"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}

func cardFactDetail(facts []cardFactView) string {
	parts := make([]string, 0, len(facts))
	for _, fact := range facts {
		parts = append(parts, fact.Text)
		if fact.Detail != "" && fact.Detail != fact.Text {
			parts = append(parts, fact.Detail)
		}
	}
	return strings.Join(parts, "; ")
}
